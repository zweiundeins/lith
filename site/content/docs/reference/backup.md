---
title: backup
headline: lith/backup: Litestream replication to S3
summary: Replicates SQLite databases to S3-compatible storage with Litestream, embedded in the process.
description: Reference for lith/backup: replicate SQLite databases to S3-compatible storage with Litestream, embedded in the process, in a module of its own.
order: 11
---

Import path `github.com/zweiundeins/lith/backup`, a module of its own: `go get github.com/zweiundeins/lith/backup`. Generated from the doc comments in [backup/](https://github.com/zweiundeins/lith/tree/main/backup).

It is a module of its own so that projects without it never pull in Litestream's dependencies.

A Manager replicates every database attached to it, each under a name: its key prefix in the bucket, such as an account ID. Without a bucket (or Dir) the Manager is disabled and does nothing, so development runs without credentials.

Start each database in this order:

 1. RestoreIfMissing, before the database is opened. Opening creates an empty file, and a restore that finds a file does nothing.
 2. Open the database. While the Manager is enabled its write connection sets PRAGMA wal\_autocheckpoint=0 (see WALAutocheckpoint) and leaves checkpoints to Litestream, which makes them at positions it has replicated. Without Litestream the WAL would grow without bound, so keep SQLite's default then.
 3. Attach. It returns once the first sync has reached the replica.

Run RunMaintenanceLoop and RunVerifyLoop once per process. An incident at the storage provider fails every replica at once and usually passes by itself, so RunVerifyLoop retries a failed verification before it reports, and reports once per run: alert on its OnFail, not on each failure.

Both loops cover the attached replicas. An app that attaches its tenants on demand passes its account list as VerifyOptions.Names, so every tenant is verified, and as MaintenanceOptions.Names, so every tenant's history older than the retention window is pruned, attached or not. A tenant not attached gets no snapshot, since nothing changed while it was not; Attach writes one in the background when it is due.

Stop in this order: drain HTTP and the writer, so every accepted write is in the file; call detach, the final sync, with a live context (the signal's context is already cancelled); then checkpoint and close the database. The process's stop timeout (systemd TimeoutStopSec) must cover the drain and the final sync: killed mid-sync, the replica trails the file.

Litestream opens the database with modernc.org/sqlite, so the application must use it too: two SQLite copies in one process break each other's locks. Only one process may replicate a database at a time. Pruning relies on it: with one replicating process the positions in a replica only grow, so the pruning of a replica not attached is safe beside an Attach of it. A second writer, or a prefix and name reused for another database, resets the positions, and then any pruning is unsafe, attached or not.

An upload of 5 MiB or more to S3 goes up in parts, and one cut off (a snapshot that detach cancels, a crash) can leave its parts in the bucket, unlisted as objects. The Manager aborts the upload of a snapshot cut off by detach, shutdown or Config.SnapshotTimeout, best effort, before the name can be attached again, and PurgeReplica aborts every one under the name before it deletes, so an erasure leaves nothing. A provider that does not implement ListMultipartUploads gets none aborted, logged once. As a backstop, give the bucket a lifecycle rule that aborts incomplete multipart uploads after a day where the provider supports one.

## Constants

```go
const (
	DefaultRetentionWindow     = 30 * 24 * time.Hour
	DefaultSnapshotInterval    = 24 * time.Hour
	DefaultMaintenanceInterval = time.Hour
	DefaultVerifyInterval      = 24 * time.Hour
	DefaultSnapshotTimeout     = 10 * time.Minute
)
```

Policy defaults. RetentionWindow is also the point-in-time recovery floor: nothing older is restorable.

```go
const (
	DefaultMaintenanceConcurrency = 2
	DefaultMaintenancePause       = 100 * time.Millisecond
	DefaultDetachedInterval       = 24 * time.Hour
	DefaultDetachedConcurrency    = 1
	DefaultDetachedPause          = 250 * time.Millisecond
)
```

Maintenance defaults. A pass over the attached replicas maintains two at a time and waits after each. Names not attached get a pass a day, one at a time with a longer wait, since each costs a client of its own (see MaintenanceOptions.Names). Both spread a long list, so that the storage provider sees no burst of listings and deletes.

## Variables

```go
var (
	// ErrNothingToRestore means nothing was ever replicated under the name,
	// or nothing at or before the requested time.
	ErrNothingToRestore = errors.New("backup: nothing to restore")
	// ErrDisabled is returned by the operations that need a replica (Restore,
	// RestoreAt, RestoreWindow, RestoreTimeline, VerifyRestore) when the
	// Manager is disabled.
	ErrDisabled = errors.New("backup: not configured")
)
```

## Functions

### func MetaPath {#MetaPath}

```go
func MetaPath(dbPath string) string
```

MetaPath is the directory Litestream keeps beside a replicated database (".\<file>-litestream"). Remove it with the database when erasing one.

## Types

### type Config {#Config}

```go
type Config struct {
	Bucket          string
	Endpoint        string // empty for AWS; set for other S3-compatible services
	Region          string
	AccessKeyID     string // empty: the AWS default credential chain
	SecretAccessKey string
	ForcePathStyle  bool // most S3-compatible services other than AWS need it

	Dir string

	// Prefix goes in front of every replica's name, so that deployments can
	// share a bucket.
	Prefix string

	// Zero means DefaultRetentionWindow and DefaultSnapshotInterval.
	RetentionWindow  time.Duration
	SnapshotInterval time.Duration

	// SnapshotTimeout bounds the snapshot Attach starts in the background
	// when one is due; zero means DefaultSnapshotTimeout.
	SnapshotTimeout time.Duration

	// Logger defaults to slog.Default().
	Logger *slog.Logger
}
```

Config says where replicas go: set Bucket for S3, or Dir for a local directory (a mounted volume, or tests). With neither, the Manager is disabled.

#### func ConfigFromEnv {#ConfigFromEnv}

```go
func ConfigFromEnv(prefix string) (Config, error)
```

ConfigFromEnv reads a Config from the environment variables prefix plus BUCKET, ENDPOINT, REGION, ACCESS\_KEY\_ID, SECRET\_ACCESS\_KEY, FORCE\_PATH\_STYLE (a bool), DIR, PREFIX, RETENTION, SNAPSHOT\_INTERVAL and SNAPSHOT\_TIMEOUT (durations such as 720h). With prefix "BACKUP\_" the bucket is BACKUP\_BUCKET.

### type MaintenanceOptions {#MaintenanceOptions}

```go
type MaintenanceOptions struct {
	// Interval is the time between passes over the attached replicas; zero
	// means DefaultMaintenanceInterval. It only has to be shorter than the
	// snapshot interval.
	Interval time.Duration

	// Concurrency is the number of attached replicas maintained at once; zero
	// means DefaultMaintenanceConcurrency.
	Concurrency int

	// Pause is the wait after each attached replica before the same worker
	// takes the next; zero means DefaultMaintenancePause, a negative value
	// none. It sets a floor of 50 ms per replica with the defaults; the round
	// trips to the provider usually take longer.
	Pause time.Duration

	// Names, if set, lists the replicas of a second loop, for the ones not
	// attached: an app that attaches its tenants on demand passes its
	// account list, since a tenant not attached at the time is otherwise
	// never pruned. That loop skips the attached names. A name not attached
	// gets no snapshot, since nothing changed while it was not attached,
	// only the pruning of its history older than the retention window,
	// through the bucket alone: one listing, and one more with a snapshot
	// older than the window. A name with nothing replicated is no failure.
	//
	// Each such name takes a replica client of its own, and with S3 a new
	// HTTP transport and TLS handshake, whose idle connection lingers for
	// 90 s. There is no cache of clients, since thousands of tenants would
	// keep thousands of transports; the cost is held down by the cadence
	// instead: a pass a day, one name at a time.
	Names func(ctx context.Context) ([]string, error)

	// DetachedInterval is the time from the start of one pass over Names to
	// the start of the next; zero means DefaultDetachedInterval. The first
	// starts after at most ten minutes, and a pass that runs longer is
	// followed at once by the next, never overlapped.
	DetachedInterval time.Duration

	// DetachedConcurrency and DetachedPause are Concurrency and Pause for
	// the names not attached; zero means DefaultDetachedConcurrency and
	// DefaultDetachedPause. With the defaults 1,000 names take at least four
	// minutes, and in practice more: the round trips, a handshake and one or
	// two listings per name, dominate.
	DetachedConcurrency int
	DetachedPause       time.Duration
}
```

MaintenanceOptions configure RunMaintenanceLoop.

### type Manager {#Manager}

```go
type Manager struct {
	// contains filtered or unexported fields
}
```

Manager owns replication for the process. Its methods are safe for concurrent use.

#### func New {#New}

```go
func New(cfg Config) (*Manager, error)
```

New returns a Manager for cfg.

#### func (*Manager) Attach {#Manager.Attach}

```go
func (m *Manager) Attach(ctx context.Context, name, dbPath string) (detach func(context.Context) error, err error)
```

Attach starts replicating the database at dbPath under name. It returns once the first sync has reached the replica, since until then a final sync does nothing. dbPath must exist and name must not be attached already.

Call detach once, before the application's last checkpoint and close: it is the final sync. A cancelled context gets a fresh one for it, bounded by Litestream's shutdown sync timeout. On a disabled Manager detach does nothing.

When the newest snapshot is older than the snapshot interval, Attach writes one in the background after the first sync, bounded by Config.SnapshotTimeout, so that a database attached only between maintenance passes still gets snapshots, and its WAL segments a floor to be pruned at. detach cancels it and waits for it to stop before the final sync, on S3 with the abort of the upload it cut off (10 s at most); a database detached before its snapshot is done gets one on its next Attach.

#### func (*Manager) Collector {#Manager.Collector}

```go
func (m *Manager) Collector(namespace string) prometheus.Collector
```

Collector returns a prometheus.Collector of the Manager's state, read from Status on each scrape. Register it even when the Manager is disabled: \<namespace>\_backup\_enabled 0 is the alert for a process without a backup. Series with a replica label cover the attached replicas; maintenance of the others counts in the detached totals, so that a long MaintenanceOptions.Names adds no series. The pruned counts are close, not exact: a batch delete that fails in part counts nothing, and a key already gone counts as pruned. \<namespace>\_backup\_detached\_last\_pass\_timestamp\_seconds older than twice the detached interval is the alert for a loop over the names that does not run, or whose Names keeps failing. It reads 0 until the first pass ends, about ten minutes after the start, so the alert needs "and ... > 0" or a "for:". A pass in which every name failed still sets it: the failures counter and the error log are the signal for that.

#### func (*Manager) Enabled {#Manager.Enabled}

```go
func (m *Manager) Enabled() bool
```

Enabled reports whether the Manager replicates.

#### func (*Manager) MaintainAll {#Manager.MaintainAll}

```go
func (m *Manager) MaintainAll(ctx context.Context, now time.Time) map[string]error
```

MaintainAll runs one maintenance pass on every attached replica, as of now: a full snapshot when the newest is older than the snapshot interval, then pruning of the history older than the retention window.

#### func (*Manager) PurgeReplica {#Manager.PurgeReplica}

```go
func (m *Manager) PurgeReplica(ctx context.Context, name string) error
```

PurgeReplica deletes everything replicated under name, for an erasure: the objects, and on S3 the parts of every unfinished multipart upload under it, which a listing of objects does not show. It fails without deleting anything when those uploads cannot be listed or aborted, so that a retry finds them. The database must be detached first. Purging a name never replicated, or on a disabled Manager, does nothing.

#### func (*Manager) Restore {#Manager.Restore}

```go
func (m *Manager) Restore(ctx context.Context, name, out string) error
```

Restore writes the latest state of the replica under name to out, which must not exist yet.

#### func (*Manager) RestoreAt {#Manager.RestoreAt}

```go
func (m *Manager) RestoreAt(ctx context.Context, name, out string, at time.Time) error
```

RestoreAt writes the replica under name as of at (zero for the latest) to out, which must not exist yet. Any moment inside the retention window is reachable, not only snapshots. A time before the oldest retained point gives ErrNothingToRestore; check it against RestoreWindow for a clearer message.

#### func (*Manager) RestoreIfMissing {#Manager.RestoreIfMissing}

```go
func (m *Manager) RestoreIfMissing(ctx context.Context, name, dbPath string) error
```

RestoreIfMissing restores the replica under name to dbPath when no file is there: a first boot on a fresh disk, or recovery after losing one. It does nothing when the Manager is disabled, the file exists, or nothing was ever replicated under name. A restore that fails quick\_check is removed again.

#### func (*Manager) RestoreTimeline {#Manager.RestoreTimeline}

```go
func (m *Manager) RestoreTimeline(ctx context.Context, name string) (Timeline, error)
```

RestoreTimeline lists the replica under name as a Timeline. Snapshots are listed first, so a truncated timeline still has all of them.

#### func (*Manager) RestoreWindow {#Manager.RestoreWindow}

```go
func (m *Manager) RestoreWindow(ctx context.Context, name string) (earliest, latest time.Time, err error)
```

RestoreWindow reports the times a restore of the replica under name can reach: after earliest, the oldest full image (a snapshot or the first sync), up to latest. It lists the replica, so call it on demand, not per request.

#### func (*Manager) RetentionWindow {#Manager.RetentionWindow}

```go
func (m *Manager) RetentionWindow() time.Duration
```

RetentionWindow is how far back a restore can reach.

#### func (*Manager) RunMaintenanceLoop {#Manager.RunMaintenanceLoop}

```go
func (m *Manager) RunMaintenanceLoop(ctx context.Context, opt MaintenanceOptions)
```

RunMaintenanceLoop maintains the attached replicas every opt.Interval and, when opt.Names is set, the names not attached every opt.DetachedInterval, until ctx is done. An attached replica costs a listing of each level per pass, for Status. Its failure is logged as a warning, since the next pass retries it, and as an error from its third failed pass in a row on. The names not attached that fail are logged once per pass, as a count with the first few names, by the same rule, and so is a failure of opt.Names.

#### func (*Manager) RunVerifyLoop {#Manager.RunVerifyLoop}

```go
func (m *Manager) RunVerifyLoop(ctx context.Context, opt VerifyOptions)
```

RunVerifyLoop verifies every attached replica, or opt.Names, until ctx is done: the first run after at most ten minutes, then one every opt.Interval from the start of the one before. The replicas a run finds failing are verified again after each of opt.Retries, and those that never pass go to opt.OnFail, once per run. When ctx ends during a run, the run reports nothing.

#### func (*Manager) Status {#Manager.Status}

```go
func (m *Manager) Status() []ReplicaStatus
```

Status reports every attached replica, sorted by name. It reads no network, so it is cheap enough for a metrics scrape.

#### func (*Manager) VerifyAll {#Manager.VerifyAll}

```go
func (m *Manager) VerifyAll(ctx context.Context) map[string]error
```

VerifyAll runs VerifyRestore on every attached replica, and stops when ctx is done.

#### func (*Manager) VerifyRestore {#Manager.VerifyRestore}

```go
func (m *Manager) VerifyRestore(ctx context.Context, name string) error
```

VerifyRestore proves the replica under name is restorable: it restores the latest state to a temporary file, runs PRAGMA integrity\_check and removes it. The result shows in Status while the replica is attached; it is kept for a replica that is not, and shows once it is. A restore cut off by a cancelled ctx keeps no result, one cut off by its deadline fails, and one that overlaps PurgeReplica of the name keeps none. It reads the whole replica, so it belongs in a daily loop, not on a request.

#### func (*Manager) WALAutocheckpoint {#Manager.WALAutocheckpoint}

```go
func (m *Manager) WALAutocheckpoint() int
```

WALAutocheckpoint is the value for PRAGMA wal\_autocheckpoint on the application's write connection: 0 while the Manager replicates, since Litestream then checkpoints, and SQLite's default of 1000 pages otherwise.

### type ReplicaStatus {#ReplicaStatus}

```go
type ReplicaStatus struct {
	Name       string
	Path       string
	LastSyncAt time.Time // zero until the first sync
	TXID       string    // replicated position, "" until known

	LastVerifyAt time.Time // zero until VerifyRestore ran
	VerifyOK     bool      // the last VerifyRestore restored and passed integrity_check

	// From RunVerifyLoop. VerifyAttempts is the number of verifications in its
	// last run: 1 if the replica passed at once, 0 until a run has finished.
	// VerifyRetried counts the runs in which it failed and then passed on a
	// retry.
	VerifyAttempts int
	VerifyRetried  int

	// From the last maintenance pass; zero until one ran.
	LastSnapshotAt time.Time
	ReplicaObjects int
	ReplicaBytes   int64

	// The maintenance passes that completed while the replica was attached,
	// and the objects they pruned. Passes over it while it was not attached
	// count in the Collector's detached totals.
	MaintenancePasses int
	PrunedObjects     int

	// MaintenanceSkipped counts the passes that found the replica busy (a
	// snapshot after Attach, or the pruning of names not attached) and left
	// it to the next; such a pass neither ends nor adds to a failure streak.
	MaintenanceSkipped int
}
```

ReplicaStatus is the state of one attached replica, read from memory.

### type RestorePoint {#RestorePoint}

```go
type RestorePoint struct {
	At       time.Time
	TXID     string
	Level    int
	Snapshot bool // a full image of the database, not a WAL segment
	Size     int64
}
```

RestorePoint is one point a restore can land on: an LTX file's time and position. A restore to a time lands on the last point at or before it.

### type Timeline {#Timeline}

```go
type Timeline struct {
	Earliest  time.Time
	Latest    time.Time
	Points    []RestorePoint
	Truncated bool // the listing stopped at maxRestorePoints: the newest WAL segments are missing
}
```

Timeline is every restore point of a replica, oldest first, for a point-in-time picker that shows where history is and where it has gaps.

### type VerifyFailure {#VerifyFailure}

```go
type VerifyFailure struct {
	Name        string
	First, Last error
	Attempts    int
}
```

VerifyFailure is a replica that failed every verification of a run. First and Last are the errors of its first and last attempt. The failures of one run share Attempts, since each went through every retry.

### type VerifyOptions {#VerifyOptions}

```go
type VerifyOptions struct {
	// Interval is the time from one run's start to the next one's; zero means
	// DefaultVerifyInterval.
	Interval time.Duration

	// Retries are the waits before each retry of the replicas still failing,
	// each from the end of the attempt before. Nil means 10 and 20 minutes,
	// about 10 and 30 minutes after the run; an empty slice means none. A
	// zero or negative wait retries at once. A retry that would start at or
	// after the next run's start is skipped, with every one after it. For a
	// provider whose incidents last hours, stretch them toward the interval,
	// such as 30m, 2h and 6h against 24h.
	Retries []time.Duration

	// Names, if set, lists the replicas each run verifies, in place of the
	// attached ones: an app that attaches its tenants on demand passes its
	// account list, since a tenant not attached at the time is otherwise
	// never verified. Each is verified by name with VerifyRestore, which
	// attaches nothing; a name with nothing replicated under it fails. Its
	// results show in Status and the metrics once the replica is attached.
	// When Names fails, the run verifies nothing and OnFail gets one failure
	// with an empty Name and Names' error.
	Names func(ctx context.Context) ([]string, error)

	// OnFail, if set, is called once per run, after its retries, with the
	// replicas that failed every attempt, sorted by name, and the run's span:
	// the time from its start to the end of its last attempt. A run in which
	// every replica passed, at once or on a retry, calls nothing, and a
	// replica purged during the run is left out. A panic in OnFail is logged
	// and the loop goes on.
	OnFail func(failures []VerifyFailure, span time.Duration)
}
```

VerifyOptions configure RunVerifyLoop.
