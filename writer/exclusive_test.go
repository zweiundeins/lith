package writer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// PRAGMA foreign_keys does nothing inside a transaction, so a toggle takes
// an Exclusive. Commands check foreign keys again after it, with the
// statements they had prepared before it.
func TestExclusiveForeignKeys(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	err := f.w.Do(ctx, Func(func(tx *Tx) error {
		if _, err := tx.Exec("CREATE TABLE parents (id INTEGER PRIMARY KEY)"); err != nil {
			return err
		}
		_, err := tx.Exec("CREATE TABLE children (id INTEGER PRIMARY KEY, parent INTEGER NOT NULL REFERENCES parents (id))")
		return err
	}))
	if err != nil {
		t.Fatal(err)
	}
	orphan := func(id int) Func {
		return func(tx *Tx) error {
			_, err := tx.Exec("INSERT INTO children (id, parent) VALUES (?, 99)", id)
			return err
		}
	}
	if err := f.w.Do(ctx, orphan(1)); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("an orphan from a command: %v", err)
	}
	err = f.w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return err
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
		_, err := conn.ExecContext(ctx, "INSERT INTO children (id, parent) VALUES (2, 99)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM children WHERE id = 2"); n != 1 {
		t.Fatal("the orphan from the Exclusive is missing")
	}
	if err := f.w.Do(ctx, orphan(3)); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("an orphan from a command after the Exclusive: %v", err)
	}
}

// ATTACH and DETACH are refused inside a transaction.
func TestExclusiveAttach(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	legacy := filepath.Join(t.TempDir(), "legacy.db")
	old, err := sql.Open("sqlite", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec("CREATE TABLE people (name TEXT); INSERT INTO people VALUES ('grace'), ('ada')"); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if err := f.w.Do(ctx, insert("before")); err != nil {
		t.Fatal(err)
	}

	err = f.w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS legacy", legacy); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO items (name) SELECT name FROM legacy.people ORDER BY name"); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "DETACH DATABASE legacy")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM items WHERE name IN ('ada', 'grace')"); n != 2 {
		t.Fatalf("%d people copied, want 2", n)
	}
	var dbs []string
	err = f.w.Do(ctx, Func(func(tx *Tx) error {
		rows, err := tx.Query("PRAGMA database_list")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var seq int
			var name string
			var file sql.NullString
			if err := rows.Scan(&seq, &name, &file); err != nil {
				return err
			}
			dbs = append(dbs, name)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return insert("after")(tx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(dbs, "legacy") {
		t.Fatalf("the writer's connection still has %v", dbs)
	}
}

// VACUUM is refused inside a transaction, and while a statement of the
// connection is under way.
func TestExclusiveVacuum(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	fill(t, f, 20)
	if err := f.w.Do(ctx, Func(func(tx *Tx) error { _, err := tx.Exec("DELETE FROM blobs"); return err })); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "PRAGMA freelist_count"); n < 100 {
		t.Fatalf("%d free pages after the delete", n)
	}
	err := f.w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "VACUUM")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "PRAGMA freelist_count"); n != 0 {
		t.Fatalf("%d free pages after VACUUM", n)
	}
	if err := f.w.Do(ctx, insert("after")); err != nil {
		t.Fatal(err)
	}
}

// A read connection keeps its statement for a query across an Exclusive
// that changes the schema: SQLite prepares it again, with the new column.
func TestExclusiveSchemaChangeAndTheReadPool(t *testing.T) {
	f := start(t, sqlite.Options{Readers: 1}, Options{})
	ctx := context.Background()
	if err := f.w.Do(ctx, insert("a")); err != nil {
		t.Fatal(err)
	}
	read := func() (cols []string, row []any) {
		t.Helper()
		err := f.db.View(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, "SELECT * FROM items ORDER BY id")
			if err != nil {
				return err
			}
			defer rows.Close()
			if cols, err = rows.Columns(); err != nil {
				return err
			}
			row = make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range row {
				ptrs[i] = &row[i]
			}
			if !rows.Next() {
				return errors.New("no row")
			}
			return rows.Scan(ptrs...)
		})
		if err != nil {
			t.Fatal(err)
		}
		return cols, row
	}
	if cols, _ := read(); !slices.Equal(cols, []string{"id", "name", "seq"}) {
		t.Fatalf("columns %v", cols)
	}
	err := f.w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "ALTER TABLE items ADD COLUMN note TEXT NOT NULL DEFAULT 'n'")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if cols, row := read(); !slices.Equal(cols, []string{"id", "name", "seq", "note"}) || row[3] != "n" {
		t.Fatalf("after the ALTER: %v %v", cols, row)
	}
	if err := f.w.Do(ctx, insert("b")); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM items WHERE note = 'n'"); n != 2 {
		t.Fatalf("%d items with the new column's default", n)
	}
}

// fn runs after the commands queued before it and before those queued
// after it; while it runs, commands wait.
func TestExclusiveOrder(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	add := func(i int) Func {
		return func(tx *Tx) error {
			_, err := tx.Exec("INSERT INTO items (name, seq) VALUES (?, ?)", fmt.Sprint("item", i), i)
			return err
		}
	}
	release := f.block(t)
	for i := range 3 {
		if err := f.w.Send(add(i)); err != nil {
			t.Fatal(err)
		}
	}
	running, gate := make(chan struct{}), make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(open) // before Close, which waits for fn
	seen := -1
	done := make(chan error, 1)
	go func() {
		done <- f.w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
			close(running)
			<-gate
			return conn.QueryRowContext(ctx, "SELECT count(*) FROM items").Scan(&seen)
		})
	}()
	f.queued(t, 4)
	release()
	<-running
	for i := 3; i < 6; i++ {
		if err := f.w.Send(add(i)); err != nil {
			t.Fatal(err)
		}
	}
	did := make(chan error, 1)
	go func() { did <- f.w.Do(ctx, add(6)) }()
	f.queued(t, 4)
	if n := f.count(t, "SELECT count(*) FROM items"); n != 3 {
		t.Fatalf("%d items while fn runs, want the 3 sent before it", n)
	}
	open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-did; err != nil {
		t.Fatal(err)
	}
	if seen != 3 {
		t.Fatalf("fn saw %d items, want the 3 sent before it", seen)
	}
	rows, err := f.db.R.Query("SELECT seq FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []int
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		order = append(order, seq)
	}
	if !slices.Equal(order, []int{0, 1, 2, 3, 4, 5, 6}) {
		t.Fatalf("applied in the order %v", order)
	}
}

// What fn leaves behind does not break the writer: an open transaction,
// a panic, a dropped table the writer made. A caller that gives up before
// fn's turn leaves fn unrun.
func TestExclusiveCleansUp(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()

	err := f.w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO items (name) VALUES ('left open')")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "transaction open") {
		t.Fatalf("a transaction left open: %v", err)
	}
	if n := f.count(t, "SELECT count(*) FROM items"); n != 0 {
		t.Fatal("the transaction left open was kept")
	}

	err = f.w.Exclusive(ctx, func(context.Context, *sql.Conn) error { panic("broken import") })
	if !errors.Is(err, ErrPanic) || !strings.Contains(err.Error(), "broken import") {
		t.Fatalf("a panic: %v", err)
	}
	if err := f.w.Do(ctx, insert("after the panic")); err != nil {
		t.Fatal(err)
	}

	if err := f.w.Do(ctx, Once("op1", increment(1))); err != nil {
		t.Fatal(err)
	}
	err = f.w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "DROP TABLE lith_ops")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.w.Do(ctx, Once("op2", increment(1))); err != nil {
		t.Fatalf("Once after its table was dropped: %v", err)
	}

	release := f.block(t)
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	var ran atomic.Bool
	err = f.w.Exclusive(short, func(context.Context, *sql.Conn) error { ran.Store(true); return nil })
	release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exclusive past its deadline: %v", err)
	}
	if err := f.w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Fatal("fn ran after its caller gave up")
	}

	if err := f.w.Exclusive(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a nil fn: %v", err)
	}
	f.w.Close()
	if err := f.w.Exclusive(ctx, func(context.Context, *sql.Conn) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Exclusive after Close: %v", err)
	}
}
