package backup

import (
	"context"
	"database/sql"
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
	"github.com/superfly/ltx"
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

// TestAttachSnapshotsWhenDue: a database attached only between passes still
// gets a snapshot, after the first sync, and no second one before the
// interval.
func TestAttachSnapshotsWhenDue(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db := openApp(t, m, path)
	mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
	detach, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return countLevel(t, m, "app", litestream.SnapshotLevel) == 1 })
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('first visit')`)
	shutdown(t, db, detach)
	first := snapshotTimes(t, m, "app")

	db = openApp(t, m, path)
	again, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatal(err)
	}
	waitForAttachSnapshot(m, "app")
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('second visit')`)
	shutdown(t, db, again)
	if got := snapshotTimes(t, m, "app"); !slices.Equal(got, first) {
		t.Errorf("snapshots = %v after a second attach, want only %v", got, first)
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, out); len(got) != 2 {
		t.Errorf("restored rows = %v, want 2", got)
	}
}

// TestDetachCancelsASlowSnapshot: a shutdown that detaches many databases
// does not wait for their snapshots, and every final sync still happens.
func TestDetachCancelsASlowSnapshot(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	var entered sync.WaitGroup
	entered.Add(4)
	snapshot := func(ctx context.Context) error {
		entered.Done()
		<-ctx.Done()
		return ctx.Err()
	}
	h.snapshot.Store(&snapshot)

	type tenant struct {
		db     *sql.DB
		detach func(context.Context) error
		path   string
	}
	var tenants []tenant
	for i := range 4 {
		name := fmt.Sprintf("t%d", i)
		path := filepath.Join(t.TempDir(), name+".db")
		db := openApp(t, m, path)
		mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
		detach, err := m.Attach(bg, name, path)
		if err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, tenant{db, detach, path})
	}
	entered.Wait()
	start := time.Now()
	for _, tn := range tenants {
		mustExec(t, tn.db, `INSERT INTO notes (body) VALUES ('last word')`)
		shutdown(t, tn.db, tn.detach)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("detaching took %v, want no wait for the snapshots", d)
	}
	h.snapshot.Store(nil)
	for i := range tenants {
		name := fmt.Sprintf("t%d", i)
		if n := countLevel(t, m, name, litestream.SnapshotLevel); n != 0 {
			t.Errorf("%s: snapshots = %d, want the cancelled one gone", name, n)
		}
		out := filepath.Join(t.TempDir(), name+"-restored.db")
		if err := m.Restore(bg, name, out); err != nil {
			t.Fatal(err)
		}
		if got := bodies(t, out); len(got) != 1 {
			t.Errorf("%s: restored rows = %v, want the last word", name, got)
		}
	}
}

// TestDetachCutsOffTheHourlyPass: a detach while the attached pass uploads
// a snapshot cancels it instead of waiting, and the final sync lands.
func TestDetachCutsOffTheHourlyPass(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	writeAndWaitLevel0(t, m, db, "app", 1)
	entered := make(chan struct{})
	snapshot := func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	h.snapshot.Store(&snapshot)
	passed := make(chan map[string]error, 1)
	go func() { passed <- m.MaintainAll(bg, time.Now().Add(25*time.Hour)) }()
	<-entered

	start := time.Now()
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('last word')`)
	shutdown(t, db, detach)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("detach took %v, want no wait for the pass's upload", d)
	}
	if errs := <-passed; len(errs) != 0 {
		t.Errorf("MaintainAll = %v, want the cut-off replica left out", errs)
	}
	h.snapshot.Store(nil)
	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, out); len(got) != 3 {
		t.Errorf("restored rows = %v, want the last word", got)
	}
}

// TestThePassSkipsABusyReplica: while Attach's snapshot uploads, the pass
// moves on instead of holding a worker.
func TestThePassSkipsABusyReplica(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	entered := make(chan struct{})
	snapshot := func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	h.snapshot.Store(&snapshot)
	path := filepath.Join(t.TempDir(), "app.db")
	db := openApp(t, m, path)
	mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
	detach, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	start := time.Now()
	if errs := m.MaintainAll(bg, time.Now()); len(errs) != 0 {
		t.Errorf("MaintainAll = %v, want the busy replica skipped", errs)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the pass took %v, want no wait for the upload", d)
	}
	if st := m.Status(); len(st) != 1 || st[0].MaintenanceSkipped != 1 {
		t.Errorf("Status = %+v, want one skipped pass", st)
	}
	shutdown(t, db, detach)
}

// TestAFailedAttachSnapshotIsLogged: a snapshot that fails is a warning, and
// detach goes on as usual.
func TestAFailedAttachSnapshotIsLogged(t *testing.T) {
	t.Parallel()
	msgs := &messages{prefix: "backup: snapshot after attach"}
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(msgs)})
	if err != nil {
		t.Fatal(err)
	}
	h := &hooks{}
	base := m.newClient
	m.newClient = func(name string) litestream.ReplicaClient { return &hookedClient{base(name), h} }
	snapshot := func(context.Context) error { return errors.New("503 SlowDown") }
	h.snapshot.Store(&snapshot)

	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	waitFor(t, 5*time.Second, func() bool { return len(msgs.levels()) > 0 })
	if got := msgs.levels(); !slices.Equal(got, []slog.Level{slog.LevelWarn}) {
		t.Errorf("levels = %v, want one warning", got)
	}
	shutdown(t, db, detach)
	if err := m.VerifyRestore(bg, "app"); err != nil {
		t.Errorf("VerifyRestore after detach: %v", err)
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
	errs := m.maintainNames(bg, time.Now().Add(2*DefaultRetentionWindow), []string{"app"}, 1, 0)
	if err, ok := errs["app"]; !ok || err != nil {
		t.Fatalf("maintainNames = %v, want app maintained", errs)
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

// TestDetachedRestoreAtTheEdgeOfTheWindow: with the floor before the
// cutoff and a snapshot inside the window, a restore just after the cutoff
// still lands where it should.
func TestDetachedRestoreAtTheEdgeOfTheWindow(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	atFloor := 1 // the row Attach's snapshot holds
	time.Sleep(100 * time.Millisecond)
	writeAndWaitLevel0(t, m, db, "app", 3)
	if errs := m.MaintainAll(bg, time.Now().Add(25*time.Hour)); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	writeAndWaitLevel0(t, m, db, "app", 1)
	shutdown(t, db, detach)

	snaps := snapshotTimes(t, m, "app")
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(snaps))
	}
	cutoff := snaps[0].Add(20 * time.Millisecond)
	if errs := m.maintainNames(bg, cutoff.Add(DefaultRetentionWindow), []string{"app"}, 1, 0); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	if got := snapshotTimes(t, m, "app"); !slices.Equal(got, snaps) {
		t.Errorf("snapshots = %v, want both kept: the floor and the one inside the window", got)
	}
	out := filepath.Join(t.TempDir(), "edge.db")
	if err := m.RestoreAt(bg, "app", out, cutoff.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if got := len(bodies(t, out)); got != atFloor {
		t.Errorf("rows just inside the window = %d, want %d", got, atFloor)
	}
}

// TestAnAttachDuringTheDetachedPrune: an Attach that starts while the name
// is pruned through the bucket loses nothing, and the attached pass leaves
// the name to the prune meanwhile.
func TestAnAttachDuringTheDetachedPrune(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	writeAndWaitLevel0(t, m, db, "app", 3)
	if errs := m.MaintainAll(bg, time.Now()); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	writeAndWaitLevel0(t, m, db, "app", 3)
	if errs := m.MaintainAll(bg, time.Now().Add(25*time.Hour)); errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	writeAndWaitLevel0(t, m, db, "app", 1)
	shutdown(t, db, detach)

	var started atomic.Bool
	var again func(context.Context) error
	var skipped map[string]error
	list := func(int) {
		if !started.CompareAndSwap(false, true) {
			return
		}
		db = openApp(t, m, path)
		var err error
		if again, err = m.Attach(bg, "app", path); err != nil {
			t.Error(err)
			return
		}
		skipped = m.MaintainAll(bg, time.Now())
	}
	h.list.Store(&list)
	if errs := m.maintainNames(bg, time.Now().Add(2*DefaultRetentionWindow), []string{"app"}, 1, 0); errs["app"] != nil {
		t.Fatalf("maintainNames = %v", errs)
	}
	h.list.Store(nil)
	if again == nil {
		t.Fatal("no attach during the prune")
	}
	if len(skipped) != 0 {
		t.Errorf("MaintainAll during the prune = %v, want the name left to it", skipped)
	}
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('after the prune')`)
	var want int
	if err := db.QueryRow(`SELECT count(*) FROM notes`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	shutdown(t, db, again)
	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatal(err)
	}
	if got := len(bodies(t, out)); got != want {
		t.Errorf("restored rows = %d, want %d", got, want)
	}
}

// TestNamesSkipTheAttached: the loop over the names leaves an attached one
// to the attached passes, takes a name once, and a name with nothing
// replicated is no failure.
func TestNamesSkipTheAttached(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	defer shutdown(t, db, detach)

	errs := m.maintainNames(bg, time.Now(), []string{"other", "app", "other", "../bad"}, 2, 0)
	if got := slices.Sorted(maps.Keys(errs)); !slices.Equal(got, []string{"other"}) || errs["other"] != nil {
		t.Fatalf("maintainNames = %v, want other alone without an error", errs)
	}
	if st := m.Status(); st[0].MaintenancePasses != 0 {
		t.Errorf("attached passes = %d, want none from the names", st[0].MaintenancePasses)
	}
	if got := detachedTotals(m); got.passes != 1 || got.pruned != 0 {
		t.Errorf("detached totals = %+v, want other's pass", got)
	}
	if errs := m.MaintainAll(bg, time.Now()); errs["app"] != nil {
		t.Fatal(errs["app"])
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

	errs := m.maintainNames(bg, time.Now(), []string{"a", "b", "c"}, 1, 0)
	if got := slices.Sorted(maps.Keys(errs)); !slices.Equal(got, []string{"c"}) {
		t.Errorf("maintainNames = %v, want only c", errs)
	}
	if got := detachedTotals(m); got.passes != 1 {
		t.Errorf("detached passes = %d, want c's alone", got.passes)
	}
}

// messages records the records whose message has prefix.
type messages struct {
	prefix string
	mu     sync.Mutex
	got    []slog.Record
}

func (l *messages) Enabled(context.Context, slog.Level) bool { return true }
func (l *messages) Handle(_ context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, l.prefix) {
		l.mu.Lock()
		l.got = append(l.got, r.Clone())
		l.mu.Unlock()
	}
	return nil
}
func (l *messages) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *messages) WithGroup(string) slog.Handler      { return l }

func (l *messages) levels() []slog.Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Level
	for _, r := range l.got {
		out = append(out, r.Level)
	}
	return out
}

// failingClient fails every listing.
type failingClient struct{ litestream.ReplicaClient }

func (failingClient) LTXFiles(context.Context, int, ltx.TXID, bool) (ltx.FileIterator, error) {
	return nil, errors.New("503 Please reduce your request rate")
}

func lastDetachedPass(t *testing.T, m *Manager) float64 {
	t.Helper()
	return gauge(t, gather(t, m.Collector("app")), "app_backup_detached_last_pass_timestamp_seconds", "")
}

// TestAFailedNamesShowsInTheLogAndTheMetrics: a list that keeps failing is
// an error from its third pass in a row, and the last pass's time stays
// where it was.
func TestAFailedNamesShowsInTheLogAndTheMetrics(t *testing.T) {
	t.Parallel()
	h := &messages{prefix: "backup: listing"}
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(h)})
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	opt := MaintenanceOptions{DetachedPause: -1, Names: func(context.Context) ([]string, error) {
		if fail {
			return nil, errors.New("accounts: database is locked")
		}
		return []string{"t1"}, nil
	}}
	l := detachedLoop{rejected: map[string]bool{}}
	for range 3 {
		l.pass(bg, m, time.Now(), opt)
	}
	if v := lastDetachedPass(t, m); v != 0 {
		t.Errorf("last pass = %v while Names fails, want 0", v)
	}
	fail = false
	l.pass(bg, m, time.Now(), opt)
	if v := lastDetachedPass(t, m); v <= 0 {
		t.Errorf("last pass = %v after a pass, want its time", v)
	}
	fail = true
	l.pass(bg, m, time.Now(), opt)

	want := []slog.Level{slog.LevelWarn, slog.LevelWarn, slog.LevelError, slog.LevelWarn}
	if got := h.levels(); !slices.Equal(got, want) {
		t.Errorf("levels = %v, want %v", got, want)
	}
}

// TestDetachedFailuresAreOneLinePerPass: the names that fail in a pass are
// one summary with the first five, a warning until the third failed pass in
// a row; a name checkName rejects is a warning once.
func TestDetachedFailuresAreOneLinePerPass(t *testing.T) {
	t.Parallel()
	h := &messages{prefix: "backup: maintenance"}
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(h)})
	if err != nil {
		t.Fatal(err)
	}
	m.newClient = func(string) litestream.ReplicaClient { return failingClient{} }
	names := []string{"a/b"}
	for i := range 7 {
		names = append(names, fmt.Sprintf("t%d", i))
	}
	opt := MaintenanceOptions{DetachedConcurrency: 3, DetachedPause: -1, Names: func(context.Context) ([]string, error) {
		return names, nil
	}}
	l := detachedLoop{rejected: map[string]bool{}}
	for range 3 {
		l.pass(bg, m, time.Now(), opt)
	}

	want := []slog.Level{slog.LevelWarn, slog.LevelWarn, slog.LevelWarn, slog.LevelError}
	if got := h.levels(); !slices.Equal(got, want) {
		t.Fatalf("levels = %v, want the rejected name once, then a line per pass: %v", got, want)
	}
	h.got[3].Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "failed":
			if a.Value.Int64() != 7 {
				t.Errorf("failed = %v, want 7", a.Value)
			}
		case "replicas":
			if got := a.Value.Any().([]string); !slices.Equal(got, names[1:6]) {
				t.Errorf("replicas = %v, want the first five", got)
			}
		}
		return true
	})
	mfs := gather(t, m.Collector("app"))
	if v := mfs["app_backup_detached_maintenance_failures_total"].GetMetric()[0].GetCounter().GetValue(); v != 21 {
		t.Errorf("failures = %v, want 21", v)
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

	if errs := m.maintainNames(bg, time.Now(), names, 2, 0); len(errs) != len(names) {
		t.Fatalf("maintainNames = %v, want every name", errs)
	}
	if p := peak.Load(); p != 2 {
		t.Errorf("listings at once = %d, want 2", p)
	}
}

// TestAttachedLoopSchedule: a pass every interval, an hour by default, and
// a cancel during a pass waits for it to end.
func TestAttachedLoopSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		t0 := time.Now()
		var calls []time.Duration
		var returned time.Duration
		go func() {
			runEvery(ctx, DefaultMaintenanceInterval, func(time.Time) {
				calls = append(calls, time.Since(t0))
				if len(calls) == 3 {
					cancel()
					time.Sleep(30 * time.Minute)
				}
			})
			returned = time.Since(t0)
		}()
		time.Sleep(5 * time.Hour)
		synctest.Wait()
		if want := []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}; !slices.Equal(calls, want) {
			t.Errorf("passes at %v, want %v", calls, want)
		}
		if want := 3*time.Hour + 30*time.Minute; returned != want {
			t.Errorf("the loop returned at %v, want %v, when its pass ended", returned, want)
		}
	})
}

// TestDetachedLoopWaitsForItsPass: a cancel while Names runs ends the loop
// once Names returns, not before.
func TestDetachedLoopWaitsForItsPass(t *testing.T) {
	m := newManager(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		t0 := time.Now()
		var returned time.Duration
		go func() {
			m.RunMaintenanceLoop(ctx, MaintenanceOptions{Names: func(context.Context) ([]string, error) {
				cancel()
				time.Sleep(time.Hour)
				return []string{"t1"}, nil
			}})
			returned = time.Since(t0)
		}()
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		if want := 70 * time.Minute; returned != want {
			t.Errorf("the loop returned at %v, want %v, when Names returned", returned, want)
		}
		if got := detachedTotals(m); got.passes != 0 {
			t.Errorf("detached passes = %d after the cancel, want none", got.passes)
		}
	})
}

// TestPassPausesOnlyAfterTheNamesItMaintained: skipped names cost no pause,
// and a cancel during a pause ends the pass at once, after the replica in
// progress on another worker is done.
func TestPassPausesOnlyAfterTheNamesItMaintained(t *testing.T) {
	m := newManager(t)
	t.Run("skips", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			t0 := time.Now()
			out := m.maintainPass(t.Context(), []string{"skip1", "a", "skip2", "skip3", "b", "skip4"}, 1, time.Second, func(name string) (bool, error) {
				return !strings.HasPrefix(name, "skip"), nil
			})
			if d := time.Since(t0); d != 2*time.Second || len(out) != 2 {
				t.Errorf("pass took %v with %v, want 2s: a pause after a and b alone", d, out)
			}
		})
	})
	t.Run("cancel", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			t0 := time.Now()
			time.AfterFunc(30*time.Minute, cancel)
			out := m.maintainPass(ctx, []string{"a", "slow", "b", "c"}, 2, time.Hour, func(name string) (bool, error) {
				if name == "slow" {
					time.Sleep(2 * time.Hour)
				}
				return true, nil
			})
			if d := time.Since(t0); d != 2*time.Hour {
				t.Errorf("pass returned after %v, want 2h, when slow was done", d)
			}
			if got := slices.Sorted(maps.Keys(out)); !slices.Equal(got, []string{"a", "slow"}) {
				t.Errorf("maintained %v, want a and slow, nothing after the cancel", got)
			}
		})
	})
}

// TestDetachedLoopSchedule: the first pass over the names after ten
// minutes, then one a day from the start of the one before, each reading
// the names afresh; a pass that runs past the next start is followed at
// once, never overlapped.
func TestDetachedLoopSchedule(t *testing.T) {
	m := newManager(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		t0 := time.Now()
		var calls []time.Duration
		running := 0
		done := make(chan struct{})
		go func() {
			m.RunMaintenanceLoop(ctx, MaintenanceOptions{Names: func(context.Context) ([]string, error) {
				running++
				defer func() { running-- }()
				if running > 1 {
					t.Error("two passes at once")
				}
				calls = append(calls, time.Since(t0))
				if len(calls) == 2 {
					time.Sleep(30 * time.Hour)
				}
				return nil, nil
			}})
			close(done)
		}()
		time.Sleep(80 * time.Hour)
		cancel()
		synctest.Wait()
		hr, mn := time.Hour, time.Minute
		if want := []time.Duration{10 * mn, 24*hr + 10*mn, 54*hr + 10*mn, 78*hr + 10*mn}; !slices.Equal(calls, want) {
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

// TestASkippedPassKeepsTheFailureStreak: a pass that finds the replica busy
// neither ends its streak nor adds to it, while a detached one drops out.
func TestASkippedPassKeepsTheFailureStreak(t *testing.T) {
	h := &levels{}
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(h)})
	if err != nil {
		t.Fatal(err)
	}
	m.attached["a"] = &attachedDB{}
	fail := errors.New("503 SlowDown")
	failing := map[string]int{"a": 2, "b": 2}
	failing = m.logMaintenance(map[string]error{}, failing)
	if failing["a"] != 2 || len(failing) != 1 {
		t.Fatalf("failing after a skipped pass = %v, want a:2 alone", failing)
	}
	failing = m.logMaintenance(map[string]error{"a": fail}, failing)
	if failing["a"] != 3 || !slices.Equal(h.got, []slog.Level{slog.LevelError}) {
		t.Errorf("failing = %v and levels %v, want a:3 logged as an error", failing, h.got)
	}
}
