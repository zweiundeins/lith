package backup

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
)

// TestTheFirstPassSnapshotsAndPrunesNothing: until a snapshot is older than
// the retention window there is no floor, so existing history stays.
func TestTheFirstPassSnapshotsAndPrunesNothing(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	defer shutdown(t, db, detach)
	writeAndWaitLevel0(t, m, db, "app", 3)
	segments := countLevel(t, m, "app", 0)

	if errs := m.MaintainAll(bg, time.Now()); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	if n := countLevel(t, m, "app", litestream.SnapshotLevel); n != 1 {
		t.Errorf("snapshots = %d, want the first one", n)
	}
	if n := countLevel(t, m, "app", 0); n != segments {
		t.Errorf("WAL segments = %d, want all %d kept", n, segments)
	}
	if errs := m.MaintainAll(bg, time.Now().Add(time.Hour)); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	if n := countLevel(t, m, "app", litestream.SnapshotLevel); n != 1 {
		t.Errorf("snapshots = %d after a pass an hour later, want no new one before the interval", n)
	}
}

// TestAgedHistoryIsPrunedWithoutLosingData drives the clock past the window.
func TestAgedHistoryIsPrunedWithoutLosingData(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	defer shutdown(t, db, detach)

	writeAndWaitLevel0(t, m, db, "app", 3)
	if errs := m.MaintainAll(bg, time.Now()); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	writeAndWaitLevel0(t, m, db, "app", 3)
	peak := countLevel(t, m, "app", 0)
	var want int
	if err := db.QueryRow(`SELECT count(*) FROM notes`).Scan(&want); err != nil {
		t.Fatal(err)
	}

	if errs := m.MaintainAll(bg, time.Now().Add(2*DefaultRetentionWindow)); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	if n := countLevel(t, m, "app", 0); n >= peak {
		t.Errorf("WAL segments = %d, want fewer than %d", n, peak)
	}
	if n := countLevel(t, m, "app", litestream.SnapshotLevel); n != 1 {
		t.Errorf("snapshots = %d, want only the newest", n)
	}

	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatal(err)
	}
	if got := len(bodies(t, out)); got != want {
		t.Errorf("restored rows = %d, want %d", got, want)
	}

	// The window starts at the snapshot that is left, not at the older WAL
	// segment litestream keeps below it.
	earliest, _, err := m.RestoreWindow(bg, "app")
	if err != nil {
		t.Fatal(err)
	}
	if snaps := snapshotTimes(t, m, "app"); len(snaps) != 1 || !earliest.Equal(snaps[0]) {
		t.Errorf("RestoreWindow starts at %v, want the snapshot's time %v", earliest, snaps)
	}
	err = m.RestoreAt(bg, "app", filepath.Join(t.TempDir(), "before.db"), earliest.Add(-time.Millisecond))
	if !errors.Is(err, ErrNothingToRestore) {
		t.Errorf("RestoreAt just before the window = %v, want ErrNothingToRestore", err)
	}
	if err := m.RestoreAt(bg, "app", filepath.Join(t.TempDir(), "after.db"), earliest.Add(time.Millisecond)); err != nil {
		t.Errorf("RestoreAt just inside the window: %v", err)
	}
}

func TestMaintenanceSkipsDetachedReplicas(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	shutdown(t, db, detach)
	if errs := m.MaintainAll(bg, time.Now()); len(errs) != 0 {
		t.Errorf("MaintainAll = %v, want nothing for a detached replica", errs)
	}
}

func TestMaintenanceRecordsTheReplicaSize(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	defer shutdown(t, db, detach)
	if st := m.Status(); !st[0].LastSnapshotAt.IsZero() {
		t.Fatal("a snapshot time before the first pass")
	}
	if errs := m.MaintainAll(bg, time.Now()); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	st := m.Status()[0]
	if st.LastSnapshotAt.IsZero() || st.ReplicaObjects < 2 || st.ReplicaBytes <= 0 {
		t.Errorf("Status = %+v, want the snapshot and the replica's size", st)
	}
}

func TestNewestAtOrBefore(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	snaps := []snapMeta{{createdAt: base}, {createdAt: base.Add(time.Hour)}, {createdAt: base.Add(2 * time.Hour)}}

	if got, ok := newestAtOrBefore(snaps, base.Add(90*time.Minute)); !ok || !got.createdAt.Equal(base.Add(time.Hour)) {
		t.Errorf("floor = %v, %v; want the snapshot an hour in", got.createdAt, ok)
	}
	if _, ok := newestAtOrBefore(snaps, base.Add(-time.Hour)); ok {
		t.Error("a floor before every snapshot")
	}
	if _, ok := newestAtOrBefore(nil, base); ok {
		t.Error("a floor without snapshots")
	}
}

// levels records the level of each log record.
type levels struct{ got []slog.Level }

func (l *levels) Enabled(context.Context, slog.Level) bool { return true }
func (l *levels) Handle(_ context.Context, r slog.Record) error {
	l.got = append(l.got, r.Level)
	return nil
}
func (l *levels) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *levels) WithGroup(string) slog.Handler      { return l }

// TestMaintenanceFailuresWarnUntilTheThirdPassInARow: the next pass retries
// a failure, so only a replica that keeps failing is an error.
func TestMaintenanceFailuresWarnUntilTheThirdPassInARow(t *testing.T) {
	h := &levels{}
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(h)})
	if err != nil {
		t.Fatal(err)
	}
	fail := errors.New("503 SlowDown")
	failing := map[string]int{}
	for _, pass := range []map[string]error{
		{"a": fail, "b": nil},
		{"a": fail, "b": fail},
		{"a": fail, "b": nil},
		{"a": fail, "b": fail},
	} {
		failing = m.logMaintenance(pass, failing)
	}
	want := []slog.Level{slog.LevelWarn, slog.LevelWarn, slog.LevelWarn, slog.LevelError, slog.LevelError, slog.LevelWarn}
	if !slices.Equal(h.got, want) {
		t.Errorf("levels = %v, want %v", h.got, want)
	}
	if failing["a"] != 4 || failing["b"] != 1 {
		t.Errorf("failing = %v, want a:4 b:1", failing)
	}
}
