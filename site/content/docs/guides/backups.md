---
title: Backups
headline: Backups: VACUUM INTO copies and Litestream to S3
summary: A consistent copy with VACUUM INTO on a timer, or continuous replication to S3 with Litestream and lith's backup module. How to wire each, check a copy, and restore.
description: A consistent SQLite copy with VACUUM INTO on a timer, or continuous replication to S3 with Litestream: how to wire each, check a copy, and restore.
order: 10
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
// Snapshots daily, history pruned after 30 days. An app that attaches its
// tenants on demand sets Names to its account list here too.
go m.RunMaintenanceLoop(jobs, backup.MaintenanceOptions{})
// Verifies daily, and retries a failure after 10 and 30 minutes.
go m.RunVerifyLoop(jobs, backup.VerifyOptions{
	// An app that attaches its tenants on demand sets Names to its account list.
	OnFail: func(failures []backup.VerifyFailure, span time.Duration) {
		// Once per run, with the replicas that failed every attempt: alert here.
		var names []string
		for _, f := range failures {
			names = append(names, f.Name)
		}
		slog.Error("backup: replicas do not restore", "dbs", names, "first", failures[0].First, "last", failures[0].Last, "span", span)
	},
})
prometheus.MustRegister(m.Collector("app")) // app_backup_enabled 0 is the alert for no backup
```

An incident at the storage provider (throttling, errors that come and go for a few hours) fails every replica's verification at once, and replication usually catches up by itself. So the verify loop retries the replicas that failed, 10 and 30 minutes after the run (`VerifyOptions.Retries`), and calls `OnFail` once per run, after the retries, with only the replicas that failed every attempt: each with its first and last error and its number of attempts. A run in which everything passed, at once or on a retry, calls nothing. Alert there, once per run, never per replica or per failed attempt. The failures of one call share their number of attempts, since each went through every retry. The next run starts a day after this one started, however long its retries took, and a retry that would reach it is skipped. For a provider whose incidents last hours, stretch the retries toward the interval, such as `Retries: []time.Duration{30 * time.Minute, 2 * time.Hour, 6 * time.Hour}` against a day.

Both loops cover the attached replicas. An app that attaches its tenants on demand ([One database per account](/docs/guides/tenants)) passes its account list as `VerifyOptions.Names`: each run then restores those names, attached or not, and a name with nothing replicated fails. Their results show in `Status` and the metrics once the replica is attached; until then `OnFail` and the logs are the signal. When `Names` returns an error, the run verifies nothing and `OnFail` gets one failure with an empty name and that error, since a day without a verification should not pass in silence. A replica purged with `PurgeReplica` during a run is not retried, not reported and keeps no result.

The same app passes its account list as `MaintenanceOptions.Names`, or a tenant that nobody opened since the last deploy keeps its snapshots and its history beyond the window until someone does. Each hourly pass reads the list and maintains every name once: an attached one as above, one that is not attached through the bucket alone. That one gets no snapshot, since nothing changed while it was not attached, only the pruning: the snapshots before the newest one at least 30 days old go, and so do the WAL segments it covers. A name with nothing replicated costs a listing and is no failure. A pass maintains two replicas at a time (`Concurrency`) and waits 100 ms after each (`Pause`), so 1,000 tenants take about a minute and the provider sees no burst of listings and deletes; a failed delete is not retried, the next pass lists again. When `Names` fails, the pass maintains the attached replicas, and the log warns until the third failed pass in a row, then logs an error, as for a replica: a missed pass costs storage, not a backup. A replica purged during a pass is skipped.

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

After the final sync, detach writes a snapshot when the newest is older than a day, so a tenant attached only between maintenance passes still gets one. The unit's stop timeout (`TimeoutStopSec`) must cover the drain, the final sync and that snapshot: a process killed mid-sync leaves the replica behind the file. Only one process may replicate a database at a time, and Litestream opens the database with modernc's SQLite, which the app must use too: two SQLite builds in one process break each other's locks. lith uses modernc's.

`m.Collector("app")` exports `app_backup_enabled`, the last sync and the last verification of each replica, its snapshot's time and its size. Register it even when the manager is disabled: `app_backup_enabled 0` is the alert for a process that backs up nothing. `app_backup_replica_verify_retried_total{replica}` counts the runs in which a replica failed and then passed on a retry, and `app_backup_replica_verify_attempts{replica}` is the number of verifications in the last run: worth a panel, since a replica that keeps needing retries points at the provider, but not an alert. `app_backup_replica_maintenance_passes_total{replica}` and `app_backup_replica_pruned_objects_total{replica}` count the maintenance of each attached replica; the passes over replicas that were not attached add to `app_backup_detached_maintenance_passes_total` and `app_backup_detached_pruned_objects_total`, without a label, so that a list of thousands of tenants adds no series.

## Restoring to a moment

`m.RestoreAt(ctx, name, out, at)` writes the replica as of `at` to a new file; any moment inside the retention window is reachable, not only snapshots. `m.RestoreWindow` reports the earliest and the latest moment a restore can reach, and `m.RestoreTimeline` lists every restore point, snapshots first, for a picker that shows where history has gaps. Restore to a new file, check it, then swap it in while the app is stopped: for one database per account, `Deny`, `Evict`, swap, `Allow` ([One database per account](/docs/guides/tenants#erasing-an-account)).

## Erasing an account

An account erased for good also leaves its replica: detach it, then `m.PurgeReplica(ctx, name)` deletes everything replicated under its name, and `backup.MetaPath(path)` is the directory Litestream keeps beside the file, to delete with it.

## Two slots for a rollback

An app can run two slots of one systemd unit on one data directory, `app@blue` and `app@green`, so that a rollback is a cutover instead of a reinstall. Only one slot runs at a time, since Litestream allows one replicator per database, and Caddy proxies to both ports with `health_uri /readyz` and `lb_try_duration`, so requests wait through the seconds of a handoff.
