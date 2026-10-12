package guestbook_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/template/internal/guestbook"
	"github.com/zweiundeins/lith/template/migrations"
)

func TestSign(t *testing.T) {
	db, w, touched := open(t)
	ctx := context.Background()
	sign := guestbook.Sign{Session: "s1", Tab: "t1", Op: "op1", Name: " Ada ", Message: "Hello", At: time.Unix(1_800_000_000, 0)}

	if err := w.Do(ctx, sign); err != nil {
		t.Fatal(err)
	}
	entries, total, state := read(t, db, "s1", "t1")
	if total != 1 || entries[0].Name != "Ada" || state.Op != "op1" || !state.Accepted() {
		t.Fatalf("signed: %d %+v, tab %+v", total, entries, state)
	}
	if got := touched(); !slices.Equal(got, []string{"guestbook", "tab:t1"}) {
		t.Errorf("a signature touched %v", got)
	}

	// The newest entry again: refused, and only the tab hears why.
	sign.Op = "op2"
	if err := w.Do(ctx, sign); err != nil {
		t.Fatal(err)
	}
	_, total, state = read(t, db, "s1", "t1")
	if total != 1 || state.Op != "op2" || state.Accepted() || state.Refusal == "" {
		t.Fatalf("repeated: %d entries, tab %+v", total, state)
	}
	if got := touched(); !slices.Equal(got, []string{"tab:t1"}) {
		t.Errorf("a refusal touched %v", got)
	}

	// A blank name never reaches the writer.
	sign.Op, sign.Name = "op3", "  "
	if err := w.Do(ctx, sign); !errors.Is(err, writer.ErrInvalid) {
		t.Errorf("a blank name: %v", err)
	}
}

func TestSweep(t *testing.T) {
	db, w, touched := open(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	fill := writer.Func(func(tx *writer.Tx) error {
		for range guestbook.Kept + 3 {
			if _, err := tx.Exec(`INSERT INTO entries (name, message, at) VALUES ('Ada', 'Hello', 0)`); err != nil {
				return err
			}
		}
		for tab, seen := range map[string]time.Time{"gone": now.Add(-guestbook.TabTTL - time.Hour), "here": now.Add(-time.Hour)} {
			if _, err := tx.Exec(`INSERT INTO tab_state (sid, tab_id, data, seen) VALUES ('s1', ?, '{}', ?)`, tab, seen.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	if err := w.Do(ctx, fill); err != nil {
		t.Fatal(err)
	}
	touched()

	if err := w.Do(ctx, guestbook.Sweep{At: now}); err != nil {
		t.Fatal(err)
	}
	var tabs []string
	var total int
	err := db.View(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT count(*) FROM entries`).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT tab_id FROM tab_state`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tab string
			if err := rows.Scan(&tab); err != nil {
				return err
			}
			tabs = append(tabs, tab)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != guestbook.Kept || !slices.Equal(tabs, []string{"here"}) {
		t.Errorf("after the sweep: %d entries, tabs %v", total, tabs)
	}
	if got := touched(); !slices.Equal(got, []string{"guestbook"}) {
		t.Errorf("the sweep touched %v", got)
	}
}

// open starts a writer on a new database and returns it with a function
// that reports the topics committed since its last call.
func open(t *testing.T) (*sqlite.DB, *writer.Writer, func() []string) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), sqlite.Options{
		Synchronous: "NORMAL",
		Migrations:  migrations.FS,
	})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var topics []string
	w, err := writer.New(db, writer.Options{OnCommit: func(ts []string) {
		mu.Lock()
		topics = append(topics, ts...)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(context.Background())
	t.Cleanup(func() {
		w.Close()
		db.Close()
	})
	return db, w, func() []string {
		mu.Lock()
		defer mu.Unlock()
		got := topics
		topics = nil
		return got
	}
}

func read(t *testing.T, db *sqlite.DB, sid, tab string) ([]guestbook.Entry, int, guestbook.TabState) {
	t.Helper()
	var entries []guestbook.Entry
	var total int
	var state guestbook.TabState
	err := db.View(context.Background(), func(tx *sql.Tx) error {
		var err error
		if entries, total, err = guestbook.Latest(context.Background(), tx, guestbook.Shown); err != nil {
			return err
		}
		state, err = guestbook.Tab(context.Background(), tx, sid, tab)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries, total, state
}
