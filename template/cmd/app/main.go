// Command app serves the guestbook: one binary, one SQLite file.
//
//	app [serve] [flags]   run the server (the default)
//	app backup [flags]    copy the database while the server runs, and keep the newest copies
//	app --version         print the version
//
// Every flag can also come from the environment: -db from APP_DB, -addr
// from APP_ADDR. The command line wins. The metrics for Prometheus go out on
// a loopback listener of their own, -metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/template/internal/guestbook"
	"github.com/zweiundeins/lith/template/internal/web"
	"github.com/zweiundeins/lith/template/migrations"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version string

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println("app", buildVersion())
		return
	}
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(context.Background(), args)
	case "backup":
		err = backup(args)
	default:
		err = fmt.Errorf("unknown command %q: serve or backup", cmd)
	}
	if err != nil {
		slog.Error(cmd, "err", err)
		os.Exit(1)
	}
}

// serve runs the server until ctx ends or the process gets SIGINT or SIGTERM.
func serve(ctx context.Context, args []string) error {
	f := flags("serve")
	addr := f.String("addr", "127.0.0.1:8080", "listen on host:port, or on a Unix socket as unix:/path")
	metricsAddr := f.String("metrics", "auto", "serve the metrics for Prometheus on this loopback host:port; auto: 127.0.0.1 at -addr's port + 100, none for a Unix socket; off: none")
	dbPath := f.String("db", "data/app.db", "the SQLite database")
	synchronous := f.String("synchronous", "NORMAL", "NORMAL (a power loss can undo up to about 35 s of commits), or FULL (one fsync per batch, loses none)")
	streams := f.Int("streams", 200, "how many streams may be open at once; more get 503")
	secure := f.Bool("secure", false, "served over HTTPS: a __Host- cookie and HSTS")
	dev := f.Bool("dev", false, "development: serve the static files as written, not minified")
	debugLog := f.Bool("debug", false, "log at debug level")
	if err := parse(f, args); err != nil {
		return err
	}
	level := slog.LevelInfo
	if *debugLog {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	maddr, err := metricsListen(*metricsAddr, *addr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := registerDriver(log); err != nil {
		return err
	}
	db, err := sqlite.Open(ctx, *dbPath, sqlite.Options{
		Synchronous: *synchronous,
		Migrations:  migrations.FS,
		Readers:     runtime.GOMAXPROCS(0) + 4, // the render workers hold one each, GETs need the rest
		Checkpoint:  sqlite.CheckpointWriter,
		Driver:      sqliteDriver,
	})
	if err != nil {
		return err
	}
	hub := live.New(live.Options{Read: db.R, MaxStreams: *streams, Log: log})
	w, err := writer.New(db, writer.Options{OnCommit: hub.Changed, Log: log})
	if err != nil {
		return errors.Join(err, db.Close())
	}
	var ln, mln net.Listener
	site, err := web.New(db, w, hub, log, *dev)
	if err == nil {
		ln, err = listen(*addr)
	}
	if err == nil && maddr != "" {
		if mln, err = net.Listen("tcp", maddr); err != nil {
			ln.Close()
			err = fmt.Errorf("metrics: %w", err)
		}
	}
	if err != nil {
		return errors.Join(err, w.Close(), db.Close())
	}

	h := http.Handler(site.Routes())
	h = edge.Sessions{Secure: *secure}.Handler(h)
	h = edge.CSRF{Datastar: []string{"POST /cmd/", "POST /pace", "POST /ack"}}.Handler(h)
	h = edge.SecurityHeaders{CSP: edge.DefaultCSP(), HSTS: *secure}.Handler(h)
	h = edge.Compress(h)
	h = edge.Recover(log, h)
	h = edge.LogRequests(log, h)

	ready := &edge.Ready{Log: log}
	ready.Add("database", func(ctx context.Context) error {
		var one int
		return db.R.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	})
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", edge.Health(buildVersion()))
	mux.Handle("GET /readyz", ready)
	mux.Handle("/", h) // probes skip the edge: no cookie, no log line

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		// No WriteTimeout: a stream gives each of its writes a deadline.
	}
	// The metrics never go on the public listener.
	msrv := &http.Server{Handler: metrics(w, hub, log), ReadHeaderTimeout: 5 * time.Second}

	// The writer runs until Close, so actions still write while the HTTP
	// server drains. Cancelling the hub's context ends every stream at once.
	go w.Run(context.Background())
	hubCtx, stopHub := context.WithCancel(context.Background())
	defer stopHub()
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		if err := hub.Run(hubCtx); err != nil {
			log.Error("render loop", "err", err)
			stop()
		}
	}()
	var jobs sync.WaitGroup
	jobs.Go(func() { sweep(ctx, w, log) })

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	if mln != nil {
		go func() {
			if err := msrv.Serve(mln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics", "err", err)
			}
		}()
	}
	log.Info("serving", "addr", *addr, "metrics", maddr, "db", *dbPath, "version", buildVersion())
	select {
	case err = <-served:
	case <-ctx.Done():
	}
	stop() // the jobs end; a second signal kills the process

	log.Info("shutting down")
	stopHub() // 1. every stream ends, so Shutdown does not wait for them
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil { // 2. actions finish, and still reach the writer
		log.Warn("shutdown", "err", err)
	}
	msrv.Close()
	jobs.Wait()
	<-hubDone
	return errors.Join(err, w.Close(), db.Close()) // 3. the writer applies what is queued; 4. the database closes
}

// sweep keeps the tables that grow bounded, every hour: tab state of tabs
// gone for a week, entries beyond the newest thousand, old op ids.
func sweep(ctx context.Context, w *writer.Writer, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, cmd := range []writer.Command{guestbook.Sweep{At: time.Now()}, writer.PruneOnce(24 * time.Hour)} {
			if err := w.Do(ctx, cmd); err != nil && ctx.Err() == nil {
				log.Warn("sweep", "err", err)
			}
		}
	}
}

// backup writes a consistent copy of the database (VACUUM INTO, safe while
// the server runs) and deletes the oldest copies beyond -keep.
func backup(args []string) error {
	f := flags("backup")
	dbPath := f.String("db", "data/app.db", "the SQLite database")
	dir := f.String("backups", "data/backups", "the directory of the copies")
	keep := f.Int("keep", 28, "how many copies to keep")
	if err := parse(f, args); err != nil {
		return err
	}
	if *keep < 1 {
		return errors.New("-keep must be 1 or more")
	}
	if _, err := os.Stat(*dbPath); err != nil {
		return err // sqlite.Open would create an empty database
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, *dbPath, sqlite.Options{Readers: 1})
	if err != nil {
		return err
	}
	defer db.Close()
	to := filepath.Join(*dir, "app-"+time.Now().UTC().Format("20060102T150405Z")+".db")
	if err := db.Backup(ctx, to); err != nil {
		return err
	}
	copies, err := filepath.Glob(filepath.Join(*dir, "app-*.db"))
	if err != nil {
		return err
	}
	slices.Sort(copies) // oldest first: the names hold the time
	for _, old := range copies[:max(0, len(copies)-*keep)] {
		if err := os.Remove(old); err != nil {
			return err
		}
	}
	slog.Info("backup", "to", to, "copies", min(len(copies), *keep))
	return nil
}

// listen opens host:port, or unix:/path: a Unix socket for a proxy that runs
// as another user, where the socket's directory decides who may connect.
func listen(addr string) (net.Listener, error) {
	path, ok := strings.CutPrefix(addr, "unix:")
	if !ok {
		return net.Listen("tcp", addr)
	}
	os.Remove(path) // left by a process that did not exit cleanly
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ExitOnError)
	f.Usage = func() {
		fmt.Fprintf(f.Output(), "usage: app %s [flags]\n\nEach flag can also be set in the environment, -db as APP_DB.\n\n", name)
		f.PrintDefaults()
	}
	return f
}

// parse reads the flags, then takes each one the command line left out from
// the environment: -addr from APP_ADDR, -db from APP_DB.
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	given := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { given[fl.Name] = true })
	var errs []error
	f.VisitAll(func(fl *flag.Flag) {
		key := "APP_" + strings.ToUpper(fl.Name)
		if v, ok := os.LookupEnv(key); ok && !given[fl.Name] {
			if err := fl.Value.Set(v); err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: %w", key, v, err))
			}
		}
	})
	return errors.Join(errs...)
}

// buildVersion is version, or else the commit the binary was built from.
func buildVersion() string {
	if version != "" {
		return version
	}
	rev, dirty := "dev", ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && len(s.Value) >= 12:
				rev = s.Value[:12]
			case s.Key == "vcs.modified" && s.Value == "true":
				dirty = "-dirty"
			}
		}
	}
	return rev + dirty
}
