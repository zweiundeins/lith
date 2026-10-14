package prom

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

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
# HELP app_sse_budget_limit_bytes The memory the streams may hold together, their encoders and the frames kept for them, 0 for none.
# TYPE app_sse_budget_limit_bytes gauge
app_sse_budget_limit_bytes 6.7108864e+07
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "app_writer_commands_total", "app_live_streams", "app_sse_budget_limit_bytes"); err != nil {
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
