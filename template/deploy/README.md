# Deploying app

One binary and one SQLite file on a Linux server, behind Caddy. The files here:

- `app.service`: the systemd unit. It runs as the `app` user, keeps the database in `/var/lib/app`, listens on the Unix socket `/run/app/app.sock`, cannot reach localhost, and is capped at 640 MB of memory.
- `app-backup.service` and `app-backup.timer`: every six hours, `app backup` copies the database (VACUUM INTO) into `/var/lib/app/backups` and keeps the newest 28 copies.
- `Caddyfile`: the site block. TLS, `reverse_proxy` to the socket, no `encode`, and `flush_interval -1` for the streams.
- `app-deploy`: installs a binary, restarts the unit, waits for `/healthz`, and puts the old binary back if the new one does not answer.

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

Then add the block in `Caddyfile` to Caddy's configuration with the real domain, and reload Caddy. Run as root, `caddy validate` creates the access log owned by root, and the reload then fails; `chown caddy:caddy /var/log/caddy/app-access.log` fixes it.

## Deploying

`app-deploy <binary>` checks that the file is an ELF binary that prints its version (it runs `--version` as the `app` user, without network, for ten seconds at most), keeps the running binary aside, restarts `app` and waits up to 30 seconds for `/healthz` to answer `ok <new version>`. If it does not, the script puts the old binary back and restarts again. `app-deploy rollback` returns to the binary that ran before the last good deploy.

CI can deploy by piping the binary over SSH into `app-deploy -`, as a user whose key may run that one command only: `restrict,command="sudo -n /usr/local/bin/app-deploy -"` in its `authorized_keys`, and a sudoers line that allows exactly that command. Starbase's `deploy/setup-host.sh` creates such a user.

## Backups

The copies stay on the server's disk, so copy `/var/lib/app/backups` off the machine as well (restic, rsync). To restore one, stop `app`, delete `app.db-wal` and `app.db-shm` in `/var/lib/app`, put the copy in place with `install -o app -g app -m 0600 <copy> /var/lib/app/app.db`, and start `app`.

## Litestream with two slots

For a continuous replica in object storage, and a rollback that is a cutover instead of a reinstall, libretto and swisscyberguard run two slots of one unit (`app@blue`, `app@green`) on one data directory. Only one slot runs at a time, since Litestream allows one replicator per database. lith's `backup` module restores a missing database before `sqlite.Open` and replicates while the process runs; the database is then opened with `sqlite.CheckpointExternal`. Caddy proxies to both ports with `lb_try_duration` and `health_uri /readyz`, so requests wait through the few seconds of a handoff. The units, the Caddyfile and the deploy script are in those projects' `deploy/` folders.
