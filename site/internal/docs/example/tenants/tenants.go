// Package tenants keeps one database per account: each account's boards
// in a file of their own, opened on first use and closed when idle, each
// with its own writer, and one hub for every account's views.
package tenants

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/tenant"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/docs/example/board"
	"github.com/zweiundeins/lith/site/internal/docs/example/board/migrations"
)

// Account is one account's open database and the writer that owns it.
type Account struct {
	DB *sqlite.DB
	W  *writer.Writer
}

// Close stops the writer, which applies what is queued, then closes the
// database. The pool calls it when it evicts the account.
func (a *Account) Close() error { return errors.Join(a.W.Close(), a.DB.Close()) }

// Accounts opens the accounts' databases on demand.
type Accounts struct {
	dir  string
	pool *tenant.Pool[*Account]
}

// ErrErased is the reason Do gives for an account that Erase deleted.
var ErrErased = errors.New("tenants: the account was erased")

// Open returns the accounts kept under dir, as dir/<key>.db, with at most
// maxOpen of them open while idle. Each account's commits mark the hub's
// views dirty under topics that start with the account (Topic).
func Open(dir string, maxOpen int, hub *live.Hub, log *slog.Logger) *Accounts {
	a := &Accounts{dir: dir}
	a.pool = tenant.New(maxOpen, func(ctx context.Context, key string) (*Account, error) {
		return open(ctx, a.path(key), key, hub)
	}, tenant.WithLogger(log))
	return a
}

// open opens an account's database and starts its writer. ctx carries the
// values of the request that needed the account, not its cancellation: a
// request that gives up does not abort the open halfway.
func open(ctx context.Context, path, key string, hub *live.Hub) (*Account, error) {
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
	return &Account{DB: db, W: w}, nil
}

// Topic is a topic of one account: board.Topic(7) of account "acme" is
// "acct:acme:board:7", so a commit of one account marks no view of another.
func Topic(key, topic string) string { return "acct:" + key + ":" + topic }

// path is the account's file. The key names a file, so only keys of
// lowercase letters, digits and dashes are accounts.
func (a *Accounts) path(key string) string { return filepath.Join(a.dir, key+".db") }

func validKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for _, c := range key {
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Do runs cmd on the account's writer, opening the account's database if
// it is closed. The account stays open while Do waits for the commit.
func (a *Accounts) Do(ctx context.Context, key string, cmd writer.Command) error {
	if !validKey(key) {
		return fmt.Errorf("tenants: %q is not an account", key)
	}
	return a.pool.Do(ctx, key, func(acct *Account) error { return acct.W.Do(ctx, cmd) })
}

// View runs fn in one read transaction of the account's database.
func (a *Accounts) View(ctx context.Context, key string, fn func(*sql.Tx) error) error {
	if !validKey(key) {
		return fmt.Errorf("tenants: %q is not an account", key)
	}
	return a.pool.Do(ctx, key, func(acct *Account) error { return acct.DB.View(ctx, fn) })
}

// CountView shows how many cards a board of the account holds. It pins
// the account for each render only: a page that stays open keeps no
// database from closing, and the next render opens it again.
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

// Erase deletes an account's data for good: Deny refuses every later
// open, Evict closes the database now, and the files go.
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

// Stats says how full the pool is. Pinned near MaxOpen is the state to
// alert on: nothing is left to evict, and every account that opens grows
// the pool past its cap.
func (a *Accounts) Stats() tenant.Stats { return a.pool.Stats() }

// Close closes every account's database, after its writer drained.
func (a *Accounts) Close() error { return a.pool.Close() }
