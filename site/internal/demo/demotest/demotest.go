// Package demotest gives a demo's tests a database with the site's schema
// and the demo's, and a running writer that records the topics it
// commits.
package demotest

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/migrate"
	"github.com/zweiundeins/lith/site/migrations"
)

// Env is a demo's database and writer for a test.
type Env struct {
	DB *sqlite.DB
	W  *writer.Writer

	mu     sync.Mutex
	topics []string
}

// Open migrates a new database with the site's scope and the demo's, and
// starts a writer on it, both closed when the test ends.
func Open(t testing.TB, d demo.Demo) *Env {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "demo.db"), sqlite.Options{
		Migrate: func(ctx context.Context, w *sql.DB) error {
			return migrate.Run(ctx, w, []migrate.Scope{{Name: "site", FS: migrations.FS}, {Name: d.Slug, FS: d.Migrations}})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{DB: db}
	e.W, err = writer.New(db, writer.Options{OnCommit: func(ts []string) {
		e.mu.Lock()
		e.topics = append(e.topics, ts...)
		e.mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	go e.W.Run(context.Background())
	t.Cleanup(func() {
		e.W.Close()
		db.Close()
	})
	return e
}

// Touched returns the topics committed since its last call, sorted.
func (e *Env) Touched() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	got := e.topics
	e.topics = nil
	slices.Sort(got)
	return slices.Compact(got)
}

// Do runs a command and fails the test on its error.
func (e *Env) Do(t testing.TB, cmd writer.Command) {
	t.Helper()
	if err := e.W.Do(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
}

// View runs fn in a read transaction and fails the test on its error.
func (e *Env) View(t testing.TB, fn func(ctx context.Context, tx *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := e.DB.View(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
}
