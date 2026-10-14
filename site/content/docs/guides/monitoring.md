---
title: Metrics and monitoring
nav: Monitoring
headline: Monitoring lith with Prometheus and Grafana
summary: The writer's, the hub's and the budget's numbers in Prometheus with lith/prom, statement timings with dbmetrics, the Grafana dashboard and alert rules this site ships, the log lines worth reading, and the probes.
description: The writer's, the hub's and the budget's numbers in Prometheus, statement timings with dbmetrics, the Grafana dashboard and alert rules, logs and probes.
order: 10
---

A lith app measures itself: `w.Stats()`, `hub.Stats()` and `hub.Budget().Stats()` count what the writer, the hub and the streams' encoders do, from the start of the process. `lith/prom` exports them to Prometheus, `dbmetrics` times every statement, and lith's `deploy/observability` has a dashboard and alert rules that read them for any lith app.

## The numbers that matter

| Source | Number | What it says |
| --- | --- | --- |
| `writer.Stats` | `Commands`, `Batches` | commands per transaction: high under load, as commands share commits |
| | `Failures` | commands that returned an error or panicked, refusals included |
| | `Queue` | commands waiting now; a queue that stays long means the writer cannot keep up |
| | `P99Commit` | the 99th percentile of the last 1024 transactions, from `BEGIN` to the end of `COMMIT` |
| `live.Stats` | `Ticks`, `Renders` | renders per tick |
| | `Overruns` | ticks that took longer than the tick (50 ms): frames come late for everyone |
| | `FramesSent`, `FramesDropped` | frames written to streams, and frames a newer one replaced before their stream sent them: slow clients |
| | `Streams`, `Views` | open streams, watched views |
| | `RendersByKey` | renders per view: a view that renders more often than its topics change listens to too much |
| `sse.BudgetStats` | `Limit`, `Reserved` | what the streams may hold and hold now: encoders, 32 KiB a stream, frames |
| | `Frames` | the frames the hub keeps for the views, part of `Reserved` |
| | `Streams` | open streams by encoder setting, uncompressed ones as `identity` |
| | `Refused` | streams turned away for want of memory |
| | `Reopened` | brotli streams asked to open again for a window that holds their frames |

## Prometheus

`lith/prom` is a module of its own, so an app without Prometheus never pulls in its client. `prom.New(namespace, sources)` returns a collector that reads the sources' stats on every scrape. `dbmetrics` times the statements a database/sql driver runs: register its wrapper of modernc's driver once, name it in `sqlite.Options.Driver`, and the write connection, the read pool and `Backup` all open through it. This site wires both:

```go source=site/internal/app/app.go
driverOnce.Do(func() {
	driverErr = dbmetrics.Register(driver, "sqlite", dbmetrics.Options{Observer: dbMetrics, Logger: cfg.Log})
})
```

```go source=site/internal/app/app.go
reg := prometheus.NewRegistry()
reg.MustRegister(
	collectors.NewGoCollector(),
	collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	prom.New(Namespace, prom.Sources{Writer: w, Hub: hub}),
	dbMetrics,
	httpMetrics,
	prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: Namespace, Name: "build_info", Help: "1, labelled with the running version.",
		ConstLabels: prometheus.Labels{"version": cfg.Version},
	}, func() float64 { return 1 }),
)
```

`dbMetrics` is `dbprom.New(Namespace)`, from the module `lith/dbmetrics/prom`. `dbmetrics` also logs every statement that takes 100 ms or more at Warn, with its SQL text and never its arguments (`Options.SlowThreshold`, negative to turn it off).

The metrics, with the namespace `app`:

| Metric | From |
| --- | --- |
| `app_writer_batches_total`, `app_writer_commands_total`, `app_writer_command_failures_total`, `app_writer_busy_seconds_total` | `lith/prom` |
| `app_writer_queue`, `app_writer_batch_max`, `app_writer_commit_p99_seconds` | `lith/prom` |
| `app_live_ticks_total`, `app_live_renders_total`, `app_live_tick_overruns_total`, `app_live_read_txs_total` | `lith/prom` |
| `app_live_frames_sent_total`, `app_live_frames_dropped_total`, `app_live_streams`, `app_live_views` | `lith/prom` |
| `app_sse_budget_limit_bytes`, `app_sse_budget_reserved_bytes`, `app_sse_budget_streams{setting}`, `app_sse_budget_refused_total` | `lith/prom` |
| `app_db_query_duration_seconds{op}`, `app_db_slow_queries_total{op}` | `lith/dbmetrics/prom` |
| `app_backup_enabled`, `app_backup_replica_last_sync_timestamp_seconds{replica}` and the rest | `lith/backup` ([Backups](/docs/guides/backups#replication-with-litestream)) |

Requests by route are the app's own: this site's `internal/metrics` counts `lith_site_http_requests_total{route,code}` and `lith_site_http_request_duration_seconds{route}`, labelled with the ServeMux pattern that matched, never the raw path, so the number of series stays small whatever URLs arrive.

## A listener of its own

The metrics never go on the public listener. This site serves them on 127.0.0.1 at its public port plus 100, where the host's Prometheus scrapes every app, and refuses an address that is not loopback:

```go source=site/cmd/lith-site/main.go
host, _, err := net.SplitHostPort(flagValue)
if err != nil {
	return "", fmt.Errorf("-metrics: %w", err)
}
if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
	return "", fmt.Errorf("-metrics %s: the metrics listen on loopback only", flagValue)
}
return flagValue, nil
```

## The dashboard

[`deploy/observability/lith-apps.json`](https://github.com/zweiundeins/lith/blob/main/deploy/observability/lith-apps.json) shows any lith app: pick its metric namespace, then the app and the slot. Its rows:

- **Requests**: requests, 5xx and p95 latency by route.
- **Live**: open streams beside the requests in flight, ticks a second and overruns, renders per tick, frames sent and dropped, watched views and read transactions.
- **Writer**: commands a second, commands per batch and the largest batch, commit p99, failures, the queue, the time the writer is busy.
- **Streams**: the budget reserved against its limit, streams by encoder setting, refused streams.
- **Database**: p95 by operation, statements and slow statements.
- **Process**: Go heap in use, resident memory, goroutines, CPU.

The site's `TestDashboard` fails when the dashboard or the alert rules read a metric the site does not export.

## Alerts

[`rules-lith-apps.yaml`](https://github.com/zweiundeins/lith/blob/main/deploy/observability/rules-lith-apps.yaml) holds Grafana alert rules that match any lith app by its metric names, one alert for each app and slot:

| Alert | Fires when |
| --- | --- |
| 5xx ratio | more than 2% of requests, and at least 5 in 5 minutes, answer a 5xx |
| ticks overrun | more than one tick in ten takes longer than the tick, for 10 minutes |
| frames dropped | more than 5% of frames are dropped, for 10 minutes |
| writer failures | more than 3 commands fail in 10 minutes |
| streams refused | the budget turned streams away in the last 10 minutes |

A site that stops answering trips the host's rule on every blackbox probe, so the file has no rule for that.

A refusal returned from `Apply` counts as a failure, so an app that refuses often either records its refusals in a row, as the board's `Act` does, or raises that alert's threshold. The thresholds are starting points. The rules were written to the host's format without a running instance: import them, check that they evaluate against real data, and export them over the file before relying on them.

## Logs

The site logs slog JSON on stdout, one object per line, which systemd's journal keeps and Alloy ships to Loki. The lines worth an alert or a look:

- `live: tick overrun` with `n`, `p50`, `p95`, `p99` and `max`: the hub samples every tick for 20 s after one overruns, then logs this line. A p99 that stays above the tick means a tick holds too much work: a view that renders too often, or renders too slowly.
- `live: render` with the view's key and the error, and `live: render overrun` for a render past `RenderTimeout`.
- `writer: batch failed`, `writer: command panicked` with its stack, and `writer: command failed` for a `Send` without `OnSendError`.
- dbmetrics' slow statements, at Warn, with their SQL.
- One line per request from `edge.LogRequests`, at Error for a 5xx, and at Debug for a request whose handler called `edge.Quiet`, such as the cursor party's moves.

## Probes

`/healthz` answers `ok <version>` while the process serves; the deploy script waits for the new version there. `/readyz` answers 200 while its checks pass and 503 naming the ones that fail: this site checks the read pool with `SELECT 1`, and that the writer and the hub run. A blackbox probe of a public page, with a keyword the page must contain, catches what neither sees: a broken proxy, an expired certificate, an error page served with a 200. The site's probe files are in `site/deploy/observability`.
