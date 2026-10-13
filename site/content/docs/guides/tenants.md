---
title: One database per account
nav: Tenants
summary: Keep each account's data in a file of its own with tenant.Pool, a writer per account and one hub for every account's views, and erase an account for good.
order: 8
---

An app whose accounts never share data can keep each account in a SQLite file of its own. A query then only ever sees one account's rows, so data cannot leak between accounts through a forgotten `WHERE`, and backing up, restoring or erasing one account is a matter of one file. The price is an open database, with its connections and its writer, for every account in use. `lith/tenant` keeps that bounded: a pool opens an account's database on first use, and closes idle ones, least recently released first, when more than its cap are open.

The board app's accounts are in `site/internal/docs/example/tenants`.

## An account

An account is its open database and the writer that owns it. One database has one writer, so each account gets its own. The pool closes an account through its `Close`, which lets the writer apply what is queued before the database closes:

```go source=site/internal/docs/example/tenants/tenants.go
// Account is one account's open database and the writer that owns it.
type Account struct {
	DB *sqlite.DB
	W  *writer.Writer
}

// Close stops the writer, which applies what is queued, then closes the
// database. The pool calls it when it evicts the account.
func (a *Account) Close() error { return errors.Join(a.W.Close(), a.DB.Close()) }
```

## The pool

`tenant.New(maxOpen, open)` returns a pool of values keyed by string. The opener runs outside the pool's lock, once per key: everyone else who needs that key waits for it, so a slow open, such as a restore from a replica, holds up only its own account, and an account never opens twice.

```go source=site/internal/docs/example/tenants/tenants.go
func Open(dir string, maxOpen int, hub *live.Hub, log *slog.Logger) *Accounts {
	a := &Accounts{dir: dir}
	a.pool = tenant.New(maxOpen, func(ctx context.Context, key string) (*Account, error) {
		return open(ctx, a.path(key), key, hub)
	}, tenant.WithLogger(log))
	return a
}
```

The opener opens the file with small settings, since many are open at once, and starts the account's writer. Its commits mark the hub's views under topics that carry the account, so a commit of one account marks no view of another:

```go source=site/internal/docs/example/tenants/tenants.go
	db, err := sqlite.Open(ctx, path, sqlite.Options{
		Migrations: migrations.FS,
		Readers:    2,    // few readers: an account's renders and GETs are few at a time
		CacheKiB:   2000, // many files are open at once: a small page cache for each
		Checkpoint: sqlite.CheckpointWriter,
	})
	if err != nil {
		return nil, err
	}
	w, err := writer.New(db, writer.Options{OnCommit: func(topics []string) {
		scoped := make([]string, len(topics))
		for i, t := range topics {
			scoped[i] = Topic(key, t)
		}
		hub.Changed(scoped)
	}})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	go w.Run(context.Background()) // until Close: the pool evicts the account
```

The opener's context carries the values of the request that needed the account, but not its cancellation: a request that gives up does not abort an open halfway. The commands stay the board's, unaware of accounts. The account's key names a file, so the pool accepts only keys made of lowercase letters, digits and dashes.

The migrations run when an account opens, so an account unused for months gets every migration since at its next open. A deploy that changes the schema migrates the accounts one by one, as they come back.

## Commands and reads

A command or a read pins its account for as long as it runs, with `pool.Do`:

```go source=site/internal/docs/example/tenants/tenants.go
func (a *Accounts) Do(ctx context.Context, key string, cmd writer.Command) error {
	if !validKey(key) {
		return fmt.Errorf("tenants: %q is not an account", key)
	}
	return a.pool.Do(ctx, key, func(acct *Account) error { return acct.W.Do(ctx, cmd) })
}
```

`pool.Get(ctx, key)` returns the value and a `release` function, for a pin that spans more than one call. Hold a pin for one unit of work, a request or a render: the pool evicts only accounts nobody holds.

## Views of an account

One hub serves every account. It needs no read pool of its own, `live.Options.Read` stays nil, and each view reads from its account's file. A view pins its account for each render only:

```go source=site/internal/docs/example/tenants/tenants.go
func (a *Accounts) CountView(key string, id int64) live.View {
	topic := Topic(key, board.Topic(id))
	return live.View{
		Key:    topic,
		Topics: []string{topic},
		Render: func(ctx context.Context, _ *sql.Tx) (any, error) {
			var n int
			err := a.View(ctx, key, func(tx *sql.Tx) error {
				b, err := board.Load(ctx, tx, id)
				for _, l := range b.Lanes {
					n += len(l.Cards)
				}
				return err
			})
			if err != nil {
				return nil, err
			}
			return sse.PatchElements(fmt.Appendf(nil, `<p id="cards">Cards on the board: %d</p>`, n)), nil
		},
	}
}
```

A stream that pinned its account for its whole life would keep the account open as long as the tab stays open, and push the pool past its cap. Pinned per render, an account whose tabs sit idle closes, and its next change opens it again.

An app that keeps shared data in a database of its own, a control plane of users and accounts say, gives that database to the hub's `Read`. A render's `db.View(ctx, fn)` on an account's database then sees that the render's transaction reads another file, and opens a transaction of its own on the account's pool.

## Erasing an account

`Deny(key, reason)` makes every later `Get` of the key fail with `reason`, and `Evict(ctx, key)` closes the account now, pinned or not. Together they close an account for good, before its files go:

```go source=site/internal/docs/example/tenants/tenants.go
func (a *Accounts) Erase(ctx context.Context, key string) error {
	if !validKey(key) {
		return fmt.Errorf("tenants: %q is not an account", key)
	}
	a.pool.Deny(key, ErrErased)
	if err := a.pool.Evict(ctx, key); err != nil {
		return err
	}
	var errs []error
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(a.path(key) + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

A restore runs the same way: `Deny`, `Evict`, put the restored file in place, then `Allow(key)` lifts the denial. Pins held on an evicted value go stale: releasing them does nothing. An account replicated with Litestream also has its replica purged and its Litestream directory removed ([Backups](/docs/guides/backups#erasing-an-account)).

## Sizing the pool

`maxOpen` is a soft cap: `Get` never waits for a free slot, so pinned accounts can exceed it. Each open account holds a write connection, its readers with their page caches, and a writer goroutine: at the settings above, up to 2,000 KiB of page cache for each of three connections. `pool.Stats()` reports the accounts open, pinned and opening, the cap, and the opens and evictions since start. `Pinned` close to `MaxOpen` is the state to alert on: nothing is left to evict, and every account that opens grows the pool past its cap.
