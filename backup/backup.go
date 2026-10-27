// Package backup replicates SQLite databases to S3-compatible storage with
// Litestream, embedded in the process. It is a module of its own so that
// projects without it never pull in Litestream's dependencies.
//
// A Manager replicates every database attached to it, each under a name: its
// key prefix in the bucket, such as an account ID. Without a bucket (or Dir)
// the Manager is disabled and does nothing, so development runs without
// credentials.
//
// Start each database in this order:
//
//  1. RestoreIfMissing, before the database is opened. Opening creates an
//     empty file, and a restore that finds a file does nothing.
//  2. Open the database. While the Manager is enabled its write connection
//     sets PRAGMA wal_autocheckpoint=0 (see WALAutocheckpoint) and leaves
//     checkpoints to Litestream, which makes them at positions it has
//     replicated. Without Litestream the WAL would grow without bound, so
//     keep SQLite's default then.
//  3. Attach. It returns once the first sync has reached the replica.
//
// Run RunMaintenanceLoop and RunVerifyLoop once per process. An incident at
// the storage provider fails every replica at once and usually passes by
// itself, so RunVerifyLoop retries a failed verification before it reports,
// and reports once per run: alert on its OnFail, not on each failure.
//
// A restore reads the newest snapshot and every WAL segment since, a file
// per second of writes. It downloads them with 4 connections and merges
// them 32 at a time, in a directory beside its output, so what it holds
// does not grow with their number; it needs disk for them and the restored
// file. VerifyRestore logs every outcome, also a restore cut off. To keep a
// restore out of the server's memory, run VerifyOnce from a command that a
// timer starts, with a memory limit of its own, in place of RunVerifyLoop.
//
// Both loops cover the attached replicas. An app that attaches its tenants
// on demand passes its account list as VerifyOptions.Names, so every tenant
// is verified, and as MaintenanceOptions.Names, so every tenant's history
// older than the retention window is pruned, attached or not. A tenant not
// attached gets no snapshot, since nothing changed while it was not; Attach
// writes one in the background when it is due.
//
// Stop in this order: drain HTTP and the writer, so every accepted write is
// in the file; call detach, the final sync, with a live context (the
// signal's context is already cancelled); then checkpoint and close the
// database. The process's stop timeout (systemd TimeoutStopSec) must cover
// the drain and the final sync: killed mid-sync, the replica trails the
// file.
//
// Litestream opens the database with modernc.org/sqlite, so the application
// must use it too: two SQLite copies in one process break each other's locks.
// Only one process may replicate a database at a time. Pruning relies on it:
// with one replicating process the positions in a replica only grow, so the
// pruning of a replica not attached is safe beside an Attach of it. A
// second writer, or a prefix and name reused for another database, resets
// the positions, and then any pruning is unsafe, attached or not.
//
// An upload of 5 MiB or more to S3 goes up in parts, and one cut off (a
// snapshot that detach cancels, a crash) can leave its parts in the bucket,
// unlisted as objects. The Manager aborts the upload of a snapshot cut off
// by detach, shutdown or Config.SnapshotTimeout, best effort, before the
// name can be attached again, and PurgeReplica aborts every one under the name
// before it deletes, so an erasure leaves nothing. A provider that does not
// implement ListMultipartUploads gets none aborted, logged once. As a
// backstop, give the bucket a lifecycle rule that aborts incomplete
// multipart uploads after a day where the provider supports one.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/benbjohnson/litestream/file"
	"github.com/benbjohnson/litestream/s3"
)

// Policy defaults. RetentionWindow is also the point-in-time recovery floor:
// nothing older is restorable.
const (
	DefaultRetentionWindow     = 30 * 24 * time.Hour
	DefaultSnapshotInterval    = 24 * time.Hour
	DefaultMaintenanceInterval = time.Hour
	DefaultVerifyInterval      = 24 * time.Hour
	DefaultSnapshotTimeout     = 10 * time.Minute
)

var (
	// ErrNothingToRestore means nothing was ever replicated under the name,
	// or nothing at or before the requested time.
	ErrNothingToRestore = errors.New("backup: nothing to restore")
	// ErrDisabled is returned by the operations that need a replica (Restore,
	// RestoreAt, RestoreWindow, RestoreTimeline, VerifyRestore) when the
	// Manager is disabled.
	ErrDisabled = errors.New("backup: not configured")
)

// Config says where replicas go: set Bucket for S3, or Dir for a local
// directory (a mounted volume, or tests). With neither, the Manager is
// disabled.
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

// ConfigFromEnv reads a Config from the environment variables prefix plus
// BUCKET, ENDPOINT, REGION, ACCESS_KEY_ID, SECRET_ACCESS_KEY,
// FORCE_PATH_STYLE (a bool), DIR, PREFIX, RETENTION, SNAPSHOT_INTERVAL and
// SNAPSHOT_TIMEOUT (durations such as 720h). With prefix "BACKUP_" the bucket is BACKUP_BUCKET.
func ConfigFromEnv(prefix string) (Config, error) {
	env := func(k string) string { return os.Getenv(prefix + k) }
	cfg := Config{
		Bucket:          env("BUCKET"),
		Endpoint:        env("ENDPOINT"),
		Region:          env("REGION"),
		AccessKeyID:     env("ACCESS_KEY_ID"),
		SecretAccessKey: env("SECRET_ACCESS_KEY"),
		Dir:             env("DIR"),
		Prefix:          env("PREFIX"),
	}
	var err error
	if v := env("FORCE_PATH_STYLE"); v != "" {
		if cfg.ForcePathStyle, err = strconv.ParseBool(v); err != nil {
			return Config{}, fmt.Errorf("backup: %sFORCE_PATH_STYLE: %w", prefix, err)
		}
	}
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{{"RETENTION", &cfg.RetentionWindow}, {"SNAPSHOT_INTERVAL", &cfg.SnapshotInterval}, {"SNAPSHOT_TIMEOUT", &cfg.SnapshotTimeout}} {
		if v := env(d.key); v != "" {
			if *d.dst, err = time.ParseDuration(v); err != nil {
				return Config{}, fmt.Errorf("backup: %s%s: %w", prefix, d.key, err)
			}
		}
	}
	return cfg, nil
}

// Manager owns replication for the process. Its methods are safe for
// concurrent use.
type Manager struct {
	cfg              Config
	enabled          bool
	retentionWindow  time.Duration
	snapshotInterval time.Duration
	snapshotTimeout  time.Duration
	log              *slog.Logger
	newClient        func(name string) litestream.ReplicaClient
	mpu              *multipartClient

	mu            sync.Mutex
	reserved      map[string]bool // names being attached or detached
	attached      map[string]*attachedDB
	verify        map[string]verifyResult
	maintenance   map[string]maintResult
	detachedMaint maintTotals
	pruning       map[string]bool // names not attached being pruned
	purgeSeq      uint64
	purges        map[string]uint64 // purgeSeq at a name's last purge: a verification that overlaps it keeps no result
}

// attachedDB pairs a replicating database with the lock that keeps a
// maintenance pass and detach from running on it at once.
type attachedDB struct {
	db       *litestream.DB
	deletes  *deleteCounter
	maint    sync.Mutex
	detached bool // set under maint by detach

	// work is the context of the snapshots and maintenance of the replica,
	// which detach cancels first, so that it waits for no upload; it waits
	// for Attach's background snapshot to stop on snapshotDone.
	work         context.Context
	stopWork     context.CancelFunc
	snapshotDone chan struct{}
}

type verifyResult struct {
	at  time.Time
	err error

	// From RunVerifyLoop's runs.
	attempts int
	retried  int
}

// New returns a Manager for cfg.
func New(cfg Config) (*Manager, error) {
	switch {
	case cfg.Bucket != "" && cfg.Dir != "":
		return nil, errors.New("backup: set Bucket or Dir, not both")
	case cfg.RetentionWindow < 0 || cfg.SnapshotInterval < 0 || cfg.SnapshotTimeout < 0:
		return nil, errors.New("backup: negative retention window, snapshot interval or snapshot timeout")
	case checkPrefix(cfg.Prefix) != nil:
		return nil, checkPrefix(cfg.Prefix)
	}
	m := &Manager{
		cfg:              cfg,
		enabled:          cfg.Bucket != "" || cfg.Dir != "",
		retentionWindow:  orDefault(cfg.RetentionWindow, DefaultRetentionWindow),
		snapshotInterval: orDefault(cfg.SnapshotInterval, DefaultSnapshotInterval),
		snapshotTimeout:  orDefault(cfg.SnapshotTimeout, DefaultSnapshotTimeout),
		log:              cfg.Logger,
		reserved:         map[string]bool{},
		attached:         map[string]*attachedDB{},
		verify:           map[string]verifyResult{},
		maintenance:      map[string]maintResult{},
		pruning:          map[string]bool{},
		mpu:              &multipartClient{},
		purges:           map[string]uint64{},
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if cfg.Dir != "" {
		m.newClient = m.fileClient
	} else {
		m.newClient = m.s3Client
	}
	m.mpu.newAPI = m.newMultipartAPI
	return m, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (m *Manager) s3Client(name string) litestream.ReplicaClient {
	c := s3.NewReplicaClient()
	c.Bucket = m.cfg.Bucket
	c.Path = path.Join(m.cfg.Prefix, name)
	c.Endpoint = m.cfg.Endpoint
	c.Region = m.cfg.Region
	c.AccessKeyID = m.cfg.AccessKeyID
	c.SecretAccessKey = m.cfg.SecretAccessKey
	c.ForcePathStyle = m.cfg.ForcePathStyle
	c.SetLogger(m.log)
	return c
}

func (m *Manager) fileClient(name string) litestream.ReplicaClient {
	c := file.NewReplicaClient(filepath.Join(m.cfg.Dir, filepath.FromSlash(path.Join(m.cfg.Prefix, name))))
	c.SetLogger(m.log)
	return c
}

// checkName accepts a single path element, so that no replica sits inside
// another's keys: a purge deletes everything under its name.
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("backup: invalid replica name %q", name)
	}
	return nil
}

func checkPrefix(prefix string) error {
	if prefix != "" && (path.Clean(prefix) != prefix || path.IsAbs(prefix) ||
		prefix == "." || prefix == ".." || strings.HasPrefix(prefix, "../")) {
		return fmt.Errorf("backup: invalid prefix %q", prefix)
	}
	return nil
}

// Enabled reports whether the Manager replicates.
func (m *Manager) Enabled() bool { return m.enabled }

// RetentionWindow is how far back a restore can reach.
func (m *Manager) RetentionWindow() time.Duration { return m.retentionWindow }

// WALAutocheckpoint is the value for PRAGMA wal_autocheckpoint on the
// application's write connection: 0 while the Manager replicates, since
// Litestream then checkpoints, and SQLite's default of 1000 pages otherwise.
func (m *Manager) WALAutocheckpoint() int {
	if m.enabled {
		return 0
	}
	return 1000
}

// MetaPath is the directory Litestream keeps beside a replicated database
// (".<file>-litestream"). Remove it with the database when erasing one.
func MetaPath(dbPath string) string {
	dir, f := filepath.Split(dbPath)
	return filepath.Join(dir, "."+f+litestream.MetaDirSuffix)
}

// RestoreIfMissing restores the replica under name to dbPath when no file is
// there: a first boot on a fresh disk, or recovery after losing one. It does
// nothing when the Manager is disabled, the file exists, or nothing was ever
// replicated under name. A restore that fails quick_check is removed again.
func (m *Manager) RestoreIfMissing(ctx context.Context, name, dbPath string) error {
	if !m.enabled {
		return nil
	}
	if err := checkName(name); err != nil {
		return err
	}
	if _, err := os.Stat(dbPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup: %w", err)
	}
	err := m.restore(ctx, name, dbPath, time.Time{}, litestream.IntegrityCheckQuick)
	if errors.Is(err, ErrNothingToRestore) {
		return nil
	}
	if err != nil {
		return wrap(err, "restore", name)
	}
	m.log.Info("backup: restored the database from its replica", "replica", name, "path", dbPath)
	return nil
}

// Restore writes the latest state of the replica under name to out, which
// must not exist yet.
func (m *Manager) Restore(ctx context.Context, name, out string) error {
	return m.RestoreAt(ctx, name, out, time.Time{})
}

// RestoreAt writes the replica under name as of at (zero for the latest) to
// out, which must not exist yet. Any moment inside the retention window is
// reachable, not only snapshots. A time before the oldest retained point
// gives ErrNothingToRestore; check it against RestoreWindow for a clearer
// message.
func (m *Manager) RestoreAt(ctx context.Context, name, out string, at time.Time) error {
	if !m.enabled {
		return ErrDisabled
	}
	if err := checkName(name); err != nil {
		return err
	}
	return wrap(m.restore(ctx, name, out, at, litestream.IntegrityCheckQuick), "restore", name)
}

func (m *Manager) restore(ctx context.Context, name, out string, at time.Time, check litestream.IntegrityCheckMode) error {
	_, err := m.restoreTo(ctx, m.newClient(name), out, at, check)
	return err
}

// wrap names the operation and replica that failed, except for
// ErrNothingToRestore, which callers compare against.
func wrap(err error, op, name string) error {
	if err == nil || err == ErrNothingToRestore {
		return err
	}
	return fmt.Errorf("backup: %s %s: %w", op, name, err)
}

// RestoreWindow reports the times a restore of the replica under name can
// reach: after earliest, the oldest full image (a snapshot or the first
// sync), up to latest. It lists the replica, so call it on demand, not per
// request.
func (m *Manager) RestoreWindow(ctx context.Context, name string) (earliest, latest time.Time, err error) {
	if !m.enabled {
		return time.Time{}, time.Time{}, ErrDisabled
	}
	if err := checkName(name); err != nil {
		return time.Time{}, time.Time{}, err
	}
	client := m.newClient(name)
	for level := 0; level <= litestream.SnapshotLevel; level++ {
		itr, err := client.LTXFiles(ctx, level, 0, false)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("backup: restore window of %s: %w", name, err)
		}
		for itr.Next() {
			info := itr.Item()
			if info.CreatedAt.After(latest) {
				latest = info.CreatedAt
			}
			if info.MinTXID <= 1 && (earliest.IsZero() || info.CreatedAt.Before(earliest)) {
				earliest = info.CreatedAt
			}
		}
		if err := errors.Join(itr.Err(), itr.Close()); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("backup: restore window of %s: %w", name, err)
		}
	}
	if earliest.IsZero() {
		return time.Time{}, time.Time{}, ErrNothingToRestore
	}
	return earliest, latest, nil
}

// RestorePoint is one point a restore can land on: an LTX file's time and
// position. A restore to a time lands on the last point at or before it.
type RestorePoint struct {
	At       time.Time
	TXID     string
	Level    int
	Snapshot bool // a full image of the database, not a WAL segment
	Size     int64
}

// Timeline is every restore point of a replica, oldest first, for a
// point-in-time picker that shows where history is and where it has gaps.
type Timeline struct {
	Earliest  time.Time
	Latest    time.Time
	Points    []RestorePoint
	Truncated bool // the listing stopped at maxRestorePoints: the newest WAL segments are missing
}

// maxRestorePoints bounds a timeline, so that a busy replica cannot turn one
// listing into an unbounded fan-out.
const maxRestorePoints = 20000

// RestoreTimeline lists the replica under name as a Timeline. Snapshots are
// listed first, so a truncated timeline still has all of them.
func (m *Manager) RestoreTimeline(ctx context.Context, name string) (Timeline, error) {
	if !m.enabled {
		return Timeline{}, ErrDisabled
	}
	if err := checkName(name); err != nil {
		return Timeline{}, err
	}
	client := m.newClient(name)
	var pts []RestorePoint
	truncated := false
	for level := litestream.SnapshotLevel; level >= 0 && !truncated; level-- {
		itr, err := client.LTXFiles(ctx, level, 0, false)
		if err != nil {
			return Timeline{}, fmt.Errorf("backup: timeline of %s: %w", name, err)
		}
		for itr.Next() {
			info := itr.Item()
			pts = append(pts, RestorePoint{
				At:       info.CreatedAt,
				TXID:     info.MaxTXID.String(),
				Level:    level,
				Snapshot: level == litestream.SnapshotLevel || info.MinTXID <= 1,
				Size:     info.Size,
			})
			if len(pts) >= maxRestorePoints {
				truncated = true
				break
			}
		}
		err = errors.Join(itr.Err(), itr.Close())
		if err != nil {
			return Timeline{}, fmt.Errorf("backup: timeline of %s: %w", name, err)
		}
	}
	if len(pts) == 0 {
		return Timeline{}, ErrNothingToRestore
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
	return Timeline{Earliest: pts[0].At, Latest: pts[len(pts)-1].At, Points: pts, Truncated: truncated}, nil
}

// Attach starts replicating the database at dbPath under name. It returns
// once the first sync has reached the replica, since until then a final sync
// does nothing. dbPath must exist and name must not be attached already.
//
// Call detach once, before the application's last checkpoint and close: it is
// the final sync. A cancelled context gets a fresh one for it, bounded by
// Litestream's shutdown sync timeout. On a disabled Manager detach does
// nothing.
//
// When the newest snapshot is older than the snapshot interval, Attach
// writes one in the background after the first sync, bounded by
// Config.SnapshotTimeout, so that a database attached only between
// maintenance passes still gets snapshots, and its WAL segments a floor to
// be pruned at. detach cancels it and waits for it to stop before the final
// sync, on S3 with the abort of the upload it cut off (10 s at most); a
// database detached before its snapshot is done gets one on its next Attach.
func (m *Manager) Attach(ctx context.Context, name, dbPath string) (detach func(context.Context) error, err error) {
	if !m.enabled {
		return func(context.Context) error { return nil }, nil
	}
	if err := checkName(name); err != nil {
		return nil, err
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("backup: attach %s: %w", name, err)
	}
	m.mu.Lock()
	if m.reserved[name] || m.attached[name] != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("backup: %s is already attached, or being detached or purged", name)
	}
	m.reserved[name] = true
	m.mu.Unlock()
	committed := false
	defer func() {
		if !committed {
			m.mu.Lock()
			delete(m.reserved, name)
			m.mu.Unlock()
		}
	}()

	db := litestream.NewDB(dbPath)
	db.SetLogger(m.log.With("replica", name))
	client, deletes := countDeletes(m.newClient(name))
	db.Replica = litestream.NewReplicaWithClient(db, client)
	if err := db.Open(); err != nil {
		return nil, fmt.Errorf("backup: attach %s: %w", name, err)
	}
	if err := db.SyncAndWait(ctx); err != nil {
		db.ShutdownSyncTimeout = 0 // one more try, not 30s of retries against a replica that just failed
		_ = db.Close(ctx)
		return nil, fmt.Errorf("backup: first sync of %s: %w", name, err)
	}

	a := &attachedDB{db: db, deletes: deletes, snapshotDone: make(chan struct{})}
	a.work, a.stopWork = context.WithCancel(context.Background())
	m.mu.Lock()
	delete(m.reserved, name)
	m.attached[name] = a
	committed = true
	m.mu.Unlock()
	go m.snapshotAfterAttach(name, a)

	var once sync.Once
	var derr error
	return func(ctx context.Context) error {
		once.Do(func() { derr = m.detach(ctx, name, a) })
		return derr
	}, nil
}

// snapshotAfterAttach writes a snapshot when one is due, under the lock that
// keeps a maintenance pass from snapshotting at the same time.
func (m *Manager) snapshotAfterAttach(name string, a *attachedDB) {
	ctx, cancel := context.WithTimeout(a.work, m.snapshotTimeout)
	defer cancel()
	a.maint.Lock()
	var err error
	if ctx.Err() == nil {
		_, err = m.snapshotIfDue(ctx, name, a.db, time.Now())
		if err != nil && ctx.Err() != nil {
			// Before snapshotDone, which detach waits for with the name
			// reserved, so the listing finds no other upload of the name.
			m.abortCancelledSnapshot(name)
		}
	}
	a.maint.Unlock()
	close(a.snapshotDone)
	switch {
	case err == nil:
	case a.work.Err() != nil:
		m.log.Debug("backup: snapshot after attach cancelled by detach", "replica", name, "error", err)
	default:
		m.log.Warn("backup: snapshot after attach failed; the next attached pass or attach takes one", "replica", name, "error", err)
	}
}

// detach keeps name reserved until the final sync is done, so that no
// second replicator starts on the file meanwhile.
func (m *Manager) detach(ctx context.Context, name string, a *attachedDB) error {
	m.mu.Lock()
	delete(m.attached, name)
	m.reserved[name] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.reserved, name)
		m.mu.Unlock()
	}()

	a.stopWork()
	<-a.snapshotDone
	a.maint.Lock()
	defer a.maint.Unlock()
	a.detached = true
	if ctx.Err() != nil {
		m.log.Warn("backup: detach called with a cancelled context; syncing with a fresh one", "replica", name)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), litestream.DefaultShutdownSyncTimeout)
		defer cancel()
	}
	if err := a.db.Close(ctx); err != nil {
		return fmt.Errorf("backup: final sync of %s: %w", name, err)
	}
	return nil
}

// ReplicaStatus is the state of one attached replica, read from memory.
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

// Status reports every attached replica, sorted by name. It reads no
// network, so it is cheap enough for a metrics scrape.
func (m *Manager) Status() []ReplicaStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ReplicaStatus, 0, len(m.attached))
	for name, a := range m.attached {
		s := ReplicaStatus{Name: name, Path: a.db.Path(), LastSyncAt: a.db.LastSuccessfulSyncAt()}
		if pos, err := a.db.Pos(); err == nil && !pos.IsZero() {
			s.TXID = pos.TXID.String()
		}
		if v, ok := m.verify[name]; ok {
			s.LastVerifyAt, s.VerifyOK = v.at, v.err == nil && !v.at.IsZero()
			s.VerifyAttempts, s.VerifyRetried = v.attempts, v.retried
		}
		if mr, ok := m.maintenance[name]; ok {
			s.LastSnapshotAt, s.ReplicaObjects, s.ReplicaBytes = mr.lastSnapshotAt, mr.objects, mr.bytes
			s.MaintenancePasses, s.PrunedObjects, s.MaintenanceSkipped = mr.passes, mr.pruned, mr.skipped
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// VerifyRestore proves the replica under name is restorable: it restores the
// latest state to a temporary file, runs PRAGMA integrity_check and removes
// it. The result shows in Status while the replica is attached; it is kept
// for a replica that is not, and shows once it is. A restore cut off by a
// cancelled ctx keeps no result, one cut off by its deadline fails, and one
// that overlaps PurgeReplica of the name keeps none. It reads the whole
// replica, so it belongs in a daily loop, not on a request.
func (m *Manager) VerifyRestore(ctx context.Context, name string) error {
	if !m.enabled {
		return ErrDisabled
	}
	if err := checkName(name); err != nil {
		return err
	}
	mark := m.purgeMark()
	start := time.Now()
	st, err := m.verifyRestore(ctx, name)
	took := time.Since(start).Round(time.Millisecond)
	switch {
	case err == nil:
		m.log.Info("backup: restore verified", "replica", name, "took", took, "bytes", st.bytes, "files", st.files)
	case errors.Is(ctx.Err(), context.Canceled):
		m.log.Warn("backup: restore verification cut off", "replica", name, "took", took, "error", err)
		return err
	default:
		m.log.Warn("backup: restore verification failed", "replica", name, "took", took, "error", err)
	}
	m.mu.Lock()
	if m.purges[name] <= mark {
		v := m.verify[name]
		v.at, v.err = time.Now(), err
		m.verify[name] = v
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) verifyRestore(ctx context.Context, name string) (restoreStats, error) {
	sweepStale(os.TempDir(), "lith-backup-verify-*")
	tmp, err := os.MkdirTemp("", "lith-backup-verify-")
	if err != nil {
		return restoreStats{}, fmt.Errorf("backup: verify %s: %w", name, err)
	}
	defer inUse(tmp)()
	st, err := m.restoreTo(ctx, m.newClient(name), filepath.Join(tmp, "verify.db"), time.Time{}, litestream.IntegrityCheckFull)
	return st, wrap(err, "verify", name)
}

// VerifyAll runs VerifyRestore on every attached replica, and stops when ctx
// is done.
func (m *Manager) VerifyAll(ctx context.Context) map[string]error {
	out := map[string]error{}
	for _, name := range m.attachedNames() {
		if ctx.Err() != nil {
			break
		}
		out[name] = m.VerifyRestore(ctx, name)
	}
	return out
}

func (m *Manager) attachedNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.attached))
	for name := range m.attached {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PurgeReplica deletes everything replicated under name, for an erasure:
// the objects, and on S3 the parts of every unfinished multipart upload
// under it, which a listing of objects does not show. It fails without
// deleting anything when those uploads cannot be listed or aborted, so that
// a retry finds them. The database must be detached first. Purging a name
// never replicated, or on a disabled Manager, does nothing.
func (m *Manager) PurgeReplica(ctx context.Context, name string) error {
	if !m.enabled {
		return nil
	}
	if err := checkName(name); err != nil {
		return err
	}
	// The reservation keeps an Attach out until the purge is done.
	m.mu.Lock()
	if m.reserved[name] || m.attached[name] != nil {
		m.mu.Unlock()
		return fmt.Errorf("backup: purge %s: still attached, or being attached, detached or purged; detach it first", name)
	}
	m.reserved[name] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.reserved, name)
		m.mu.Unlock()
	}()
	if _, err := m.abortUploads(ctx, name, -1); err != nil {
		return fmt.Errorf("backup: purge %s: %w", name, err)
	}
	if err := m.newClient(name).DeleteAll(ctx); err != nil {
		return fmt.Errorf("backup: purge %s: %w", name, err)
	}
	m.mu.Lock()
	m.purgeSeq++
	m.purges[name] = m.purgeSeq
	delete(m.verify, name)
	delete(m.maintenance, name)
	m.mu.Unlock()
	return nil
}
