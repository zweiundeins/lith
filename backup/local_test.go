package backup

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"
)

// busyReplica attaches a database, writes a row every sync interval for
// ticks intervals, and returns the most local L0 files seen at once and the
// L0 files in the replica. It restores the replica afterwards and checks
// every row arrived.
func busyReplica(t *testing.T, retention time.Duration, ticks int) (maxLocal, remote int, m *Manager) {
	t.Helper()
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(slog.DiscardHandler), LocalL0Retention: retention})
	if err != nil {
		t.Fatal(err)
	}
	const tick = 10 * time.Millisecond
	m.localL0Interval = 2 * tick
	m.tuneDB = func(db *litestream.DB) {
		db.MonitorInterval = tick
		db.Replica.SyncInterval = tick
	}
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	dir := filepath.Join(MetaPath(path), "ltx", "0")
	payload := strings.Repeat("x", 4096)
	for i := range ticks {
		mustExec(t, db, `INSERT INTO notes (body) VALUES (?)`, fmt.Sprintf("%d %s", i, payload))
		time.Sleep(tick)
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		maxLocal = max(maxLocal, len(ents))
	}
	shutdown(t, db, detach)
	remote = countLevel(t, m, "app", 0)

	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := len(bodies(t, out)); got != ticks+1 {
		t.Fatalf("restored %d rows, want %d", got, ticks+1)
	}
	return maxLocal, remote, m
}

// A busy writer once filled a disk with local L0 files: Litestream deletes
// them only after a compaction into L1, which the Manager does not run.
func TestLocalL0StaysBounded(t *testing.T) {
	t.Parallel()
	const ticks = 300
	t.Run("kept", func(t *testing.T) {
		t.Parallel()
		maxLocal, remote, _ := busyReplica(t, 1000*time.Hour, ticks)
		t.Logf("retention 1000h: %d local L0 files at most, %d in the replica", maxLocal, remote)
		if maxLocal < ticks/3 {
			t.Errorf("%d local L0 files at most; want them to pile up with a retention longer than the test", maxLocal)
		}
	})
	t.Run("trimmed", func(t *testing.T) {
		t.Parallel()
		maxLocal, remote, _ := busyReplica(t, time.Nanosecond, ticks)
		t.Logf("retention 1ns: %d local L0 files at most, %d in the replica", maxLocal, remote)
		if maxLocal > 30 {
			t.Errorf("%d local L0 files at most, want a handful", maxLocal)
		}
		if remote < ticks/3 {
			t.Errorf("%d L0 files in the replica; want every segment uploaded", remote)
		}
	})
}

func TestLocalL0Metrics(t *testing.T) {
	t.Parallel()
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(slog.DiscardHandler), LocalL0Retention: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	m.localL0Interval = 10 * time.Millisecond
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	defer shutdown(t, db, detach)
	writeAndWaitLevel0(t, m, db, "app", 3)
	writeAndWaitLevel0(t, m, db, "app", 3)
	waitFor(t, 10*time.Second, func() bool { return m.Status()[0].LocalL0Removed > 0 })
	mfs := gather(t, m.Collector("app"))
	if v := gauge(t, mfs, "app_backup_replica_local_l0_files", "app"); v < 1 {
		t.Errorf("local_l0_files = %v, want the newest at least", v)
	}
	if v := gauge(t, mfs, "app_backup_replica_local_l0_bytes", "app"); v <= 0 {
		t.Errorf("local_l0_bytes = %v, want some", v)
	}
	var removed float64
	for _, s := range mfs["app_backup_replica_local_l0_removed_total"].GetMetric() {
		removed = s.GetCounter().GetValue()
	}
	if removed < 1 {
		t.Errorf("local_l0_removed_total = %v, want at least 1", removed)
	}
}

// The trim removes a run of the oldest segments that are uploaded and old
// enough, and never the newest.
func TestTrimLocalL0Rules(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old, recent := now.Add(-time.Hour), now.Add(-time.Second)
	cases := []struct {
		name     string
		uploaded ltx.TXID
		mods     []time.Time // segment i+1 written at mods[i]
		want     []ltx.TXID  // left
	}{
		{"all uploaded and old keeps the newest", 5, []time.Time{old, old, old, old, old}, []ltx.TXID{5}},
		{"keeps what is not uploaded", 3, []time.Time{old, old, old, old, old}, []ltx.TXID{4, 5}},
		{"keeps the recent and all after", 5, []time.Time{old, old, recent, old, old}, []ltx.TXID{3, 4, 5}},
		{"keeps all before the first upload", 0, []time.Time{old, old}, []ltx.TXID{1, 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m, err := New(Config{Dir: t.TempDir(), LocalL0Retention: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			db := litestream.NewDB(filepath.Join(t.TempDir(), "app.db"))
			db.Replica = litestream.NewReplicaWithClient(db, nil)
			db.Replica.SetPos(ltx.Pos{TXID: c.uploaded})
			dir := db.LTXLevelDir(0)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for i, mod := range c.mods {
				p := db.LTXPath(0, ltx.TXID(i+1), ltx.TXID(i+1))
				if err := os.WriteFile(p, []byte("ltx"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(p, mod, mod); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "x.ltx.tmp"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			a := &attachedDB{db: db}
			removed, _, err := m.trimLocalL0(a, now)
			if err != nil {
				t.Fatal(err)
			}
			var left []ltx.TXID
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				if _, maxTXID, err := ltx.ParseFilename(e.Name()); err == nil {
					left = append(left, maxTXID)
				}
			}
			if !slices.Equal(left, c.want) || removed != len(c.mods)-len(c.want) {
				t.Errorf("left %v after removing %d, want %v", left, removed, c.want)
			}
			if files, bytes, n := a.local.stats(); files != len(c.want) || bytes != int64(3*len(c.want)) || n != removed {
				t.Errorf("stats = %d files, %d bytes, %d removed", files, bytes, n)
			}
		})
	}
}
