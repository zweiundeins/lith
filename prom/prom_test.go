package prom

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

func TestCollector(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"), sqlite.Options{
		Migrations: fstest.MapFS{"001_init.sql": {Data: []byte("CREATE TABLE n (v INTEGER);")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w, err := writer.New(db, writer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(ctx)
	defer w.Close()
	for range 3 {
		if err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			_, err := tx.Exec("INSERT INTO n (v) VALUES (1)")
			return err
		})); err != nil {
			t.Fatal(err)
		}
	}
	hub := live.New(live.Options{Read: db.R, SSE: sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{Limit: 64 << 20})}})

	reg := prometheus.NewRegistry()
	reg.MustRegister(New("app", Sources{Writer: w, Hub: hub}))
	want := `
# HELP app_writer_commands_total Commands the writer ran, failed ones included.
# TYPE app_writer_commands_total counter
app_writer_commands_total 3
# HELP app_live_streams Streams open now.
# TYPE app_live_streams gauge
app_live_streams 0
# HELP app_live_newest_frame_behind_total Reports from pages that fell behind their stream (newest-frame-behind, Hub.Behind).
# TYPE app_live_newest_frame_behind_total counter
app_live_newest_frame_behind_total 0
# HELP app_sse_budget_limit_bytes The memory the streams may hold together, their encoders and the frames kept for them, 0 for none.
# TYPE app_sse_budget_limit_bytes gauge
app_sse_budget_limit_bytes 6.7108864e+07
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "app_writer_commands_total", "app_live_streams", "app_live_newest_frame_behind_total", "app_sse_budget_limit_bytes"); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(reg); err != nil || n < 17 {
		t.Fatalf("%d metrics, %v", n, err)
	}
}

// A collector without sources exports nothing and does not panic.
func TestNoSources(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(New("app", Sources{}))
	if n, err := testutil.GatherAndCount(reg); err != nil || n != 0 {
		t.Fatalf("%d metrics, %v", n, err)
	}
}

type taskEvent interface{ taskEvent() }

type taskAdded struct {
	Title string `json:"title"`
}

func (taskAdded) taskEvent() {}

var (
	tasks = events.NewStream[int64, taskEvent]("task")
	added = events.Define(tasks, "task.added", func(e taskAdded) taskEvent { return e })
)

// The event store's counters, the head, and a catch-up projector that
// parked on the second event, a position behind.
func TestEvents(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w, err := writer.New(db, writer.Options{Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(ctx)
	defer w.Close()
	hook := events.NewProjector(tasks, "hook", 1,
		events.After(added, func(ctx context.Context, r events.Record[int64, taskAdded]) error {
			if r.Data.Title == "poison" {
				return errors.New("refused")
			}
			return nil
		}))
	st := events.Must(events.New(hook))
	runner := events.NewRunner(st, db, w, events.Options{Retries: 1, Log: slog.New(slog.DiscardHandler)})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx) }()
	defer func() {
		stop()
		<-done
	}()
	add := func(id int64, want events.Expected, title string) error {
		return w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			return events.Append(st, tx, tasks.ID(id), want, events.Meta{}, events.One[taskEvent](taskAdded{Title: title}))
		}))
	}
	for i, title := range []string{"a", "poison"} {
		if err := add(int64(i+1), events.NoStream, title); err != nil {
			t.Fatal(err)
		}
	}
	if err := add(1, events.NoStream, "again"); !errors.Is(err, events.ErrConflict) {
		t.Fatalf("an append to a stream that exists: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for runner.Stats().Projectors[0].Status != "parked" {
		if time.Now().After(deadline) {
			t.Fatalf("not parked: %+v", runner.Stats())
		}
		time.Sleep(time.Millisecond)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(New("app", Sources{Events: runner, Store: st}))
	want := `
# HELP app_events_appended_total Events committed, by stream kind.
# TYPE app_events_appended_total counter
app_events_appended_total{stream="task"} 2
# HELP app_events_conflicts_total Appends refused because the stream was not at the expected version.
# TYPE app_events_conflicts_total counter
app_events_conflicts_total 1
# HELP app_events_head_position The last position committed in the database.
# TYPE app_events_head_position gauge
app_events_head_position 2
# HELP app_events_projector_lag Positions between a catch-up projector or a rebuild and the last event of its stream kind; 0 for a live inline projector.
# TYPE app_events_projector_lag gauge
app_events_projector_lag{projector="hook"} 1
# HELP app_events_projector_position A catch-up projector's checkpoint, or how far a rebuild got.
# TYPE app_events_projector_position gauge
app_events_projector_position{projector="hook"} 1
# HELP app_events_projector_status 1 for the projector's status (live, rebuilding or parked), 0 for the others.
# TYPE app_events_projector_status gauge
app_events_projector_status{projector="hook",status="live"} 0
app_events_projector_status{projector="hook",status="parked"} 1
app_events_projector_status{projector="hook",status="rebuilding"} 0
# HELP app_events_projector_attempts Failures of the event a catch-up projector retries.
# TYPE app_events_projector_attempts gauge
app_events_projector_attempts{projector="hook"} 1
`
	names := []string{"app_events_appended_total", "app_events_conflicts_total", "app_events_head_position",
		"app_events_projector_lag", "app_events_projector_position", "app_events_projector_status", "app_events_projector_attempts"}
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), names...); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(reg, "app_events_refolds_total"); err != nil || n != 1 {
		t.Fatalf("%d refolds series, %v", n, err)
	}
}
