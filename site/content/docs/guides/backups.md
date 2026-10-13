---
title: Backups
summary: A consistent copy with VACUUM INTO on a timer, or continuous replication to S3 with Litestream and lith's backup module. How to wire each, check a copy, and restore.
order: 9
---

A copy of a SQLite file in WAL mode made with `cp` is not a backup: the copy can catch the database file between two checkpoints, without the commits that are still in the `-wal` file, or halfway through one. lith offers two ways that are: a copy with `VACUUM INTO`, and continuous replication with Litestream.

## Copies with VACUUM INTO

`db.Backup(ctx, path)` writes a consistent copy of the database to a new file with `VACUUM INTO`, on a read-only connection of its own, while the app keeps running. The copy is compacted and holds every commit up to the moment it began. The template's binary has a `backup` command that makes one and keeps the newest 28:

```go source=template/cmd/app/main.go
	ctx := context.Background()
	db, err := sqlite.Open(ctx, *dbPath, sqlite.Options{Readers: 1})
	if err != nil {
		return err
	}
	defer db.Close()
	to := filepath.Join(*dir, "app-"+time.Now().UTC().Format("20060102T150405Z")+".db")
	if err := db.Backup(ctx, to); err != nil {
		return err
	}
```

A systemd timer runs it every six hours, so 28 copies cover a week:

```ini source=template/deploy/app-backup.timer
[Timer]
OnCalendar=*-*-* 00/6:17:00
Persistent=true
```

The copies sit on the server's disk, which a lost disk takes with them. Copy the directory off the machine as well, with restic or rsync. A copy holds what was committed when it began: with copies every six hours, a lost disk loses up to six hours.

To restore one, stop the app, delete the database's `-wal` and `-shm` files, put the copy in its place with the app's owner and mode, and start the app again. The template's [deploy README](https://github.com/zweiundeins/lith/blob/main/template/deploy/README.md) has the commands.

## Checking a copy

A copy that nobody restored is a hope. Open it read-only and run SQLite's integrity check: read-only writes nothing, and a missing file is an error rather than a new, empty database.

```go source=site/internal/docs/example/board/store.go
func Check(ctx context.Context, path string) error {
	db, err := sqlite.Open(ctx, path, sqlite.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.R.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("%s: %s", path, result)
	}
	return nil
}
```

## Replication with Litestream

The `backup` module embeds [Litestream](https://litestream.io) in the app's process: it copies every commit to S3-compatible storage within seconds, keeps snapshots, and restores the database to any moment of its retention window, 30 days by default. It is a module of its own, `github.com/zweiundeins/lith/backup`, so an app without it never pulls in Litestream and the AWS SDK. Without a bucket (or a local `Dir`), the manager is disabled and every call does nothing, so development runs without credentials.

The order of a start and of a stop matters, so the board app's wiring is a program of its own, in a module of its own:

```go source=site/internal/docs/example/backup/main.go
	// 1. Before the database opens: a fresh disk gets the replica's state.
	if err := m.RestoreIfMissing(ctx, "app", path); err != nil {
		return err
	}
	// 2. Litestream checkpoints at positions it has replicated. Without it
	// the writer does, or the WAL would grow without end.
	checkpoint := sqlite.CheckpointWriter
	if m.Enabled() {
		checkpoint = sqlite.CheckpointExternal
	}
	db, err := sqlite.Open(ctx, path, sqlite.Options{Checkpoint: checkpoint})
	if err != nil {
		return err
	}
	w, err := writer.New(db, writer.Options{})
	if err != nil {
		return errors.Join(err, db.Close())
	}
	go w.Run(context.Background())
	// 3. Replicate. Attach returns once the first sync reached the replica.
	detach, err := m.Attach(ctx, "app", path)
	if err != nil {
		return errors.Join(err, w.Close(), db.Close())
	}
```

1. `RestoreIfMissing` runs before the database opens: opening creates an empty file, and a restore that finds a file does nothing. On a fresh disk it restores the replica's latest state; with nothing replicated yet, it does nothing.
2. With Litestream attached, the database opens with `sqlite.CheckpointExternal`: Litestream checkpoints at positions it has copied. One owner for checkpoints; without Litestream the writer has them.
3. `Attach` starts replicating and returns once the first sync has reached the replica.

The configuration comes from the environment, `BACKUP_BUCKET`, `BACKUP_REGION`, `BACKUP_ACCESS_KEY_ID` and the rest, with `backup.ConfigFromEnv("BACKUP_")`. Two loops run once per process: maintenance takes a snapshot each day and prunes history older than the retention window, and verification restores each replica to a temporary file every day and runs `PRAGMA integrity_check` on it:

```go source=site/internal/docs/example/backup/main.go
	go m.RunMaintenanceLoop(jobs, 0) // snapshots daily, history pruned after 30 days
	go m.RunVerifyLoop(jobs, 0, func(name string, err error) {
		slog.Error("backup: the replica does not restore", "db", name, "err", err)
	})
	prometheus.MustRegister(m.Collector("app")) // app_backup_enabled 0 is the alert for no backup
```

The stop runs in the opposite order. Every accepted write must be in the file before the final sync, and the final sync needs a context that is still alive, since the signal's is cancelled already:

```go source=site/internal/docs/example/backup/main.go
	srv.Shutdown(sctx)
	stopJobs()
	err = w.Close()
	err = errors.Join(err, detach(context.Background()))
	if _, e := db.W.ExecContext(sctx, "PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
		err = errors.Join(err, e)
	}
	return errors.Join(err, db.Close())
```

The unit's stop timeout (`TimeoutStopSec`) must cover the drain and the final sync: a process killed mid-sync leaves the replica behind the file. Only one process may replicate a database at a time, and Litestream opens the database with modernc's SQLite, which the app must use too: two SQLite builds in one process break each other's locks. lith uses modernc's.

`m.Collector("app")` exports `app_backup_enabled`, the last sync and the last verification of each replica, its snapshot's time and its size. Register it even when the manager is disabled: `app_backup_enabled 0` is the alert for a process that backs up nothing.

## Restoring to a moment

`m.RestoreAt(ctx, name, out, at)` writes the replica as of `at` to a new file; any moment inside the retention window is reachable, not only snapshots. `m.RestoreWindow` reports the earliest and the latest moment a restore can reach, and `m.RestoreTimeline` lists every restore point, snapshots first, for a picker that shows where history has gaps. Restore to a new file, check it, then swap it in while the app is stopped: for one database per account, `Deny`, `Evict`, swap, `Allow` ([One database per account](/docs/guides/tenants#erasing-an-account)).

## Erasing an account

An account erased for good also leaves its replica: detach it, then `m.PurgeReplica(ctx, name)` deletes everything replicated under its name, and `backup.MetaPath(path)` is the directory Litestream keeps beside the file, to delete with it.

## Two slots for a rollback

libretto and swisscyberguard run two slots of one systemd unit on one data directory, `app@blue` and `app@green`, so that a rollback is a cutover instead of a reinstall. Only one slot runs at a time, since Litestream allows one replicator per database, and Caddy proxies to both ports with `health_uri /readyz` and `lb_try_duration`, so requests wait through the seconds of a handoff. Their units and deploy scripts are in those projects' `deploy/` folders.
