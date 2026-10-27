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

The same app passes its account list as `MaintenanceOptions.Names`, or a tenant that nobody opened since the last deploy keeps its snapshots and its history beyond the window until someone does. The attached replicas keep their hourly pass, two at a time with 100 ms after each (`Concurrency`, `Pause`). The names get a loop of their own: a pass a day (`DetachedInterval`), the first ten minutes after the start, each reading the list afresh, skipping the attached names and never overlapping the one before. A name not attached gets no snapshot, since nothing changed while it was not attached, only the pruning through the bucket: the snapshots before the newest one at least 30 days old go, and so do the WAL segments it covers. A name with nothing replicated costs a listing and is no failure. Each name takes a client of its own, and with S3 a TLS handshake, since a cache of clients would keep a transport per tenant; so that loop runs one name at a time with 250 ms after each (`DetachedConcurrency`, `DetachedPause`). 1,000 tenants take at least four minutes, more in practice, as the round trips dominate, and the provider sees no burst of listings and deletes. A failed delete is not retried within the pass; the next pass lists again. The failures of a pass are one log line with their count and the first five names, a warning until the third failed pass in a row and then an error, and so is a failed `Names`: a missed pass costs storage, not a backup. A replica purged during a pass is skipped. Pruning and `RestoreWindow` go by each object's S3 LastModified time, while `RestoreAt` goes by the time Litestream stored with it, so a restore right at the window's edge can miss by the writer's clock skew against S3. Pruning is safe while one process replicates a name, as it must; a second writer, or a prefix reused for another database, makes any pruning unsafe.

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

When the newest snapshot is older than a day, `Attach` writes one in the background after the first sync, so a tenant attached only between maintenance passes still gets one. It is bounded by `Config.SnapshotTimeout` (10 minutes). Detach cancels it, and the hourly pass's work on that tenant too, then waits for them to stop before the final sync, so an eviction, a purge or a shutdown waits for a cancel to take hold, not for an upload, and on S3 for the abort of the upload it cut off, 10 s at most; a tenant detached before its snapshot is done gets one on its next attach. A pass that finds a tenant busy with that snapshot skips it until the next pass, counts it in `app_backup_replica_maintenance_skipped_total{replica}` and keeps the tenant's run of failed passes as it was. The unit's stop timeout (`TimeoutStopSec`) must cover the drain and the final sync: a process killed mid-sync leaves the replica behind the file. Only one process may replicate a database at a time, and Litestream opens the database with modernc's SQLite, which the app must use too: two SQLite builds in one process break each other's locks. lith uses modernc's.

`m.Collector("app")` exports `app_backup_enabled`, the last sync and the last verification of each replica, its snapshot's time and its size. Register it even when the manager is disabled: `app_backup_enabled 0` is the alert for a process that backs up nothing. `app_backup_replica_verify_retried_total{replica}` counts the runs in which a replica failed and then passed on a retry, and `app_backup_replica_verify_attempts{replica}` is the number of verifications in the last run: worth a panel, since a replica that keeps needing retries points at the provider, but not an alert. `app_backup_replica_maintenance_passes_total{replica}` and `app_backup_replica_pruned_objects_total{replica}` count the maintenance of each attached replica; the passes over replicas that were not attached add to `app_backup_detached_maintenance_passes_total`, `app_backup_detached_maintenance_failures_total` and `app_backup_detached_pruned_objects_total`, without a label, so that a list of thousands of tenants adds no series. The pruned counts are approximate: a delete that fails in part counts nothing, and a key already gone counts. `app_backup_detached_last_pass_timestamp_seconds` is when the last pass over the names ended; a failed `Names` leaves it where it was, so older than two days it is the alert for a loop that does not run or a list that keeps failing: `time() - app_backup_detached_last_pass_timestamp_seconds > 2 * 86400 and app_backup_detached_last_pass_timestamp_seconds > 0`, since it reads 0 until the first pass ends, about ten minutes after the start. A pass in which every name failed still sets it; the failures counter and the error log show that one.

## What a verification costs

A restore, and so a verification, reads the newest snapshot and every WAL segment written after it. Under steady writes that is a small file for every second with a commit since the snapshot, which comes once a day by default (`Config.SnapshotInterval`): tens of thousands by the evening. The manager downloads them with four connections and merges them 32 at a time, in a directory beside the restored file, so a restore holds a few connections and a few dozen open files however many there are. It needs disk for the files it reads and the restored database, and memory for little more than SQLite's integrity check. Litestream's own restore opens every file at once, one connection each, until it is done; the manager used it up to `backup/v0.3.0`, and a replica with thousands of segments after its snapshot held thousands of connections and hundreds of MiB for the length of the restore. A shorter `SnapshotInterval`, such as six hours, keeps the number of segments down, and the restore's time with it.

Every verification logs its outcome: `backup: restore verified` (info) with the replica, the time it took, the restored database's bytes and the number of files; `backup: restore verification failed` (warning) with the error; `backup: restore verification cut off` (warning) when its context ended, such as a stop in the middle of it. The loop's lines about retries and the run's failures come on top. The first run comes ten minutes after the start (`VerifyOptions.FirstDelay`). A server that is deployed often, or busy for a while after it starts, can wait longer; a stop cuts the run off, and the next start schedules it again.

### Verifying out of process

The restore runs in the process that calls it, with the server's memory limit and inside its stop timeout. To keep it apart, leave `RunVerifyLoop` out of the server and run `VerifyOnce` from a command of the app's own binary, which a systemd timer starts with a memory limit of its own:

```go source=site/internal/docs/example/backup/main.go
// verify is "app verify": one verification run for a systemd timer, so
// that the restore's memory and connections are not the server's. It
// exits non-zero when a replica fails every attempt.
func verify() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := backup.ConfigFromEnv("BACKUP_")
	if err != nil {
		return err
	}
	m, err := backup.New(cfg)
	if err != nil {
		return err
	}
	// Nothing is attached here: name the replicas, or the account list.
	return m.VerifyOnce(ctx, backup.VerifyOptions{
		Names:   func(context.Context) ([]string, error) { return []string{"app"}, nil },
		Retries: []time.Duration{10 * time.Minute},
	})
}
```

`VerifyOnce` is one run of the loop: it restores each name, retries the failures after each of `Retries`, calls `OnFail` once with those that failed every attempt and returns their errors, or nil. The command exits non-zero, so the unit fails, and a failed unit is the alert. The command only reads the bucket, so it runs beside the replicating server. The server's `Status` and metrics show no verification then; the unit's state and its log do.

```ini title=/etc/systemd/system/app-verify.service
[Unit]
Description=app: restore the replicas and check them

[Service]
Type=oneshot
User=app
EnvironmentFile=/etc/app/env
ExecStart=/opt/app/app verify
# A tmpfs /tmp would count the restored file against MemoryMax.
Environment=TMPDIR=/var/lib/app/tmp
MemoryMax=512M
MemorySwapMax=0
Nice=10
IOSchedulingClass=idle
TimeoutStartSec=2h
```

```ini title=/etc/systemd/system/app-verify.timer
[Unit]
Description=app: verify the replicas daily

[Timer]
OnCalendar=*-*-* 04:30
RandomizedDelaySec=30m
Persistent=true

[Install]
WantedBy=timers.target
```

`TMPDIR` must exist and have room for the restore. Enable the timer with `systemctl enable --now app-verify.timer`, and alert on the failed unit (`OnFailure=` or the host's alert on failed units).

## Restoring to a moment

`m.RestoreAt(ctx, name, out, at)` writes the replica as of `at` to a new file; any moment inside the retention window is reachable, not only snapshots. `m.RestoreWindow` reports the earliest and the latest moment a restore can reach, and `m.RestoreTimeline` lists every restore point, snapshots first, for a picker that shows where history has gaps. Restore to a new file, check it, then swap it in while the app is stopped: for one database per account, `Deny`, `Evict`, swap, `Allow` ([One database per account](/docs/guides/tenants#erasing-an-account)).

## Erasing an account

An account erased for good also leaves its replica: detach it, then `m.PurgeReplica(ctx, name)` deletes everything replicated under its name, and `backup.MetaPath(path)` is the directory Litestream keeps beside the file, to delete with it.

An upload of 5 MiB or more goes to S3 in parts, and one that is cut off, such as a snapshot that detach cancels or one in a process that crashed, can leave its parts in the bucket: pages of the account's database that a listing of objects does not show. The S3 SDK's own abort runs with the context that was cancelled, so it fails. When a snapshot is cut off, by detach, a shutdown or its timeout, the manager lists the unfinished uploads under the replica's snapshot level and aborts them with a fresh context, best effort, before the name can be attached again, so a new snapshot of the name is never among them. `PurgeReplica` holds the name, lists every unfinished upload under it and aborts it before it deletes anything; when it cannot, the purge fails and a retry finds them again. A provider that does not implement the listing (`NotImplemented`) has nothing the manager can abort: the purge goes on and logs that once. The purge is the reliable fix. As a backstop, give the bucket a lifecycle rule `AbortIncompleteMultipartUpload` after a day where the provider supports it: AWS does, and so do several S3-compatible providers, but S3 layers on OpenStack Swift (Infomaniak's object storage is one) may not implement lifecycle rules at all. We could not confirm it for Infomaniak, so test it with `aws s3api put-bucket-lifecycle-configuration` before relying on it.

## Two slots for a rollback

An app can run two slots of one systemd unit on one data directory, `app@blue` and `app@green`, so that a rollback is a cutover instead of a reinstall. Only one slot runs at a time, since Litestream allows one replicator per database, and Caddy proxies to both ports with `health_uri /readyz` and `lb_try_duration`, so requests wait through the seconds of a handoff.
