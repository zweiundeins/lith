package backup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"
)

// hooks lets a test stall or fail a replica's uploads and listings.
type hooks struct {
	write    atomic.Pointer[func(level int) error]
	snapshot atomic.Pointer[func(ctx context.Context) error] // snapshot uploads only
	list     atomic.Pointer[func(level int)]
}

type hookedClient struct {
	litestream.ReplicaClient
	h *hooks
}

func (c *hookedClient) WriteLTXFile(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, r io.Reader) (*ltx.FileInfo, error) {
	if f := c.h.write.Load(); f != nil {
		if err := (*f)(level); err != nil {
			return nil, err
		}
	}
	if f := c.h.snapshot.Load(); f != nil && level == litestream.SnapshotLevel {
		if err := (*f)(ctx); err != nil {
			return nil, err
		}
	}
	return c.ReplicaClient.WriteLTXFile(ctx, level, minTXID, maxTXID, r)
}

func (c *hookedClient) LTXFiles(ctx context.Context, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error) {
	if f := c.h.list.Load(); f != nil {
		(*f)(level)
	}
	return c.ReplicaClient.LTXFiles(ctx, level, seek, useMetadata)
}

func hookedManager(t *testing.T) (*Manager, *hooks) {
	t.Helper()
	m := newManager(t)
	h := &hooks{}
	base := m.newClient
	m.newClient = func(name string) litestream.ReplicaClient { return &hookedClient{base(name), h} }
	return m, h
}

// stall returns a hook that blocks the first call it sees until release is
// called; entered is closed once that call is waiting.
func stall() (hook func(), entered <-chan struct{}, release func()) {
	in := make(chan struct{})
	out := make(chan struct{})
	var once sync.Once
	return func() {
			first := false
			once.Do(func() { first = true; close(in) })
			if first {
				<-out
			}
		}, in, func() {
			close(out)
		}
}

func TestDetachHoldsTheNameUntilTheFinalSync(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	defer closeApp(t, db)

	block, entered, release := stall()
	write := func(int) error { block(); return nil }
	h.write.Store(&write)
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('second')`)

	detached := make(chan error, 1)
	go func() { detached <- detach(bg) }()
	<-entered
	waitFor(t, 5*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.attached["app"] == nil
	})
	if _, err := m.Attach(bg, "app", path); err == nil || !strings.Contains(err.Error(), "already attached") {
		t.Errorf("Attach during the final sync = %v, want a refusal", err)
	}
	if err := m.PurgeReplica(bg, "app"); err == nil {
		t.Error("PurgeReplica during the final sync succeeded")
	}
	release()
	if err := <-detached; err != nil {
		t.Fatalf("detach: %v", err)
	}
	h.write.Store(nil)

	again, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatalf("Attach after the final sync: %v", err)
	}
	if err := again(bg); err != nil {
		t.Error(err)
	}
}

// TestDetachSyncsAfterWaitingForMaintenance: a deadline that passes while
// detach waits for a maintenance pass must not cost the final sync.
func TestDetachSyncsAfterWaitingForMaintenance(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	defer closeApp(t, db)

	block, entered, release := stall()
	list := func(level int) {
		if level == litestream.SnapshotLevel {
			block()
		}
	}
	h.list.Store(&list)
	maintained := make(chan map[string]error, 1)
	go func() { maintained <- m.MaintainAll(bg, time.Now()) }()
	<-entered

	mustExec(t, db, `INSERT INTO notes (body) VALUES ('during maintenance')`)
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	detached := make(chan error, 1)
	go func() { detached <- detach(ctx) }()
	time.Sleep(150 * time.Millisecond)
	h.list.Store(nil)
	release()
	if errs := <-maintained; errs["app"] != nil {
		t.Errorf("MaintainAll: %v", errs["app"])
	}
	if err := <-detached; err != nil {
		t.Fatalf("detach whose deadline passed while it waited: %v", err)
	}

	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, out); len(got) != 2 {
		t.Errorf("restored rows = %v, want the row written before detach", got)
	}
}

func TestAFailedFirstSyncFailsFast(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	down := errors.New("replica unreachable")
	fail := func(int) error { return down }
	h.write.Store(&fail)

	path := filepath.Join(t.TempDir(), "app.db")
	db := openApp(t, m, path)
	mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
	start := time.Now()
	if _, err := m.Attach(bg, "app", path); err == nil || !strings.Contains(err.Error(), down.Error()) {
		t.Fatalf("Attach against a failing replica = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Attach took %v to fail", d)
	}

	h.write.Store(nil)
	detach, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatalf("Attach once the replica is back: %v", err)
	}
	shutdown(t, db, detach)
}

func snapshotTimes(t *testing.T, m *Manager, name string) []time.Time {
	t.Helper()
	snaps, err := listSnapshots(bg, m.newClient(name))
	if err != nil {
		t.Fatal(err)
	}
	var out []time.Time
	for _, s := range snaps {
		out = append(out, s.createdAt)
	}
	return out
}

// TestAnIdleReplicaKeepsItsSnapshot: a snapshot at an unchanged position
// would replace the one there and move its time forward.
func TestAnIdleReplicaKeepsItsSnapshot(t *testing.T) {
	t.Parallel()
	m, err := New(Config{
		Dir:              t.TempDir(),
		RetentionWindow:  24 * time.Hour,
		SnapshotInterval: 24 * time.Hour,
		Logger:           slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	defer shutdown(t, db, detach)

	now := time.Now()
	if errs := m.MaintainAll(bg, now); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	first := snapshotTimes(t, m, "app")
	time.Sleep(20 * time.Millisecond)
	if errs := m.MaintainAll(bg, now.Add(25*time.Hour)); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	second := snapshotTimes(t, m, "app")
	if len(first) != 1 || len(second) != 1 || !second[0].Equal(first[0]) {
		t.Errorf("snapshots %v, then %v; want the one snapshot kept as it was", first, second)
	}
}
