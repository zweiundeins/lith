package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/benbjohnson/litestream/file"
	"github.com/benbjohnson/litestream/s3"
)

func detachedTotals(m *Manager) maintTotals {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.detachedMaint
}

// clientInterfaces are the optional interfaces litestream asserts on a
// ReplicaClient (`rg '\.\(' --glob '!*_test.go'` in its module). Add any
// that a litestream update brings, and forward it in countDeletes.
var clientInterfaces = []reflect.Type{
	reflect.TypeFor[litestream.ReplicaClientV3](),
}

// TestCountDeletesKeepsTheClientsInterfaces: the wrapper on an attached
// replica's client must not hide what a restore looks for.
func TestCountDeletesKeepsTheClientsInterfaces(t *testing.T) {
	clients := []litestream.ReplicaClient{s3.NewReplicaClient(), file.NewReplicaClient(t.TempDir())}
	if _, ok := clients[0].(litestream.ReplicaClientV3); !ok {
		t.Fatal("the S3 client reads no v0.3 backups: the check below proves nothing")
	}
	for _, c := range clients {
		wrapped, _ := countDeletes(c)
		for _, i := range clientInterfaces {
			if reflect.TypeOf(c).Implements(i) && !reflect.TypeOf(wrapped).Implements(i) {
				t.Errorf("%T wrapped hides %v", c, i)
			}
		}
	}
}

// TestDetachSnapshotsWhenDue: a database attached only between passes still
// gets a snapshot, and no second one before the interval.
func TestDetachSnapshotsWhenDue(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	writeAndWaitLevel0(t, m, db, "app", 2)
	if n := countLevel(t, m, "app", litestream.SnapshotLevel); n != 0 {
		t.Fatalf("snapshots = %d before detach, want none", n)
	}
	shutdown(t, db, detach)
	first := snapshotTimes(t, m, "app")
	if len(first) != 1 {
		t.Fatalf("snapshots = %d after detach, want 1", len(first))
	}

	db = openApp(t, m, path)
	again, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('second visit')`)
	shutdown(t, db, again)
	if got := snapshotTimes(t, m, "app"); len(got) != 1 || !got[0].Equal(first[0]) {
		t.Errorf("snapshots = %v after a second detach, want only %v", got, first)
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, out); len(got) != 4 {
		t.Errorf("restored rows = %v, want 4", got)
	}
}

// TestMaintenancePrunesANamedDetachedReplica: a name that is not attached
// loses its history older than the window and gets no snapshot.
func TestMaintenancePrunesANamedDetachedReplica(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	writeAndWaitLevel0(t, m, db, "app", 3)
	if errs := m.MaintainAll(bg, time.Now()); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	writeAndWaitLevel0(t, m, db, "app", 3)
	if errs := m.MaintainAll(bg, time.Now().Add(25*time.Hour)); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	writeAndWaitLevel0(t, m, db, "app", 1)
	var want int
	if err := db.QueryRow(`SELECT count(*) FROM notes`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	shutdown(t, db, detach)

	snaps := snapshotTimes(t, m, "app")
	segments := countLevel(t, m, "app", 0)
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2 before the detached pass", len(snaps))
	}
	errs := m.maintainPass(bg, time.Now().Add(2*DefaultRetentionWindow), []string{"app"}, 1, 0)
	if err, ok := errs["app"]; !ok || err != nil {
		t.Fatalf("maintainPass = %v, want app maintained", errs)
	}
	after := snapshotTimes(t, m, "app")
	if len(after) != 1 || !after[0].Equal(snaps[1]) {
		t.Errorf("snapshots = %v, want only the newest of %v and no new one", after, snaps)
	}
	left := countLevel(t, m, "app", 0)
	if left >= segments {
		t.Errorf("WAL segments = %d, want fewer than %d", left, segments)
	}
	if got := detachedTotals(m); got.passes != 1 || got.pruned != 1+segments-left {
		t.Errorf("detached totals = %+v, want 1 pass and %d pruned", got, 1+segments-left)
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatal(err)
	}
	if got := len(bodies(t, out)); got != want {
		t.Errorf("restored rows = %d, want %d", got, want)
	}
}

// TestMaintenanceTakesANameOnce: a named replica that is attached is
// maintained once, as an attached one, and a name with nothing replicated
// is no failure.
func TestMaintenanceTakesANameOnce(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	defer shutdown(t, db, detach)

	errs := m.maintainPass(bg, time.Now(), []string{"other", "app", "app"}, 2, 0)
	if got := slices.Sorted(maps.Keys(errs)); !slices.Equal(got, []string{"app", "other"}) || errs["app"] != nil || errs["other"] != nil {
		t.Fatalf("maintainPass = %v, want app and other without errors", errs)
	}
	if n := countLevel(t, m, "app", litestream.SnapshotLevel); n != 1 {
		t.Errorf("snapshots = %d, want 1", n)
	}
	if st := m.Status(); st[0].MaintenancePasses != 1 {
		t.Errorf("attached passes = %d, want 1", st[0].MaintenancePasses)
	}
	if got := detachedTotals(m); got.passes != 1 || got.pruned != 0 {
		t.Errorf("detached totals = %+v, want other's pass", got)
	}

	mfs := gather(t, m.Collector("app"))
	if v := counter(t, mfs, "app_backup_replica_maintenance_passes_total", "app"); v != 1 {
		t.Errorf("replica passes = %v, want 1", v)
	}
	if v := counter(t, mfs, "app_backup_replica_pruned_objects_total", "app"); v != 0 {
		t.Errorf("replica pruned = %v, want 0", v)
	}
	if v := mfs["app_backup_detached_maintenance_passes_total"].GetMetric()[0].GetCounter().GetValue(); v != 1 {
		t.Errorf("detached passes = %v, want 1", v)
	}
}

// TestMaintenanceSkipsANamePurgedDuringThePass: a purge while the pass runs
// leaves the name without a result, whether it came before the name's turn
// or during it.
func TestMaintenanceSkipsANamePurgedDuringThePass(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	for _, name := range []string{"a", "b"} {
		db, detach := replicated(t, m, name, filepath.Join(t.TempDir(), name+".db"))
		shutdown(t, db, detach)
	}
	var once sync.Once
	list := func(int) {
		once.Do(func() {
			for _, name := range []string{"a", "b"} {
				if err := m.PurgeReplica(bg, name); err != nil {
					t.Error(err)
				}
			}
		})
	}
	h.list.Store(&list)

	errs := m.maintainPass(bg, time.Now(), []string{"a", "b", "c"}, 1, 0)
	if got := slices.Sorted(maps.Keys(errs)); !slices.Equal(got, []string{"c"}) {
		t.Errorf("maintainPass = %v, want only c", errs)
	}
	if got := detachedTotals(m); got.passes != 1 {
		t.Errorf("detached passes = %d, want c's alone", got.passes)
	}
}

// messages records the level of each record whose message has prefix.
type messages struct {
	prefix string
	mu     sync.Mutex
	got    []slog.Level
}

func (l *messages) Enabled(context.Context, slog.Level) bool { return true }
func (l *messages) Handle(_ context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, l.prefix) {
		l.mu.Lock()
		l.got = append(l.got, r.Level)
		l.mu.Unlock()
	}
	return nil
}
func (l *messages) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *messages) WithGroup(string) slog.Handler      { return l }

// TestAFailedNamesStillMaintainsTheAttached: without the list, a pass still
// maintains the attached replicas, and a list that keeps failing is an error
// from its third pass in a row.
func TestAFailedNamesStillMaintainsTheAttached(t *testing.T) {
	t.Parallel()
	h := &messages{prefix: "backup: listing"}
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(h)})
	if err != nil {
		t.Fatal(err)
	}
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	defer shutdown(t, db, detach)

	fail := true
	opt := MaintenanceOptions{Pause: -1, Names: func(context.Context) ([]string, error) {
		if fail {
			return nil, errors.New("accounts: database is locked")
		}
		return []string{"app"}, nil
	}}
	var l maintLoop
	for range 3 {
		l.pass(bg, m, time.Now(), opt)
	}
	fail = false
	l.pass(bg, m, time.Now(), opt)
	fail = true
	l.pass(bg, m, time.Now(), opt)

	if st := m.Status(); st[0].MaintenancePasses != 5 || st[0].LastSnapshotAt.IsZero() {
		t.Errorf("Status = %+v, want 5 passes and a snapshot", st[0])
	}
	want := []slog.Level{slog.LevelWarn, slog.LevelWarn, slog.LevelError, slog.LevelWarn}
	if !slices.Equal(h.got, want) {
		t.Errorf("levels = %v, want %v", h.got, want)
	}
}

// TestMaintenanceHoldsItsConcurrency: a pass over many names lists no more
// replicas at once than its workers.
func TestMaintenanceHoldsItsConcurrency(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	var names []string
	for i := range 6 {
		name := fmt.Sprintf("t%d", i)
		db, detach := replicated(t, m, name, filepath.Join(t.TempDir(), name+".db"))
		shutdown(t, db, detach)
		names = append(names, name)
	}
	var now, peak atomic.Int32
	list := func(int) {
		n := now.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		now.Add(-1)
	}
	h.list.Store(&list)

	if errs := m.maintainPass(bg, time.Now(), names, 2, 0); len(errs) != len(names) {
		t.Fatalf("maintainPass = %v, want every name", errs)
	}
	if p := peak.Load(); p != 2 {
		t.Errorf("listings at once = %d, want 2", p)
	}
}

// TestMaintenanceLoopSchedule: a pass every interval, an hour by default,
// each reading the names afresh, until ctx is done.
func TestMaintenanceLoopSchedule(t *testing.T) {
	m := newManager(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		t0 := time.Now()
		var calls []time.Duration
		done := make(chan struct{})
		go func() {
			m.RunMaintenanceLoop(ctx, MaintenanceOptions{Names: func(context.Context) ([]string, error) {
				calls = append(calls, time.Since(t0))
				return nil, nil
			}})
			close(done)
		}()
		time.Sleep(3*time.Hour + time.Minute)
		cancel()
		synctest.Wait()
		if want := []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}; !slices.Equal(calls, want) {
			t.Errorf("passes at %v, want %v", calls, want)
		}
		select {
		case <-done:
		default:
			t.Error("the loop did not stop")
		}
	})
}

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
