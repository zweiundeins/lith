# Deploying app

One binary and one SQLite file on a Linux server, behind Caddy. The files here:

- `app.service`: the systemd unit. It runs as the `app` user, keeps the database in `/var/lib/app`, listens on the Unix socket `/run/app/app.sock` (mode 0660, in `/run/app` with mode 0750: only the `app` group gets in, which Caddy's user joins), serves its metrics on `127.0.0.1:8180`, reaches no other host and binds no other port, and is capped at 640 MB of memory.
- `app-backup.service` and `app-backup.timer`: every six hours, `app backup` copies the database (VACUUM INTO) into `/var/lib/app/backups` and keeps the newest 28 copies.
- `app.caddy`: the site block. TLS, `reverse_proxy` to the socket with the client's address in X-Forwarded-For and X-Real-IP, request bodies of 1 MB at most, the access log in JSON with a hash in place of each tab id, no `encode`, and `flush_interval -1` for the streams.
- `app-deploy`: installs a binary, restarts the unit, waits for `/healthz`, and puts the old binary back if the new one does not answer.
- `observability/`: the Prometheus target of the metrics, the blackbox probes, and the Alloy file that ships the journal and the access log to Loki ([Monitoring](#monitoring)).
- `setup-host.sh`: puts Caddy's user in the `app` group, installs the files in `observability/` into the folders the host's Prometheus and Alloy load, and with a key, creates the user CI deploys as.

## One-time setup

As root on the server, with the binary from `go tool task build` and this folder copied over:

```sh
useradd --system --home /var/lib/app --shell /usr/sbin/nologin app
install -m 0644 app.service app-backup.service app-backup.timer /etc/systemd/system/
install -m 0755 app-deploy /usr/local/bin/
systemctl daemon-reload
systemctl enable app.service app-backup.timer
app-deploy ./app
systemctl start app-backup.timer
```

Put the real domain into `app.caddy` and `observability/app.targets.yml`, point its DNS records at the host (Caddy asks for the certificate when it loads the block), and install the block where the host's Caddyfile imports it:

```sh
install -m 0644 app.caddy /etc/caddy/sites/
caddy validate --config /etc/caddy/Caddyfile
chown caddy:caddy /var/log/caddy/app-access.log
systemctl reload caddy
bash setup-host.sh
```

Run as root, `caddy validate` creates the access log owned by root, and the reload then fails: the `chown` fixes it. `setup-host.sh` puts the `caddy` user in the `app` group and installs the monitoring files (below). Restart Caddy once after that (`systemctl restart caddy`): a reload keeps the groups it started with, and without the group it cannot reach the socket.

### Whom the app believes

The rate limits and the cap on streams per client count by the client's address, which the app takes from X-Forwarded-For when the request comes through the socket or loopback, and from the peer otherwise. It logs at start whose headers it believes (`proxy_headers_from`). On the socket that is the `app` group, so the host needs Caddy's user in it and no other user there. On a loopback port (`-addr 127.0.0.1:8080`) every local user may connect and name any address, to dodge a limit or spend someone else's: keep the socket on a host that runs other people's code. A proxy that connects from another address, such as a container bridge, is not believed, and every client then counts as the proxy.

The app's server gives a request's headers and body 15 s on every route (`ReadTimeout`), and an upload's handler more while the body keeps coming (`edge.UploadBody`), so a client that declares a body and trickles it holds none of the app's connections longer; streams live on. Behind Caddy, such a client still holds Caddy's connection to the client for as long as Caddy allows. Caddy's server-wide `read_body` timeout would close it, and does not end streams either (checked with Caddy 2.10.2), but it would have to be longer than the host's slowest honest upload, and Caddy's per-route `request_body { read_timeout }` is experimental, so lith recommends neither and leaves Caddy's timeouts to the host. Caddy's HTTP/3 server sets no read timeout at all.

## Deploying

`app-deploy <binary>` checks that the file is an ELF binary that prints its version (it runs `--version` as the `app` user, without network, for ten seconds at most), keeps the running binary aside, restarts `app` and waits up to 30 seconds for `/healthz` to answer `ok <new version>`. If it does not, the script puts the old binary back and restarts again. `app-deploy rollback` returns to the binary that ran before the last good deploy.

CI can deploy by piping the binary over SSH into `app-deploy -`, as a user whose key may run that one command only: `restrict,command="sudo -n /usr/local/bin/app-deploy -"` in its `authorized_keys`, and a sudoers line that allows exactly that command. `setup-host.sh` creates such a user when given the public half of CI's key: run it as root, from a copy of this folder (`bash setup-host.sh app-deploy-key.pub`). lith's `.github/workflows/deploy-site.yml` is a workflow that deploys this way.

## Monitoring

A host that runs several apps keeps its own configs for Caddy, Prometheus, Alloy and Grafana, and each loads a folder of files that the apps install. lith's guide [Metrics and monitoring](https://lith.zweiundeins.gmbh/docs/guides/monitoring#drop-in-files-for-a-shared-host) describes the folders and the labels. `setup-host.sh` installs app's:

- `observability/app.metrics.yml` as `/etc/prometheus/apps/app.yml`: Prometheus scrapes `127.0.0.1:8180`, never public, labelled with app's name and `slot: prod`. The file lists the metrics: `app_writer_*`, `app_live_*`, `app_sse_budget_*`, `app_db_*`, `go_*` and `process_*`. Prometheus watches the folder.
- `observability/app.targets.yml` as `/etc/prometheus/targets/app.yml`: blackbox probes of `/` and `/healthz` (whose body must start with `ok `), labelled `service: app`. Prometheus watches the folder.
- `observability/app.alloy` as `/etc/alloy/app.alloy`: the journal as `{job="app"}`, with slog's level as the label `level`, and Caddy's access log as `{job="caddy", site="app"}`. The script reloads Alloy and puts the old file back if Alloy refuses the new one.

Each goes in only where its service is installed; run the script again after changing them.

The host's "lith apps" dashboard and alert rules show app once Prometheus scrapes it. Each app on a host needs a metrics port of its own: if 8180 is taken, change it in `app.service` (`-metrics` and `SocketBindAllow`) and in `observability/app.metrics.yml`. For Prometheus to connect, the unit opens loopback both ways, so app could also reach the other services that listen there (Caddy's admin API, databases); it reaches no other host. An app that calls out adds the addresses it needs to `IPAddressAllow`.

## Backups

The copies stay on the server's disk, so copy `/var/lib/app/backups` off the machine as well (restic, rsync). To restore one, stop `app`, delete `app.db-wal` and `app.db-shm` in `/var/lib/app`, put the copy in place with `install -o app -g app -m 0600 <copy> /var/lib/app/app.db`, and start `app`.

## Litestream with two slots

For a continuous replica in object storage, and a rollback that is a cutover instead of a reinstall, an app can run two slots of one unit (`app@blue`, `app@green`) on one data directory. Only one slot runs at a time, since Litestream allows one replicator per database. lith's `backup` module restores a missing database before `sqlite.Open` and replicates while the process runs; the database is then opened with `sqlite.CheckpointExternal`. Caddy proxies to both ports with `lb_try_duration` and `health_uri /readyz`, so requests wait through the few seconds of a handoff.
