---
title: sqlite
summary: Opens one SQLite database file the way the projects use it: a single write connection, for migrations and the writer, and a pool of read-only connections for everything else.
order: 1
---

Import path `github.com/zweiundeins/lith/sqlite`. Generated from the doc comments in [sqlite/](https://github.com/zweiundeins/lith/tree/main/sqlite).

## Functions

### func Migrate {#Migrate}

```go
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) (err error)
```

Migrate applies the NNN\_name.sql files at the root of fsys that are numbered above the database's PRAGMA user\_version, as Open does with Options.Migrations: in order, each in a transaction of its own, and it refuses a database whose version is newer than the newest file. db is the write connection (DB.W), or any connection to the file in a test or a tool.

Foreign keys are off meanwhile, as SQLite's procedure for schema changes asks: otherwise rebuilding a table would cascade the deletes of its old copy. foreign\_key\_check runs before each commit instead.

### func TxFrom {#TxFrom}

```go
func TxFrom(ctx context.Context) (*sql.Tx, bool)
```

TxFrom returns the transaction ctx carries (WithTx, or a live Render's). It does not tell which database the transaction reads; View checks that.

### func WithTx {#WithTx}

```go
func WithTx(ctx context.Context, tx *sql.Tx) context.Context
```

WithTx returns a copy of ctx that carries tx. A View called with it, or with a context made from it, runs in tx when tx reads the same database file, so code that takes a ctx joins the caller's snapshot. A live Render's ctx carries its worker's transaction this way.

## Types

### type Checkpoint {#Checkpoint}

```go
type Checkpoint int
```

Checkpoint says who copies the WAL back into the database file.

```go
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
```

### type DB {#DB}

```go
type DB struct {
	W    *sql.DB
	R    *sql.DB
	Path string
	// contains filtered or unexported fields
}
```

DB is one database file. W holds a single connection that takes the write lock when a transaction begins (BEGIN IMMEDIATE); only migrations and the writer use it, and it is nil with Options.ReadOnly. R is the pool of query\_only connections.

#### func Open {#Open}

```go
func Open(ctx context.Context, path string, opts Options) (*DB, error)
```

Open opens (and creates) the database at path, applies the migrations and opens the read pool. With Options.ReadOnly it opens the read pool of an existing database only.

#### func (*DB) Backup {#DB.Backup}

```go
func (d *DB) Backup(ctx context.Context, dst string) error
```

Backup writes a consistent copy of the database to dst (VACUUM INTO) on a read-only connection of its own, while the database stays in use. dst must not exist; on failure it is removed again.

#### func (*DB) Close {#DB.Close}

```go
func (d *DB) Close() error
```

Close closes both pools. Close the writer first.

#### func (*DB) Options {#DB.Options}

```go
func (d *DB) Options() Options
```

Options returns the options Open used, defaults filled in.

#### func (*DB) View {#DB.View}

```go
func (d *DB) View(ctx context.Context, fn func(*sql.Tx) error) error
```

View runs fn in one read transaction on R: every query in fn sees the same snapshot of the database. When ctx carries a transaction on the same database file (WithTx, or a live Render's ctx), fn runs in that one instead, and sees the caller's snapshot; its owner ends it.

#### func (*DB) ViewValue {#DB.ViewValue}

```go
func (d *DB) ViewValue[T any](ctx context.Context, fn func(*sql.Tx) (T, error)) (T, error)
```

ViewValue runs fn like View and returns what it returned, or the zero value and the error.

```go
n, err := db.ViewValue(ctx, func(tx *sql.Tx) (n int, err error) {
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM notes`).Scan(&n)
	return n, err
})
```

### type Options {#Options}

```go
type Options struct {
	// Synchronous is "NORMAL" (the default) or "FULL". NORMAL syncs the WAL
	// only at checkpoints: a power loss or kernel crash can undo the commits
	// since, up to about 35 s of them with Linux's default writeback; a
	// crash of the process loses none. FULL syncs every commit, one fsync
	// per batch. ReadOnly needs neither.
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
	// CacheKiB is the page cache of each connection (default 32000).
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
}
```

Options configure Open; zero fields take the defaults.

### type Querier {#Querier}

```go
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
```

Querier is the method set for queries that \*sql.DB, \*sql.Tx and \*sql.Conn share, and \*writer.Tx as well: query code that takes a Querier serves a GET's View, a live Render and a command alike.

#### func QuerierFrom {#QuerierFrom}

```go
func QuerierFrom(ctx context.Context) (Querier, bool)
```

QuerierFrom returns a Querier for the transaction ctx carries. In a live Render it runs each SQL text on a statement that the worker prepared once on its connection, in the render's transaction: modernc.org/sqlite parses and plans every query that has no prepared statement. The worker keeps the 512 statements used last; SQL built at runtime, such as an IN list with a mark per value, runs on the Render's tx, which keeps none. As with a command's Tx, scan a row before the same query runs again: until then its statement is busy. Elsewhere it is the transaction from WithTx. Like TxFrom, it does not tell which database the transaction reads.
