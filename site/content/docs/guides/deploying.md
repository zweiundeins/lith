---
title: Deploying
headline: Deploy a lith app: one binary, SQLite and Caddy
summary: One static binary and one SQLite file behind Caddy. The template's systemd unit, Caddy block and deploy script with a health check and rollback, the memory settings to change together, and what a restart does to open pages.
description: One static binary and one SQLite file behind Caddy: the template's systemd unit, a deploy script with health check and rollback, and the memory settings.
order: 12
---

A lith app ships as one static binary and keeps its state in one SQLite file. The template's `deploy/` folder holds what runs it on a Linux host: a systemd unit, a Caddy site block, a deploy script, a timer for backups, and the probes and log shipping for the host's monitoring. This site runs on its own files of the same kind, in `site/deploy`.

## The binary

`go tool task build` builds it with `CGO_ENABLED=0` (SQLite is pure Go), `-trimpath`, and the version from `git describe` in `main.version`. `--version` prints it, `/healthz` answers with it, and the deploy script compares it. Everything the app serves is embedded: templates, static files, migrations.

## The systemd unit

The unit runs the binary as an unprivileged user, keeps the database in `/var/lib/app` (`StateDirectory`), listens on a Unix socket that Caddy reaches, and serves its metrics on 127.0.0.1:8180 for the host's Prometheus:

```ini source=template/deploy/app.service
ExecStart=/opt/app/app serve \
	-addr unix:/run/app/app.sock \
	-metrics 127.0.0.1:8180 \
	-db /var/lib/app/app.db \
	-secure \
	-streams 200
Restart=always
RestartSec=1
# SIGTERM: the streams end (pages reconnect to the next process), actions
# finish, the writer commits what is queued.
KillSignal=SIGTERM
TimeoutStopSec=20
LimitNOFILE=65536
```

The app believes the `X-Forwarded-For` of whoever connects to the socket ([The request edge](/docs/guides/edge#client-addresses)), so only Caddy may: the unit makes `/run/app` mode 0750 (`RuntimeDirectoryMode`), the app makes the socket 0660, both of the group `app`, and the host puts Caddy's user in that group (`usermod -aG app caddy`, which `setup-host.sh` does), then restarts Caddy once, since a reload keeps the groups it started with. The app logs at start whose headers it believes (`proxy_headers_from`). On a loopback port every local user of the host could connect and name any client address.

The rest of the unit closes the process in: it reaches no host but its own, may bind its metrics port and no other, and runs with a read-only system, private `/tmp` and devices, no capabilities, and the `@system-service` system calls. Loopback is open both ways, since Prometheus connects from 127.0.0.1, so the app could also reach the other services that listen there; an app that calls out to another host adds the addresses it needs to `IPAddressAllow` ([Metrics and monitoring](/docs/guides/monitoring#metrics-from-the-template)). Each flag of the binary can also come from the environment, `-db` as `APP_DB`, through an optional `/etc/app/app.env`.

## Memory

An open stream holds its encoder's window for as long as the tab stays open, so the number of tabs, the memory limit and the streams' budget belong together. The template:

```ini source=template/deploy/app.service
Environment=GOMEMLIMIT=448MiB
MemoryHigh=512M
MemoryMax=640M
TasksMax=256
```

- `GOMEMLIMIT` makes Go's garbage collector work harder as the heap nears it, before systemd's `MemoryHigh` throttles the process and `MemoryMax` ends it.
- The hub's budget gives the streams half of `GOMEMLIMIT`, 224 MiB here, for their encoders, 32 KiB each and the views' frames. That holds about 129 streams at zstd 2^19, and about 250 once the budget moves them to 2^18 with low memory, less what the frames take ([Streams and compression](/docs/guides/streams#the-memory-budget)). The other half is for the rest of the process and for the garbage between two collections.
- `-streams 200` caps the open tabs: the 201st gets a 503, which its page retries.

This site runs in less, beside other apps on its host:

```ini source=site/deploy/lith-site.service
# Memory. The live hub gives the streams half of GOMEMLIMIT, 96 MiB, for
# their encoders, 32 KiB each and the views' frames: about 55 streams at
# zstd 2^19, about 125 once the budget moves them down to 2^17 with low
# memory, then uncompressed (a guestbook frame is a few KB). An idle stream gives its encoder back after a minute (Hibernate).
# The rest is the process: Go, the page caches of the SQLite connections at
# 4 MiB at most and their prepared statements at 0.5 MiB at most, the
# static files. -streams caps the open tabs. Change these together.
Environment=GOMEMLIMIT=192MiB
MemoryHigh=224M
MemoryMax=256M
TasksMax=128

# CPU: half the weight of the host's other units, so that a crowd on the
# demos cannot slow the apps beside it down when the cores are busy.
CPUWeight=50
```

With 200 open streams it held 110 MB resident, 100 MB of them encoders: 115 streams at zstd 2^18 with low memory, the rest uncompressed (measured with windows down to 2^18). The budget's 96 MiB cover the encoders, 32 KiB for each stream and the frames the hub keeps for the views.

## Caddy

Caddy terminates TLS and proxies to the socket. The block is `deploy/app.caddy`, installed as `/etc/caddy/sites/app.caddy`, a folder the host's Caddyfile imports. The app compresses its pages, files and streams itself, so the block has no `encode`:

```text source=template/deploy/app.caddy
reverse_proxy unix//run/app/app.sock {
	# The client's address for the rate limits. app takes it only from
	# the socket or loopback, the last X-Forwarded-For address before
	# X-Real-IP, and a client cannot set either: these replace them.
	header_up X-Forwarded-For {http.request.remote.host}
	header_up X-Real-IP {http.request.remote.host}
	# Server-sent events: pass every write through at once.
	flush_interval -1
}
```

`flush_interval -1` passes every write of a stream through at once; without it, Caddy buffers frames. The block caps request bodies at 1 MB (`request_body { max_size 1MB }`). A client that trickles its body still holds Caddy's connection to the client for as long as Caddy allows; the app frees its own side with its `ReadTimeout` and, for an upload, `edge.UploadBody`. Caddy's server-wide `read_body` would have to be longer than the host's slowest honest upload, and its per-route `request_body { read_timeout }` is experimental, so lith recommends neither and leaves Caddy's timeouts to the host; Caddy's HTTP/3 server sets no read timeout at all ([Request bodies](/docs/guides/edge#request-bodies)). The access log is JSON, in `/var/log/caddy`, with mode 0640, and keeps a hash of each tab id instead of the id: a tab id lets whoever has it pace or end that tab's stream, and the log keeps 30 days. Run as root, `caddy validate` creates the log owned by root and the next reload fails; `chown caddy:caddy` on the file fixes it.

## The deploy script

`app-deploy <binary>` installs a new binary and keeps the old one for a rollback:

1. It checks that the file is an ELF binary, and runs `--version` as the app's user, without network, for ten seconds at most.
2. It keeps the running binary aside, moves the new one in place, and restarts the unit.
3. It waits up to 30 seconds for `/healthz` to answer `ok <new version>`.
4. If the new version does not answer, it puts the old binary back and restarts again.

```bash source=template/deploy/app-deploy
# The running binary becomes $NAME.prev only once the new one is healthy, so a
# failed deploy leaves the rollback target alone.
[[ -x "$DIR/$NAME" ]] && cp -f "$DIR/$NAME" "$DIR/$NAME.running"
mv -f "$DIR/$NAME.new" "$DIR/$NAME"
systemctl restart "$NAME"
if healthy "$NEW"; then
	[[ -f "$DIR/$NAME.running" ]] && mv -f "$DIR/$NAME.running" "$DIR/$NAME.prev"
	echo ">> healthy: $NEW"
	exit 0
fi
```

`app-deploy rollback` returns to the binary that ran before the last good deploy. CI deploys by piping the binary over SSH into `app-deploy -`, as a user whose key may run that one command only: `restrict,command="sudo -n /usr/local/bin/app-deploy -"` in its `authorized_keys`, and a sudoers line that allows exactly that command.

A migration runs when the new binary opens the database, and an older binary refuses the migrated database: `sqlite.Open` runs no program on a schema newer than its newest file. So the script's rollback works across deploys without a migration. Before a deploy that migrates, take a copy with the binary's `backup` command: rolling back past a migration means the old binary and that copy.

## A restart, seen from a page

On `SIGTERM` the app stops in an order that loses no action and strands no page:

```go source=template/cmd/app/main.go
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
```

1. The hub stops: every stream's last event is `sse.Reconnect`, and the page opens its stream again, with `retry: 'error'` carrying it through the seconds without a process.
2. `srv.Shutdown` lets running requests finish. Actions that arrive meanwhile still commit: the writer runs on a context of its own.
3. `w.Close()` applies the queue and runs `PRAGMA optimize`.
4. `db.Close()` closes both pools.

`TimeoutStopSec=20` covers that: the streams end at once, requests get ten seconds. The new process sends each reconnected page every view's current frame, so a page shows what changed during the restart without a reload. [Pages and Datastar](/docs/guides/pages#reconnecting-after-a-deploy) has the page's side.

The server's base context must not be the signal's context: request contexts would end with the signal, before the hub stops, and the streams would end without `sse.Reconnect`. Chrome takes a stream cut that way for a clean end, so even `retry: 'error'` would not reopen it.

## The host's folders

On a host that runs several apps, each app puts its files into folders that the host's services load, and never edits the host's own configuration: the Caddy block into `/etc/caddy/sites/`, its probes and its metrics target into Prometheus's folders, its log shipping into Alloy's. `setup-host.sh` installs the monitoring files, and [Metrics and monitoring](/docs/guides/monitoring#drop-in-files-for-a-shared-host) lists the folders and the labels. Each app needs a metrics port of its own: the template's is 8180, in the unit's `-metrics` and `SocketBindAllow` and in `observability/app.metrics.yml`.

## This site's deploy

`site/deploy` is a second, complete example: the unit listens on a socket of its group, as the template's does, its metrics on 127.0.0.1:8197 for Prometheus only, and the unit may connect nowhere and bind that port alone; its README says how a host moves it from the loopback port it used before. Its README installs it, and `site/deploy/observability` holds its probes and log shipping, beside the dashboard and the alerts in lith's `deploy/observability` ([Metrics and monitoring](/docs/guides/monitoring)).
