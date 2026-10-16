package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zweiundeins/lith/dbmetrics"
	dbprom "github.com/zweiundeins/lith/dbmetrics/prom"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/prom"
	"github.com/zweiundeins/lith/writer"
)

// namespace prefixes app's metrics, app_writer_*, app_live_*,
// app_sse_budget_* and app_db_*, which the "lith apps" dashboard and alert
// rules in lith's deploy/observability read. Metric names take no -.
var namespace = strings.ReplaceAll("app", "-", "_")

// sqliteDriver is modernc's driver wrapped by dbmetrics, which times every
// statement of the write connection, the read pool and Backup into
// dbMetrics, and logs those that take 100 ms or more at Warn.
const sqliteDriver = "sqlite-app"

var (
	dbMetrics  = dbprom.New(namespace)
	driverOnce sync.Once
	driverErr  error
)

// registerDriver registers sqliteDriver once per process, as database/sql
// wants, with the logger of the first call.
func registerDriver(log *slog.Logger) error {
	driverOnce.Do(func() {
		driverErr = dbmetrics.Register(sqliteDriver, "sqlite", dbmetrics.Options{Observer: dbMetrics, Logger: log})
	})
	return driverErr
}

// metrics serves app's numbers in Prometheus' text format at every path:
// the writer's, the hub's and the streams' budget's (lith/prom), the
// statements' (dbmetrics), the Go runtime's and the process's.
func metrics(w *writer.Writer, hub *live.Hub, log *slog.Logger) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prom.New(namespace, prom.Sources{Writer: w, Hub: hub}), // the budget is the hub's
		dbMetrics,
	)
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)})
}

// metricsListen resolves -metrics: auto is 127.0.0.1 at -addr's port + 100,
// the host's convention, and none for a Unix socket, whose unit names the
// port; off is none. An address must be on loopback, since the metrics are
// not for the public.
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
