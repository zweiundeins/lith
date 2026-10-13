# Deploying lith-site

lith's website (lith.zweiundeins.gmbh) is one binary and one SQLite file, behind Caddy, on the same host as the other apps. The files here:

- `lith-site.service`: the systemd unit. It runs as the `lith-site` user, keeps the database in `/var/lib/lith-site`, serves the site on `127.0.0.1:8097` and its metrics on `127.0.0.1:8197`, connects nowhere and may bind no other port. Memory: `MemoryHigh=224M`, `MemoryMax=256M`, `GOMEMLIMIT=192MiB`, at most 300 open streams.
- `lith-site.env.example`: optional settings, installed as `/etc/lith-site/lith-site.env`.
- `lith-site.caddy`: the site block. TLS, `reverse_proxy` to the loopback port with the client's address in X-Forwarded-For and X-Real-IP, the access log in JSON at `/var/log/caddy/lith-access.log`, no `encode`, and `flush_interval -1` for the streams.
- `lith-site-deploy`: installs a binary, restarts the unit, waits for `/healthz`, and puts the old binary back if the new one does not answer.
- `setup-host.sh`: creates the user that CI deploys as, whose key may run `lith-site-deploy -` and nothing else.
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

Caddy asks for the site's certificate when it loads the block, so lith.zweiundeins.gmbh's A and AAAA records must point at the host before the reload. The host's Caddyfile imports every file in `/etc/caddy/sites/`:

```sh
install -m 0644 lith-site.caddy /etc/caddy/sites/
caddy validate --config /etc/caddy/Caddyfile
sh -c 'chown caddy:caddy /var/log/caddy/*.log'
systemctl reload caddy
```

Run as root, `caddy validate` creates the access log owned by root and the reload then fails: the `chown` fixes it.

`-base` in the unit is the public URL: canonical links, the Open Graph image and the sitemap start with it. A move to another domain changes it, the Caddy block, the probe targets and the URL in the deploy workflow together. Another port changes `-addr`, `SocketBindAllow`, the Caddy block, `HEALTH` in `lith-site-deploy` and the Prometheus target; the metrics follow at the port + 100.

### Deploys from CI

The deploy workflow logs in as `lith-site-deploy`, a user whose key runs `sudo -n /usr/local/bin/lith-site-deploy -` and nothing else: no shell, no forwarding, and sudo allows that one command. The worst a leaked key can do is deploy a binary that passes the script's checks and runs as `lith-site`, which a merge to `main` does anyway.

1. Make a deploy key on your machine:

   ```sh
   ssh-keygen -t ed25519 -N "" -C "github-actions@zweiundeins/lith" -f lith-site-deploy-key
   ```

2. Copy `lith-site-deploy-key.pub` to the server and run, as root, from this folder:

   ```sh
   bash setup-host.sh lith-site-deploy-key.pub
   ```

   It installs `lith-site-deploy`, creates the user with that key and its sudoers line, and ends with `>> ready: CI can deploy as lith-site-deploy@…`. Running it again with another key replaces the old one.

3. Give GitHub the key and the host. The job's `if` runs before the environment's variables load, so the host and its host key are repository variables. The private key is a secret of the `production` environment, which only `main` may use:

   ```sh
   gh api -X PUT repos/zweiundeins/lith/environments/production --input - <<<'{"deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}'
   gh api -X POST repos/zweiundeins/lith/environments/production/deployment-branch-policies -f name=main -f type=branch
   gh secret set SITE_DEPLOY_SSH_KEY --env production -R zweiundeins/lith < lith-site-deploy-key
   ssh-keyscan -t ed25519 179.237.86.212 2>/dev/null | gh variable set SITE_DEPLOY_KNOWN_HOSTS -R zweiundeins/lith
   gh variable set SITE_DEPLOY_HOST -R zweiundeins/lith --body 179.237.86.212
   rm lith-site-deploy-key lith-site-deploy-key.pub
   ```

4. Try it: `gh workflow run deploy-site.yml -R zweiundeins/lith`, then `gh run watch -R zweiundeins/lith`.

Deleting the `SITE_DEPLOY_HOST` variable pauses the deploys. Deleting `/var/lib/lith-site-deploy/.ssh/authorized_keys` on the server takes CI's access away.

## Deploying

`main` deploys itself: when CI passes on a push to `main`, [`deploy-site.yml`](../../.github/workflows/deploy-site.yml) builds the binary with `go tool task build`, pipes it over SSH into `lith-site-deploy -`, and checks that `https://lith.zweiundeins.gmbh/healthz` answers with the new version. "Run workflow" on GitHub starts it by hand, from `main` only. Without the `SITE_DEPLOY_HOST` variable the workflow does nothing.

`lith-site-deploy <binary>` checks that the file is an ELF binary that prints its version (as the `lith-site` user, without network, for ten seconds at most), keeps the running binary aside, restarts the unit and waits up to 30 seconds for `/healthz` to answer `ok <new version>`. If it does not, it puts the old binary back, restarts again and exits non-zero, which fails the workflow. `lith-site-deploy rollback` returns to the binary that ran before the last good deploy.

On a restart every open page reconnects: the streams end with `sse.Reconnect`, and the pages' `retry: 'error'` carries them through the restart.

## Monitoring

- **Metrics:** Prometheus scrapes `127.0.0.1:8197`, never public. Add the target in `observability/prometheus-scrape.yml` to the `go_runtime` job; it lists the metrics: `lith_site_http_requests_total{route,code}`, `lith_site_http_request_duration_seconds{route}`, `lith_site_writer_*`, `lith_site_live_*`, `lith_site_sse_budget_*`, `lith_site_db_*`, `go_*` and `process_*`.
- **Probes:** `observability/lith-site.targets.yml` goes to `/etc/prometheus/targets/lith-site.yml`, and the `http_2xx_lith` module in `observability/blackbox-module.yml` into the blackbox exporter's `blackbox.yml`. The targets' `service` label is `lith`, the site's name in the access log's stream, so the host's Services dashboard shows the probes and the log under one name.
- **Dashboard:** `observability/grafana/lith-apps.json`, provisioned like the host's other dashboards. It shows any lith app: pick its metric namespace (`lith_site`), then the job and the app.
- **Alerts:** `observability/grafana/alerting/rules-lith-apps.yaml`: a 5xx ratio above 2%, tick overruns, dropped frames, writer failures and refused streams, for every app that runs lith's writer. A site that stops answering trips the host's rule on every blackbox probe. Hand-written to the host's format: import, check and export them over the file before relying on them.
- **Logs:** slog JSON on stdout, one object per line, into the journal. `observability/lith-site.alloy` is the block for the host's Alloy config that ships them to Loki as `{job="lith-site"}`, with `level` as a label like the other apps. Caddy's access log reaches Loki as `{job="caddy", site="lith"}`, from a path target in the same config.
- **Health:** `/healthz` answers `ok <version>` while the process serves; `/readyz` answers 200 while the database answers and the writer and the hub run, else 503 naming what failed.
