// Command lith-site serves lith's website: the landing page, the docs and
// the demos, from one binary and one SQLite file.
//
//	lith-site [serve] [flags]   run the server (the default)
//	lith-site --version         print the version
//
// Every flag can also come from the environment: -db from LITH_SITE_DB,
// -addr from LITH_SITE_ADDR. The command line wins.
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
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zweiundeins/lith/edge"

	"github.com/zweiundeins/lith/site/internal/app"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version string

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println("lith-site", buildVersion())
		return
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if err := serve(args); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	f := flag.NewFlagSet("serve", flag.ExitOnError)
	f.Usage = func() {
		fmt.Fprint(f.Output(), "usage: lith-site [serve] [flags]\n\nEach flag can also be set in the environment, -db as LITH_SITE_DB.\n\n")
		f.PrintDefaults()
	}
	addr := f.String("addr", "127.0.0.1:8097", "listen on host:port, or on a Unix socket as unix:/path")
	metricsAddr := f.String("metrics", "auto", "serve /metrics on this loopback host:port; auto: 127.0.0.1 at -addr's port + 100; off: none")
	dbPath := f.String("db", "data/site.db", "the SQLite database")
	base := f.String("base", "", "the site's public URL, for canonical links and the sitemap (default: http://<addr>)")
	repo := f.String("repo", "https://github.com/zweiundeins/lith", "the repository's URL")
	streams := f.Int("streams", 300, "how many streams may be open at once; more get 503")
	clientStreams := f.Int("client-streams", 16, "how many streams one client address may hold open at once; more get 429")
	workers := f.Int("workers", 2, "render workers")
	synchronous := f.String("synchronous", "NORMAL", "NORMAL (a power loss can undo up to about 35 s of commits), or FULL (one fsync per batch)")
	secure := f.Bool("secure", false, "served over HTTPS: a __Host- cookie and HSTS")
	dev := f.Bool("dev", false, "development: show draft docs pages, read the docs from ./content on every request, serve scripts and stylesheets as written")
	logFormat := f.String("log", "json", "json (one object per line on stdout, for journald) or text")
	debugLog := f.Bool("debug", false, "log at debug level")
	if err := parse(f, args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *debugLog {
		level = slog.LevelDebug
	}
	var handler slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	if *logFormat == "text" {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	log := slog.New(handler)
	slog.SetDefault(log)

	if *base == "" {
		*base = "http://" + strings.TrimPrefix(*addr, "unix:")
	}
	maddr, err := metricsListen(*metricsAddr, *addr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, app.Config{
		DB: *dbPath, Base: strings.TrimRight(*base, "/"), Repo: *repo, Version: buildVersion(),
		Secure: *secure, Dev: *dev, MaxStreams: *streams, ClientStreams: *clientStreams, Workers: *workers, Synchronous: *synchronous, Log: log,
	})
	if err != nil {
		return err
	}
	ln, err := listen(*addr)
	if err != nil {
		return errors.Join(err, a.Close())
	}
	srv := &http.Server{
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		// No WriteTimeout: a stream gives each of its writes a deadline.
	}
	var msrv *http.Server
	if maddr != "" {
		mln, err := net.Listen("tcp", maddr)
		if err != nil {
			ln.Close()
			return errors.Join(fmt.Errorf("metrics: %w", err), a.Close())
		}
		msrv = &http.Server{Handler: metricsMux(a.Metrics()), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := msrv.Serve(mln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics", "err", err)
			}
		}()
	}

	a.Start()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("serving", "addr", *addr, "metrics", maddr, "db", *dbPath, "version", buildVersion(),
		"proxy_headers_from", edge.TrustedProxies(ln.Addr()))
	select {
	case err = <-served:
	case <-ctx.Done():
	}
	stop() // a second signal kills the process

	log.Info("shutting down")
	a.StopStreams() // 1. every stream ends with sse.Reconnect: pages reconnect to the next process
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil { // 2. actions finish, and still reach the writer
		log.Warn("shutdown", "err", err)
	}
	if msrv != nil {
		msrv.Close()
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, a.Close()) // 3. the writer applies what is queued; 4. the database closes
}

// metricsMux serves the metrics at /metrics and at every other path, as
// the host's other apps do.
func metricsMux(h http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", h)
	return mux
}

// metricsListen resolves -metrics: auto is 127.0.0.1 at the public port +
// 100, the host's convention; an explicit address must be on loopback,
// since the metrics are not for the public.
func metricsListen(flagValue, addr string) (string, error) {
	switch flagValue {
	case "", "off":
		return "", nil
	case "auto":
		if strings.HasPrefix(addr, "unix:") {
			return "", nil
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return "", err
		}
		p, err := strconv.Atoi(port)
		if err != nil || p == 0 {
			return "", nil // a port chosen by the system has no neighbour to predict
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(p+100)), nil
	}
	host, _, err := net.SplitHostPort(flagValue)
	if err != nil {
		return "", fmt.Errorf("-metrics: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("-metrics %s: the metrics listen on loopback only", flagValue)
	}
	return flagValue, nil
}

// listen opens host:port, or unix:/path: a socket for the site's user and
// group (0660), which the proxy's user joins. The site believes the
// X-Forwarded-For of whoever connects to it, as on loopback, where every
// local user may.
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
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// parse reads the flags, then takes each one the command line left out
// from the environment: -addr from LITH_SITE_ADDR.
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	given := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { given[fl.Name] = true })
	var errs []error
	f.VisitAll(func(fl *flag.Flag) {
		key := "LITH_SITE_" + strings.ToUpper(fl.Name)
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
