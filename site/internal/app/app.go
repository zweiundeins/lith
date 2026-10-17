// Package app wires the site the way lith's template wires an app: one
// SQLite file opened through a timed driver, the writer, the live hub, the
// web server behind the edge, the sweep, and a registry of metrics for a
// listener of its own. cmd/lith-site runs it; tests run it in process.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zweiundeins/lith/dbmetrics"
	dbprom "github.com/zweiundeins/lith/dbmetrics/prom"
	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/prom"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demos"
	"github.com/zweiundeins/lith/site/internal/metrics"
	"github.com/zweiundeins/lith/site/internal/migrate"
	"github.com/zweiundeins/lith/site/internal/web"
)

// Namespace prefixes the site's metrics: lith_site_http_*, lith_site_writer_*,
// lith_site_live_*, lith_site_sse_budget_* and lith_site_db_*.
const Namespace = "lith_site"

// Config is how the site runs.
type Config struct {
	DB          string // the SQLite file
	Base        string // the public URL, without a trailing /
	Repo        string // the repository on GitHub
	Version     string
	Secure      bool // served over HTTPS: a __Host- cookie and HSTS
	Dev         bool // drafts in the docs, read from ./content; scripts and stylesheets as written
	MaxStreams  int  // open streams at most; more get 503
	Workers     int  // render workers, each holding a read connection
	Synchronous string
	Log         *slog.Logger
	// Demos are the demos to serve; nil serves demos.All.
	Demos []demo.Demo
	// AckWindow, AckAfterPaint, Hybrid and AckRelease pace the demos'
	// streams by the pages' acknowledgements (web.Config), for
	// measurements.
	AckWindow     int
	AckAfterPaint bool
	Hybrid        string
	AckRelease    time.Duration
}

// App is the running site.
type App struct {
	cfg     Config
	db      *sqlite.DB
	w       *writer.Writer
	hub     *live.Hub
	handler http.Handler
	metrics http.Handler
	log     *slog.Logger
	// runner keeps the projectors of the demos' event store current, nil
	// when no demo keeps events.
	runner *events.Runner

	writerUp, hubUp atomic.Bool
	hubCtx          context.Context
	stopHub         context.CancelFunc
	hubDone         chan struct{}
	writerDone      chan struct{}
	jobsCtx         context.Context
	stopJobs        context.CancelFunc
	jobs            sync.WaitGroup
	stopRunner      context.CancelFunc
	runnerDone      chan struct{}
}

// dbMetrics times every statement of every database the process opens; it
// is one, since the driver it observes is registered once.
var (
	dbMetrics  = dbprom.New(Namespace)
	driverOnce sync.Once
	driverErr  error
)

const driver = "sqlite-lith-site"

// New opens the database and builds the site. Start runs it.
func New(ctx context.Context, cfg Config) (*App, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Demos == nil {
		cfg.Demos = demos.All
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 300
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if err := demos.Check(cfg.Demos); err != nil {
		return nil, err
	}
	driverOnce.Do(func() {
		driverErr = dbmetrics.Register(driver, "sqlite", dbmetrics.Options{Observer: dbMetrics, Logger: cfg.Log})
	})
	if driverErr != nil {
		return nil, driverErr
	}
	db, err := sqlite.Open(ctx, cfg.DB, sqlite.Options{
		Synchronous: cfg.Synchronous,
		Migrate: func(ctx context.Context, w *sql.DB) error {
			return migrate.Run(ctx, w, demos.Scopes(cfg.Demos))
		},
		Readers:    cfg.Workers + 4, // the render workers hold one each, GET documents share the rest
		CacheKiB:   4096,            // the demos' tables are small; keep the page caches small too
		Checkpoint: sqlite.CheckpointWriter,
		Driver:     driver,
	})
	if err != nil {
		return nil, err
	}
	hub := live.New(live.Options{
		Read:       db.R,
		Workers:    cfg.Workers,
		MaxStreams: cfg.MaxStreams,
		// An idle tab gives its zstd encoder back after a minute: most tabs
		// on a public site sit still, and an encoder holds 1 MiB or more.
		Hibernate: time.Minute,
		// No window below 2^17, whatever a stream's largest write: the
		// checkbox grid sends its twelve tiles in turn, and a tile refers
		// back to its own last frame, 104 KB back.
		SSE: sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{MinWindow: 17})},
		Log: cfg.Log,
	})
	w, err := writer.New(db, writer.Options{OnCommit: hub.Changed, Log: cfg.Log})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	site, err := web.New(web.Config{Base: cfg.Base, Repo: cfg.Repo, Version: cfg.Version, Dev: cfg.Dev, AckWindow: cfg.AckWindow, AckAfterPaint: cfg.AckAfterPaint, Hybrid: cfg.Hybrid, AckRelease: cfg.AckRelease}, db, w, hub, cfg.Log, cfg.Demos)
	if err != nil {
		return nil, errors.Join(err, w.Close(), db.Close())
	}
	a := &App{cfg: cfg, db: db, w: w, hub: hub, log: cfg.Log}
	sources := prom.Sources{Writer: w, Hub: hub}
	for _, d := range cfg.Demos {
		if d.Events != nil {
			a.runner = events.NewRunner(d.Events, db, w, events.Options{Log: cfg.Log})
			sources.Events, sources.Store = a.runner, d.Events
		}
	}

	httpMetrics := metrics.NewHTTP(Namespace)
	h := metrics.Route(site.Routes())
	h = edge.Sessions{Secure: cfg.Secure}.Handler(h)
	h = edge.CSRF{Datastar: []string{"POST /cmd/"}}.Handler(h)
	h = edge.SecurityHeaders{CSP: edge.DefaultCSP(), HSTS: cfg.Secure}.Handler(h)
	h = edge.Compress(h)
	h = edge.Recover(cfg.Log, h)
	h = edge.LogRequests(cfg.Log, h)
	h = httpMetrics.Measure(h)

	ready := &edge.Ready{Log: cfg.Log}
	ready.Add("database", func(ctx context.Context) error {
		var one int
		return db.R.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	})
	ready.Add("writer", running(&a.writerUp))
	ready.Add("hub", running(&a.hubUp))
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", edge.Health(cfg.Version))
	mux.Handle("GET /readyz", ready)
	mux.Handle("/", h) // probes skip the edge: no cookie, no log line, no count
	a.handler = mux

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prom.New(Namespace, sources),
		dbMetrics,
		httpMetrics,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: Namespace, Name: "build_info", Help: "1, labelled with the running version.",
			ConstLabels: prometheus.Labels{"version": cfg.Version},
		}, func() float64 { return 1 }),
	)
	a.metrics = promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorLog: slog.NewLogLogger(cfg.Log.Handler(), slog.LevelWarn)})
	return a, nil
}

func running(up *atomic.Bool) func(context.Context) error {
	return func(context.Context) error {
		if !up.Load() {
			return errors.New("not running")
		}
		return nil
	}
}

// Handler is the public handler: the site behind the edge, and the probes
// /healthz and /readyz beside it. It never serves /metrics.
func (a *App) Handler() http.Handler { return a.handler }

// Metrics serves Prometheus' text format: the Go runtime, the process, the
// writer, the hub, the streams' budget, the statements and the requests.
// Serve it on a loopback listener of its own.
func (a *App) Metrics() http.Handler { return a.metrics }

// DB, Writer and Hub are for tests.
func (a *App) DB() *sqlite.DB         { return a.db }
func (a *App) Writer() *writer.Writer { return a.w }
func (a *App) Hub() *live.Hub         { return a.hub }

// Start runs the writer, the hub, the sweep and the events runner. The
// writer runs until Close, so actions still write while the HTTP server
// drains.
func (a *App) Start() {
	a.writerDone = make(chan struct{})
	a.writerUp.Store(true)
	go func() {
		defer close(a.writerDone)
		a.w.Run(context.Background())
		a.writerUp.Store(false)
	}()
	a.hubCtx, a.stopHub = context.WithCancel(context.Background())
	a.hubDone = make(chan struct{})
	a.hubUp.Store(true)
	go func() {
		defer close(a.hubDone)
		// Every view's Render gets the hub in its ctx, for demo.Watching.
		if err := a.hub.Run(demo.WithHub(a.hubCtx, a.hub)); err != nil {
			a.log.Error("render loop", "err", err)
		}
		a.hubUp.Store(false)
	}()
	a.jobsCtx, a.stopJobs = context.WithCancel(context.Background())
	a.jobs.Go(func() { a.sweep(a.jobsCtx) })
	if a.runner != nil {
		var ctx context.Context
		ctx, a.stopRunner = context.WithCancel(context.Background())
		a.runnerDone = make(chan struct{})
		go func() {
			defer close(a.runnerDone)
			if err := a.runner.Run(ctx); err != nil {
				a.log.Error("events runner", "err", err)
			}
		}()
	}
}

// StopStreams ends every stream, each with sse.Reconnect as its last
// event, so that the pages reconnect to the next process. Call it before
// the HTTP server's Shutdown, which would wait for them otherwise.
func (a *App) StopStreams() {
	if a.stopHub != nil {
		a.stopHub()
		<-a.hubDone
	}
}

// Close stops the sweep and the events runner, lets the writer apply what
// is queued, and closes the database. Call it after the HTTP server's
// Shutdown.
func (a *App) Close() error {
	a.StopStreams()
	if a.stopJobs != nil {
		a.stopJobs()
		a.jobs.Wait()
	}
	if a.stopRunner != nil {
		a.stopRunner() // before the writer closes
		<-a.runnerDone
	}
	err := a.w.Close()
	if a.writerDone != nil {
		<-a.writerDone
	}
	return errors.Join(err, a.db.Close())
}

// sweep keeps the tables small: every demo's Sweep, the tab states gone
// quiet, the op ids older than a day.
func (a *App) sweep(ctx context.Context) {
	t := time.NewTicker(demo.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.SweepNow(ctx, time.Now()); err != nil && ctx.Err() == nil {
			a.log.Warn("sweep", "err", err)
		}
	}
}

// SweepNow runs every sweep once, as commands.
func (a *App) SweepNow(ctx context.Context, now time.Time) error {
	cmds := []writer.Command{writer.Func(func(tx *writer.Tx) error { return demo.SweepTabs(tx, now) }), writer.PruneOnce(24 * time.Hour)}
	for _, d := range a.cfg.Demos {
		if d.Sweep != nil {
			sweep := d.Sweep
			slug := d.Slug
			cmds = append(cmds, writer.Func(func(tx *writer.Tx) error {
				if err := sweep(tx, now); err != nil {
					return fmt.Errorf("%s: %w", slug, err)
				}
				return nil
			}))
		}
	}
	ctx = demo.WithHub(ctx, a.hub) // a sweep's tx.Context() has it
	var errs []error
	for _, c := range cmds {
		if err := a.w.Do(ctx, c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
