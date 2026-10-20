# Deploying lith-site

lith's website (lith.zweiundeins.gmbh) is one binary and one SQLite file, behind Caddy, on the same host as the other apps. The files here:

- `lith-site.service`: the systemd unit. It runs as the `lith-site` user, keeps the database in `/var/lib/lith-site`, serves the site on the Unix socket `/run/lith-site/lith-site.sock` (mode 0660, in `/run/lith-site` with mode 0750: only the `lith-site` group gets in, which Caddy's user joins) and its metrics on `127.0.0.1:8197`, connects nowhere and may bind no other port. Memory: `MemoryHigh=224M`, `MemoryMax=256M`, `GOMEMLIMIT=192MiB`, at most 300 open streams. `CPUWeight=50` lets the host's other apps go first when the cores are busy.
- `lith-site.env.example`: optional settings, installed as `/etc/lith-site/lith-site.env`.
- `lith-site.caddy`: the site block. TLS, `reverse_proxy` to the socket with the client's address in X-Forwarded-For and X-Real-IP, request bodies of 1 MB at most, the access log in JSON at `/var/log/caddy/lith-access.log` with a hash in place of each tab id, no `encode`, and `flush_interval -1` for the streams.
- `lith-site-deploy`: installs a binary, restarts the unit, waits for `/healthz` (on the socket, or on `127.0.0.1:8097` while the unit from before the socket runs), and puts the old binary back if the new one does not answer.
- `observability/`: the Prometheus target, the blackbox probes and the Alloy file, for the folders the host's Prometheus and Alloy load ([Monitoring](#monitoring)). The Grafana dashboard for lith apps and their alert rules are in `deploy/observability` at lith's root.
- `setup-host.sh`: puts Caddy's user in the `lith-site` group, installs the files in `observability/`, and with a key, creates the user that CI deploys as, whose key may run `lith-site-deploy -` and nothing else.

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
bash setup-host.sh
```

Run as root, `caddy validate` creates the access log owned by root and the reload then fails: the `chown` fixes it. `setup-host.sh` puts the `caddy` user in the `lith-site` group and installs the monitoring files ([Monitoring](#monitoring)). Restart Caddy once after that (`systemctl restart caddy`): a reload keeps the groups it started with, and without the group it cannot reach the socket.

### Whom the site believes

The rate limits and the cap on streams per client (`-client-streams`, 16) count by the client's address, which the site takes from X-Forwarded-For when the request comes through the socket or loopback, and from the peer otherwise. It logs at start whose headers it believes (`proxy_headers_from`). On the socket that is the `lith-site` group: Caddy's user, and no one else. On a loopback port every local user of the host may connect and name any address, to dodge a limit or spend someone else's, which is why the unit uses the socket.

### Moving from the loopback port to the socket

A host set up before the socket runs the site on `127.0.0.1:8097`. CI deploys only the binary, which listens wherever the unit says, so nothing changes until the files here are installed, together, as root from this folder:

```sh
bash setup-host.sh                    # caddy joins lith-site; lith-site-deploy is reinstalled only with a key
install -m 0755 lith-site-deploy /usr/local/bin/
install -m 0644 lith-site.service /etc/systemd/system/
systemctl daemon-reload
systemctl restart lith-site           # now on /run/lith-site/lith-site.sock
install -m 0644 lith-site.caddy /etc/caddy/sites/
caddy validate --config /etc/caddy/Caddyfile
systemctl restart caddy               # a restart, for the new group
```

The new `lith-site-deploy` asks the socket when it is there and the port otherwise, so either order of deploys works.

`-base` in the unit is the public URL: canonical links, the Open Graph image and the sitemap start with it. A move to another domain changes it, the Caddy block, the probe targets and the URL in the deploy workflow together. Another metrics port changes `-metrics`, `SocketBindAllow` and `observability/lith-site.metrics.yml`.

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

   It installs `lith-site-deploy`, creates the user with that key and its sudoers line, says `>> ready: CI can deploy as lith-site-deploy@…`, and installs the monitoring files. Running it again with another key replaces the old one.

3. Give GitHub the key and the host. The job's `if` runs before the environment's variables load, so the host and its host key are repository variables. The private key is a secret of the `production` environment, which only `main` may use:

   ```sh
   HOST=<the server's address>
   gh api -X PUT repos/zweiundeins/lith/environments/production --input - <<<'{"deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}'
   gh api -X POST repos/zweiundeins/lith/environments/production/deployment-branch-policies -f name=main -f type=branch
   gh secret set SITE_DEPLOY_SSH_KEY --env production -R zweiundeins/lith < lith-site-deploy-key
   ssh-keyscan -t ed25519 "$HOST" 2>/dev/null | gh variable set SITE_DEPLOY_KNOWN_HOSTS -R zweiundeins/lith
   gh variable set SITE_DEPLOY_HOST -R zweiundeins/lith --body "$HOST"
   rm lith-site-deploy-key lith-site-deploy-key.pub
   ```

4. Try it: `gh workflow run deploy-site.yml -R zweiundeins/lith`, then `gh run watch -R zweiundeins/lith`.

Deleting the `SITE_DEPLOY_HOST` variable pauses the deploys. Deleting `/var/lib/lith-site-deploy/.ssh/authorized_keys` on the server takes CI's access away.

## Deploying

`main` deploys itself: when CI passes on a push to `main`, [`deploy-site.yml`](../../.github/workflows/deploy-site.yml) builds the binary with `go tool task build`, pipes it over SSH into `lith-site-deploy -`, and checks that `https://lith.zweiundeins.gmbh/healthz` answers with the new version. "Run workflow" on GitHub starts it by hand, from `main` only. Without the `SITE_DEPLOY_HOST` variable the workflow does nothing.

`lith-site-deploy <binary>` checks that the file is an ELF binary that prints its version (as the `lith-site` user, without network, for ten seconds at most), keeps the running binary aside, restarts the unit and waits up to 30 seconds for `/healthz` to answer `ok <new version>`. If it does not, it puts the old binary back, restarts again and exits non-zero, which fails the workflow. `lith-site-deploy rollback` returns to the binary that ran before the last good deploy.

On a restart every open page reconnects: the streams end with `sse.Reconnect`, and the pages' `retry: 'error'` carries them through the restart.

## Monitoring

The host keeps its own configs for Caddy, Prometheus, Alloy and Grafana, each of which loads a folder of files the apps install: lith's guide [Metrics and monitoring](https://lith.zweiundeins.gmbh/docs/guides/monitoring#drop-in-files-for-a-shared-host) has the folders and the labels. `setup-host.sh` installs the site's, each only where its service is installed, and reloads Alloy; run it again after changing them.

- **Metrics:** `observability/lith-site.metrics.yml` goes to `/etc/prometheus/apps/lith-site.yml`: Prometheus scrapes `127.0.0.1:8197`, never public, as `{app="lith-site", slot="prod"}`. The file lists the metrics: `lith_site_http_requests_total{route,code}`, `lith_site_http_request_duration_seconds{route}`, `lith_site_writer_*`, `lith_site_live_*`, `lith_site_sse_budget_*`, `lith_site_db_*`, `go_*` and `process_*`.
- **Probes:** `observability/lith-site.targets.yml` goes to `/etc/prometheus/targets/lith-site.yml`: `/` and `/docs` with the host's `http_2xx` module, `/healthz` with `http_2xx_ok`, whose body must start with `ok `. The targets' `service` label is `lith`, the site's name in the access log's stream, so the host's Services dashboard shows the probes and the log under one name.
- **Dashboard:** [`deploy/observability/lith-apps.json`](../../deploy/observability) at lith's root, which the host installs once for every lith app. It shows any lith app: pick its metric namespace (`lith_site`), then the app and the slot.
- **Alerts:** `rules-lith-apps.yaml`, beside the dashboard: a 5xx ratio above 2%, tick overruns, dropped frames, writer failures and refused streams, for every app that runs lith's writer. A site that stops answering trips the host's rule on every blackbox probe. Hand-written to Grafana's format: import, check and export them over the file before relying on them.
- **Logs:** slog JSON on stdout, one object per line, into the journal. `observability/lith-site.alloy` goes to `/etc/alloy/lith-site.alloy` and ships them to Loki as `{job="lith-site"}`, with `level` as a label like the other apps, and Caddy's access log as `{job="caddy", site="lith"}`, through the host's pipeline for access logs.
- **Health:** `/healthz` answers `ok <version>` while the process serves; `/readyz` answers 200 while the database answers and the writer and the hub run, else 503 naming what failed.
