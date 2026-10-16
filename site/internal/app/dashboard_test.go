package app_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/prom"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/docs/example/board"
	"github.com/zweiundeins/lith/site/internal/docs/example/board/migrations"
)

// TestDashboard checks that every metric the Grafana dashboard and the
// alert rules in lith's deploy/observability read is one the site exports,
// under its namespace: a renamed metric must not leave a panel empty. They
// serve every lith app, so they name none. The site keeps no events, so
// the events metrics come from the board example's store, under the
// site's namespace.
func TestDashboard(t *testing.T) {
	a, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/")
	get(t, c, base+"/nope")
	events := listen(t, c, base+"/demos/guestbook/stream?tab=tab1")
	next(t, events, `id="guestbook-entries"`)
	post(t, c, base+"/cmd/guestbook/sign", `{"tab":"tab1","op":"op1","name":"Ada","message":"Hello"}`)
	next(t, events, `"ack":"op1"`)

	rec := httptest.NewRecorder()
	a.Metrics().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	exported := map[string]bool{}
	for line := range strings.Lines(rec.Body.String() + boardEvents(t)) {
		if name, _, ok := strings.Cut(line, "{"); ok && !strings.HasPrefix(line, "#") {
			exported[name] = true
		} else if name, _, ok := strings.Cut(line, " "); ok && !strings.HasPrefix(line, "#") {
			exported[name] = true
		}
	}
	wanted := map[string]string{}
	for _, file := range []string{"../../../deploy/observability/lith-apps.json", "../../../deploy/observability/rules-lith-apps.yaml"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"lith_site", "lith-site", "job="} {
			if strings.Contains(string(b), name) {
				t.Errorf("%s holds %q, but serves every lith app on any scrape job", file, name)
			}
		}
		for _, re := range []*regexp.Regexp{namespaced, anyNamespace} {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				wanted["lith_site_"+m[1]] = file
			}
		}
		for _, m := range runtime.FindAllString(string(b), -1) {
			wanted[m] = file
		}
	}
	if len(wanted) < 20 {
		t.Fatalf("found only %d metric names in the dashboard and the rules", len(wanted))
	}
	for name, file := range wanted {
		if !exported[name] {
			t.Errorf("%s reads %s, which the site does not export", file, name)
		}
	}
}

var (
	namespaced   = regexp.MustCompile(`\$\{ns\}_([a-z0-9_]+)`)
	anyNamespace = regexp.MustCompile(`__name__=~\\?"\.\+_([a-z0-9_]+)\\?"`)
	runtime      = regexp.MustCompile(`\b(go_[a-z_]+|process_[a-z_]+)\b`)
)

// boardEvents returns the metrics of lith/prom's events collector, for the
// board example's store and a runner of its projectors.
func boardEvents(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "board.db"), sqlite.Options{Migrations: migrations.FS})
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
	runner := events.NewRunner(board.Events, db, w, events.Options{Log: slog.New(slog.DiscardHandler)})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx) }()
	defer func() {
		stop()
		<-done
	}()
	for deadline := time.Now().Add(10 * time.Second); len(runner.Stats().Projectors) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the runner did not start")
		}
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(prom.New(app.Namespace, prom.Sources{Events: runner, Store: board.Events}))
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
