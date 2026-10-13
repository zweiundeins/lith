# Deploying lith-site

lith's website (lith.zweiundeins.gmbh) is one binary and one SQLite file, behind Caddy, on the same host as the other apps. The files here:

- `lith-site.service`: the systemd unit. It runs as the `lith-site` user, keeps the database in `/var/lib/lith-site`, serves the site on `127.0.0.1:8097` and its metrics on `127.0.0.1:8197`, connects nowhere and may bind no other port. Memory: `MemoryHigh=224M`, `MemoryMax=256M`, `GOMEMLIMIT=192MiB`, at most 300 open streams.
- `lith-site.env.example`: optional settings, installed as `/etc/lith-site/lith-site.env`.
- `lith-site.caddy`: the site block. TLS, `reverse_proxy` to the loopback port with the client's address in X-Forwarded-For and X-Real-IP, the access log in JSON at `/var/log/caddy/lith-access.log`, no `encode`, and `flush_interval -1` for the streams.
- `lith-site-deploy`: installs a binary, restarts the unit, waits for `/healthz`, and puts the old binary back if the new one does not answer.
- `observability/`: the blackbox probes, the Prometheus target, a Grafana dashboard for lith apps and their alert rules (below).

The database holds the demos' data only, which the sweep keeps small: it needs no backup.

## One-time setup

As root on the server, with the binary from `go tool task build` and this folder copied over:

```sh
useradd --system --home /var/lib/lith-site --shell /usr/sbin/nologin lith-site
install -m 0644 lith-site.service /etc/systemd/system/
install -D -m 0640 -g lith-site lith-site.env.example /etc/lith-site/lith-site.env
install -m 0755 lith-site-deploy /usr/local/bin/
systemctl daemon-reload
systemctl enable lith-site.service
lith-site-deploy ./lith-site
```

Then add `lith-site.caddy` to Caddy's sites and reload Caddy. Run as root, `caddy validate` creates the access log owned by root and the reload then fails: `sh -c 'chown caddy:caddy /var/log/caddy/*.log'` fixes it.

`-base` in the unit is the public URL: canonical links, the Open Graph image and the sitemap start with it. A move to another domain changes it and the Caddy block together. Another port changes `-addr`, `SocketBindAllow`, the Caddy block, `HEALTH` in `lith-site-deploy` and the Prometheus target; the metrics follow at the port + 100.

## Deploying

`lith-site-deploy <binary>` checks that the file is an ELF binary that prints its version (as the `lith-site` user, without network, for ten seconds at most), keeps the running binary aside, restarts the unit and waits up to 30 seconds for `/healthz` to answer `ok <new version>`. If it does not, it puts the old binary back and restarts again. `lith-site-deploy rollback` returns to the binary that ran before the last good deploy.

On a restart every open page reconnects: the streams end with `sse.Reconnect`, and the pages' `retry: 'error'` carries them through the restart.

CI can deploy by piping the binary over SSH into `lith-site-deploy -`, as a user whose key may run that one command only (`restrict,command="sudo -n /usr/local/bin/lith-site-deploy -"` and a sudoers line for exactly that command). Starbase's `deploy/setup-host.sh` creates such a user.

## Monitoring

- **Metrics:** Prometheus scrapes `127.0.0.1:8197`, never public. Add the target in `observability/prometheus-scrape.yml` to the `go_runtime` job; it lists the metrics: `lith_site_http_requests_total{route,code}`, `lith_site_http_request_duration_seconds{route}`, `lith_site_writer_*`, `lith_site_live_*`, `lith_site_sse_budget_*`, `lith_site_db_*`, `go_*` and `process_*`.
- **Probes:** `observability/lith-site.targets.yml` goes to `/etc/prometheus/targets/lith-site.yml`, and the `http_2xx_lith` module in `observability/blackbox-module.yml` into the blackbox exporter's `blackbox.yml`.
- **Dashboard:** `observability/grafana/lith-apps.json`, provisioned like the host's other dashboards. It shows any lith app: pick its metric namespace (`lith_site`), then the job and the app.
- **Alerts:** `observability/grafana/alerting/rules-lith-apps.yaml`: lith-site down (probe), a 5xx ratio above 2%, tick overruns, dropped frames, writer failures and refused streams, for every app that runs lith's writer. Hand-written to the host's format: import, check and export them over the file before relying on them.
- **Logs:** slog JSON on stdout, one object per line, into the journal. `observability/lith-site.alloy` is the block for the host's Alloy config that ships them to Loki as `{job="lith-site"}`, with `level` as a label like the other apps.
- **Health:** `/healthz` answers `ok <version>` while the process serves; `/readyz` answers 200 while the database answers and the writer and the hub run, else 503 naming what failed.
