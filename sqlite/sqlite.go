// Package sqlite opens one SQLite database file the way the projects use
// it: a single write connection, for migrations and the writer, and a pool
// of read-only connections for everything else. Each read connection keeps
// the statements it ran last prepared (Options.Statements), so a query
// that comes again skips modernc.org/sqlite's parse and plan.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Checkpoint says who copies the WAL back into the database file.
type Checkpoint int

const (
	// CheckpointAuto leaves it to SQLite: a commit that leaves more than
	// 1000 pages in the WAL checkpoints it.
	CheckpointAuto Checkpoint = iota
	// CheckpointWriter turns SQLite's checkpoints off on the write
	// connection; the writer checkpoints between batches instead.
	CheckpointWriter
	// CheckpointExternal turns them off and leaves checkpoints to another
	// process, such as Litestream.
	CheckpointExternal
)

// Options configure Open; zero fields take the defaults.
type Options struct {
	// Synchronous is "NORMAL" (the default), "FULL" or "OFF". NORMAL syncs the WAL
	// only at checkpoints: a power loss or kernel crash can undo the commits
	// since, up to about 35 s of them with Linux's default writeback; a
	// crash of the process loses none. FULL syncs every commit, one fsync
	// per batch. "OFF" syncs nothing at all: a crash of the process can
	// still lose nothing, but a power loss or kernel crash can corrupt the
	// file, so it is for databases that die with the process, such as
	// tests and scratch files. ReadOnly needs none of them.
	Synchronous string
	// ReadOnly opens an existing database for reading only, for a CLI or
	// a check of a backup. Open then never creates the file or its
	// directory, opens no write connection (W is nil) and applies no
	// migrations: setting Migrations or Migrate is an error.
	ReadOnly bool
	// Migrations holds NNN_name.sql files at its root (fs.Sub an embed.FS
	// to its directory). Open applies those numbered above the database's
	// PRAGMA user_version in order, each in a transaction of its own, and
	// refuses a database whose version is newer than the newest file.
	Migrations fs.FS
	// Migrate is a project's own migration runner, for one that keeps its
	// schema_migrations table, say. Open calls it instead of applying
	// Migrations (set one of them, not both), with the write connection,
	// before it opens the read pool. A runner that changes the
	// connection's pragmas (foreign_keys) must set them back: the writer
	// takes the connection over.
	Migrate func(ctx context.Context, w *sql.DB) error
	// Readers is the size of the read pool (default GOMAXPROCS + 4, at
	// least 8). A live hub's render workers each hold one of its
	// connections for the hub's life, GOMAXPROCS of them by default, and
	// GET documents and every other read share the rest: size it as the
	// hub's Workers plus the reads that run at once besides the hub.
	Readers int
	// CacheKiB is the page cache of each connection (default 32000). Readers
	// and the hub's Workers grow with GOMAXPROCS, and so does the cache they
	// can hold: 36 readers on a 32-core desktop may hold over 1 GiB. That
	// suits a server. A desktop or background app pins Readers, CacheKiB and
	// the hub's Workers instead (8, 4000 and 4, say): reads of a file that
	// MmapBytes covers go through the mapping, and a large cache per
	// connection then buys little.
	CacheKiB int
	// MmapBytes is how much of the file each reader maps (default 256 MiB,
	// negative: none).
	MmapBytes int64
	// BusyTimeout is how long a connection waits for a lock (default 5s).
	BusyTimeout time.Duration
	// Checkpoint picks who checkpoints the WAL (default CheckpointAuto).
	Checkpoint Checkpoint
	// Driver is the database/sql driver every connection opens with
	// (default "sqlite", modernc.org/sqlite's), such as a wrapper of it
	// that dbmetrics.Register named. It must be modernc's driver
	// underneath: the DSN carries its _pragma parameters, and with
	// CheckpointWriter the writer reaches its connection through Unwrap,
	// as dbmetrics' connections allow.
	Driver string
	// Statements is how many prepared statements each read connection
	// keeps, the ones used last (default 128, negative: none). A query on
	// R, in View or anywhere else, runs on its connection's statement for
	// its SQL text instead of having modernc.org/sqlite parse and plan it
	// again; while that statement's rows are open, the query runs on one
	// of its own. A statement holds its compiled program, about 4 KiB for
	// the queries of kanban's board page, so a full cache takes about
	// 0.5 MiB per connection, and a connection keeps only what it ran.
	// Build SQL from constants: SQL built at runtime, such as an IN list
	// with a mark per value, takes a place for each text it makes, unless
	// it runs with a ctx from Uncached. A connection reached through
	// sql.Conn.Raw Unwraps to the driver's.
	Statements int
}

func (o Options) withDefaults() (Options, error) {
	switch s := strings.ToUpper(o.Synchronous); s {
	case "FULL", "NORMAL", "OFF":
		o.Synchronous = s
	case "":
		if !o.ReadOnly {
			o.Synchronous = "NORMAL"
		}
	default:
		return o, fmt.Errorf("sqlite: Options.Synchronous is %q, want FULL, NORMAL or OFF", o.Synchronous)
	}
	if o.ReadOnly && (o.Migrations != nil || o.Migrate != nil) {
		return o, errors.New("sqlite: a ReadOnly database cannot be migrated")
	}
	switch o.Checkpoint {
	case CheckpointAuto, CheckpointWriter, CheckpointExternal:
	default:
		return o, fmt.Errorf("sqlite: unknown Options.Checkpoint %d", o.Checkpoint)
	}
	if o.Migrations != nil && o.Migrate != nil {
		return o, errors.New("sqlite: set Options.Migrations or Options.Migrate, not both")
	}
	if o.Readers <= 0 {
		o.Readers = max(8, runtime.GOMAXPROCS(0)+4)
	}
	if o.CacheKiB <= 0 {
		o.CacheKiB = 32000
	}
	if o.MmapBytes == 0 {
		o.MmapBytes = 256 << 20
	}
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = 5 * time.Second
	}
	if o.Driver == "" {
		o.Driver = "sqlite"
	}
	if o.Statements == 0 {
		o.Statements = 128
	}
	return o, nil
}

// DB is one database file. W holds a single connection that takes the
// write lock when a transaction begins (BEGIN IMMEDIATE); only migrations
// and the writer use it, and it is nil with Options.ReadOnly. R is the
// pool of read connections: each opens the file read-only (mode=ro) and
// is query_only, so a statement that runs on one cannot write the file,
// not even one that starts with PRAGMA query_only = 0. A statement can
// still ATTACH another file the process may open, which mode=ro does not
// cover: it can read it, and after that pragma write to it. Only an
// authorizer could refuse ATTACH, which modernc.org/sqlite does not
// expose. So bind every value, and build SQL texts from constants only.
type DB struct {
	W    *sql.DB
	R    *sql.DB
	Path string
	opts Options
	file string // as SQLite names it, to recognise a transaction on it
}

// Open opens (and creates) the database at path, applies the migrations
// and opens the read pool. With Options.ReadOnly it opens the read pool
// of an existing database only.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("sqlite: no path")
	}
	common := []string{
		fmt.Sprintf("busy_timeout(%d)", opts.BusyTimeout.Milliseconds()),
		"foreign_keys(1)",
		"temp_store(MEMORY)",
		fmt.Sprintf("cache_size(%d)", -opts.CacheKiB),
	}
	var w *sql.DB
	if opts.ReadOnly {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("sqlite: open read-only: %w", err)
		}
	} else {
		wp := append(slices.Clone(common), "journal_mode(WAL)", "synchronous("+opts.Synchronous+")")
		if w, err = openWriter(ctx, path, opts, wp); err != nil {
			return nil, err
		}
	}

	// The readers open the file read-only, and query_only besides: a
	// pragma in a statement's text can turn query_only off, but not the
	// handle's mode.
	ro := url.Values{"mode": {"ro"}}
	rp := append(slices.Clone(common), "query_only(1)")
	if opts.MmapBytes > 0 {
		rp = append(rp, fmt.Sprintf("mmap_size(%d)", opts.MmapBytes))
	}
	var file string
	r, err := openReaders(opts, dsn(path, rp, ro))
	if err == nil {
		r.SetMaxOpenConns(opts.Readers)
		r.SetMaxIdleConns(opts.Readers)
		r.SetConnMaxLifetime(0)
		r.SetConnMaxIdleTime(0)
		if file, err = mainFile(ctx, r); err != nil {
			r.Close()
			err = fmt.Errorf("sqlite: open %s: %w", path, err)
		}
	}
	if err != nil {
		if w != nil {
			w.Close()
		}
		return nil, err
	}
	return &DB{W: w, R: r, Path: path, opts: opts, file: file}, nil
}

// openReaders opens the read pool, with a statement cache on each
// connection unless Options.Statements turns it off.
func openReaders(opts Options, dsn string) (*sql.DB, error) {
	if opts.Statements < 0 {
		return sql.Open(opts.Driver, dsn)
	}
	c, err := connector(opts.Driver, dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(&stmtConnector{base: c, max: opts.Statements}), nil
}

// openWriter opens the write connection, checks that the database is in
// WAL mode and migrates it.
func openWriter(ctx context.Context, path string, opts Options, pragmas []string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	wp := slices.Clone(pragmas)
	if opts.Checkpoint != CheckpointAuto {
		wp = append(wp, "wal_autocheckpoint(0)")
	}
	w, err := sql.Open(opts.Driver, dsn(path, wp, url.Values{"_txlock": {"immediate"}}))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	w.SetConnMaxIdleTime(0)
	var mode string
	if err := w.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		w.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	if mode != "wal" {
		w.Close()
		return nil, fmt.Errorf("sqlite: open %s: journal mode is %s, not wal", path, mode)
	}
	if opts.Migrations != nil {
		if err := Migrate(ctx, w, opts.Migrations); err != nil {
			w.Close()
			return nil, err
		}
	}
	if opts.Migrate != nil {
		if err := opts.Migrate(ctx, w); err != nil {
			w.Close()
			return nil, fmt.Errorf("sqlite: migrate %s: %w", path, err)
		}
	}
	return w, nil
}

// Options returns the options Open used, defaults filled in.
func (d *DB) Options() Options { return d.opts }

// View runs fn in one read transaction on R: every query in fn sees the
// same snapshot of the database. When ctx carries a transaction on the
// same database file (WithTx, or a live Render's ctx), fn runs in that
// one instead, and sees the caller's snapshot; its owner ends it.
func (d *DB) View(ctx context.Context, fn func(*sql.Tx) error) error {
	if tx := d.joined(ctx); tx != nil {
		return fn(tx)
	}
	tx, err := d.R.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// ViewValue runs fn like View and returns what it returned, or the zero
// value and the error.
//
//	n, err := db.ViewValue(ctx, func(tx *sql.Tx) (n int, err error) {
//		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM notes`).Scan(&n)
//		return n, err
//	})
func (d *DB) ViewValue[T any](ctx context.Context, fn func(*sql.Tx) (T, error)) (T, error) {
	var v T
	err := d.View(ctx, func(tx *sql.Tx) (err error) {
		v, err = fn(tx)
		return err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}

// Backup writes a consistent copy of the database to dst (VACUUM INTO) on
// a read-only connection of its own, while the database stays in use. dst
// must not exist; on failure it is removed again.
func (d *DB) Backup(ctx context.Context, dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("sqlite: backup: %w", err)
	}
	f.Close()
	if err := d.backup(ctx, dst); err != nil {
		os.Remove(dst)
		return fmt.Errorf("sqlite: backup to %s: %w", dst, err)
	}
	return nil
}

func (d *DB) backup(ctx context.Context, dst string) error {
	pragmas := []string{fmt.Sprintf("busy_timeout(%d)", d.opts.BusyTimeout.Milliseconds())}
	c, err := sql.Open(d.opts.Driver, dsn(d.Path, pragmas, url.Values{"mode": {"ro"}}))
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetMaxOpenConns(1)
	if _, err := c.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return err
	}
	f, err := os.Open(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Checkpointer opens a connection of its own for checkpoints of the WAL
// beside the write connection (writer.Options.CheckpointAside), with
// SQLite's automatic checkpoints off. The caller closes it.
func (d *DB) Checkpointer() (*sql.DB, error) {
	if d.W == nil {
		return nil, errors.New("sqlite: a ReadOnly database has no checkpoints")
	}
	pragmas := []string{
		fmt.Sprintf("busy_timeout(%d)", d.opts.BusyTimeout.Milliseconds()),
		"synchronous(" + d.opts.Synchronous + ")",
		"wal_autocheckpoint(0)",
	}
	c, err := sql.Open(d.opts.Driver, dsn(d.Path, pragmas, nil))
	if err != nil {
		return nil, err
	}
	c.SetMaxOpenConns(1)
	c.SetMaxIdleConns(1)
	c.SetConnMaxLifetime(0)
	c.SetConnMaxIdleTime(0)
	return c, nil
}

// Close closes both pools. Close the writer first.
func (d *DB) Close() error {
	err := d.R.Close()
	if d.W != nil {
		err = errors.Join(err, d.W.Close())
	}
	return err
}

// uriPath escapes what SQLite's URI parser would read as the end of the
// path, or as an escape.
var uriPath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// dsn is a URI for the driver, which runs the pragmas on every connection
// it opens: busy_timeout first, then the rest in lexical order.
func dsn(path string, pragmas []string, extra url.Values) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	maps.Copy(q, extra)
	return "file:" + uriPath.Replace(path) + "?" + q.Encode()
}
