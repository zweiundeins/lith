---
title: Metrics and monitoring
nav: Monitoring
headline: Monitoring lith with Prometheus and Grafana
summary: The writer's, the hub's and the budget's numbers in Prometheus with lith/prom, statement timings with dbmetrics, the Grafana dashboard and alert rules lith ships, the log lines worth reading, the probes, and the files each app installs on a shared host.
description: The writer's, the hub's and the budget's numbers in Prometheus, statement timings, the Grafana dashboard and alerts, logs, probes and a host's drop-ins.
order: 11
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
| | `RendersDeferred` | renders put off while every stream that watches the view still had a frame of it waiting: streams that do not keep up, slow links |
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
| `app_live_frames_sent_total`, `app_live_frames_dropped_total`, `app_live_renders_deferred_total`, `app_live_streams`, `app_live_views` | `lith/prom` |
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
- **Live**: open streams beside the requests in flight, ticks a second and overruns, renders per tick, frames sent and dropped and renders deferred, watched views and read transactions.
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

This site logs slog JSON on stdout, one object per line; the template logs slog's text format. systemd's journal keeps both, and Alloy ships them to Loki with slog's level as a label ([below](#drop-in-files-for-a-shared-host)). The lines worth an alert or a look:

- `live: tick overrun` with `n`, `p50`, `p95`, `p99` and `max`: the hub samples every tick for 20 s after one overruns, then logs this line. A p99 that stays above the tick means a tick holds too much work: a view that renders too often, or renders too slowly.
- `live: render` with the view's key and the error, and `live: render overrun` for a render past `RenderTimeout`.
- `writer: batch failed`, `writer: command panicked` with its stack, and `writer: command failed` for a `Send` without `OnSendError`.
- dbmetrics' slow statements, at Warn, with their SQL.
- One line per request from `edge.LogRequests`, at Error for a 5xx, and at Debug for a request whose handler called `edge.Quiet`, such as the cursor party's moves.

## Probes

`/healthz` answers `ok <version>` while the process serves; the deploy script waits for the new version there. `/readyz` answers 200 while its checks pass and 503 naming the ones that fail: this site checks the read pool with `SELECT 1`, and that the writer and the hub run. A blackbox probe from outside catches what neither sees: a broken proxy, an expired certificate. A probe of `/healthz` that wants `ok ` at the start of the body also catches an error page served with a 200 by anything but the app. The template probes its home page and `/healthz`:

```yaml source=template/deploy/observability/app.targets.yml
- targets: ['https://app.example.com/']
  labels: { service: app, module: http_2xx }
- targets: ['https://app.example.com/healthz']
  labels: { service: app, module: http_2xx_ok }
```

## Drop-in files for a shared host

A host can run many apps, lith's and others, beside one Caddy, one Prometheus with its blackbox exporter, one Alloy, Loki and Grafana. The host keeps one configuration for each, and each loads a folder of files that the apps install, so that adding, moving or removing an app never edits the host's files:

| On the host | From the app's `deploy/` | What it holds |
| --- | --- | --- |
| `/etc/caddy/sites/<app>.caddy` | `<app>.caddy` | the site block, with its JSON access log in `/var/log/caddy/<site>-access.log` |
| `/etc/prometheus/apps/<app>.yml` | `observability/<app>.metrics.yml` | the metrics listener, as a `file_sd` target with the labels `app` and `slot` |
| `/etc/prometheus/targets/<app>.yml` | `observability/<app>.targets.yml` | the blackbox probes, with the labels `service` and `module` |
| `/etc/alloy/<app>.alloy` | `observability/<app>.alloy` | the journal and the access log, for Loki |

`<app>` is the systemd unit's name. Grafana's part is the same for every lith app: the host installs the dashboard and the alert rules once, from [`deploy/observability`](https://github.com/zweiundeins/lith/tree/main/deploy/observability) in lith's repository, whose README says how. The template's `deploy/` holds the files of the table for a new app, and this site's `site/deploy` its own. Each has a `setup-host.sh` that installs the three monitoring files, each only where its service is installed. It reloads Alloy, and puts the old file back if Alloy refuses the new one; Prometheus watches its folders and needs no reload. The Caddy block goes in by hand, once the domain's DNS records point at the host, since Caddy asks for the certificate as it loads the block.

### Labels

- `app`, on the metrics target: the unit's name, unique on the host. The dashboard picks apps by it, and the alerts name it.
- `slot`, on the metrics target: which instance of the app it is, `prod` for an app with one unit, `blue`, `green` or `staging` for slots. The dashboard filters by it, and the alerts fire for each app and slot.
- `service`, on each probe: the short name that the site's access log has as `site` (`lith` for this site), so that a dashboard can show an app's probes beside its log.
- `module`, on each probe: one of the host's generic blackbox modules, never one per app.
- `job` belongs to the host: `apps` for the metrics, its blackbox job for the probes. The dashboard and the alert rules match the metric names and `app`, never the job. In Loki, an app's journal is `{job="<app>", level="ERROR"}` and its access log `{job="caddy", site="<site>"}`.
- `level`, on each journal line: slog's level, `DEBUG`, `INFO`, `WARN` or `ERROR`.

### The host's side

Prometheus's configuration has a job for the apps' metrics and one for the probes, both reading the folders:

```yaml title=prometheus.yml
scrape_configs:
  - job_name: apps
    file_sd_configs:
      - files: ['/etc/prometheus/apps/*.yml']

  - job_name: blackbox_service # `blackbox` is the exporter's own scrape
    metrics_path: /probe
    file_sd_configs:
      - files: ['/etc/prometheus/targets/*.yml']
    relabel_configs:
      - source_labels: [module]
        target_label: __param_module
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: 127.0.0.1:9115
```

The blackbox exporter's modules are generic. `http_2xx_ok` is for lith's `/healthz`; a keyword on one app's page would take a module of its own, so the probes keep to these:

```yaml title=blackbox.yml
modules:
  http_2xx:
    prober: http
    timeout: 5s
    http: &http_2xx
      valid_status_codes: [200]
      preferred_ip_protocol: ip4
  http_2xx_ok:
    prober: http
    timeout: 5s
    http: { <<: *http_2xx, fail_if_body_not_matches_regexp: ['^ok '] }
  http_redirect:
    prober: http
    timeout: 5s
    http: { valid_status_codes: [301, 302, 303, 308], method: GET, no_follow_redirects: true }
```

Alloy runs on the folder, `alloy run /etc/alloy` (on Debian, `CONFIG_FILE="/etc/alloy"` in `/etc/default/alloy`), and combines every `.alloy` file in it, without subfolders, into one configuration. A component's label must therefore be unique across the files: an app's start with its name as an identifier (`lith_site` for this site), which lith-new writes from the template's `app_`. The host's own `config.alloy` defines the two components that the drop-ins send to: `loki.write "default"`, for the journal's lines, and `loki.process "caddy_logs"`, which takes Caddy's JSON access logs, labels them by status class and forwards them to Loki. Alloy's user reads the journal (the group `systemd-journal`) and `/var/log/caddy`. This site's access log:

```hcl source=site/deploy/observability/lith-site.alloy
local.file_match "lith_site_access" {
	path_targets = [{"__path__" = "/var/log/caddy/lith-access.log", "job" = "caddy", "site" = "lith"}]
}

loki.source.file "lith_site_access" {
	targets       = local.file_match.lith_site_access.targets
	forward_to    = [loki.process.caddy_logs.receiver]
	tail_from_end = true
}
```

Caddy's main file imports the sites, `import /etc/caddy/sites/*.caddy`, and Grafana has a Prometheus data source with the uid `prometheus-ds`, which the dashboard and the alert rules name.

### Metrics from the template

The template serves no metrics, so its `deploy/observability` has no `metrics.yml`. To add them: a registry with `lith/prom` and `lith/dbmetrics/prom` on a loopback port of its own ([A listener of its own](#a-listener-of-its-own)), a unit that lets Prometheus connect (the template's `IPAddressDeny=localhost` blocks loopback both ways; this site's unit allows localhost and binds only its two ports), and the target, labelled like this site's:

```yaml source=site/deploy/observability/lith-site.metrics.yml
- targets: ['127.0.0.1:8197']
  labels: { app: lith-site, slot: prod }
```
