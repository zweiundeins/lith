package sqlite

import (
	"context"
	"sync"
	"testing"
	"time"
)

// burst holds n connections of the pool at once and returns them.
func burst(t *testing.T, d *DB, n int) {
	t.Helper()
	ctx := context.Background()
	var cs []interface{ Close() error }
	for range n {
		c, err := d.R.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var x int
		if err := c.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&x); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	if got := d.R.Stats().OpenConnections; got != n {
		t.Fatalf("the burst opened %d connections, want %d", got, n)
	}
	for _, c := range cs {
		c.Close()
	}
}

func count(t *testing.T, d *DB) {
	t.Helper()
	var n int
	if err := d.R.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a read after the burst: %d, %v", n, err)
	}
}

var oneRow = files(map[string]string{"001_t.sql": "CREATE TABLE t (x INT); INSERT INTO t VALUES (1);"})

func TestIdleReaders(t *testing.T) {
	for _, c := range []struct{ idle, want int }{{0, 6}, {1, 1}, {3, 3}, {-1, 0}} {
		d := open(t, Options{Readers: 6, IdleReaders: c.idle, Migrations: oneRow})
		burst(t, d, 6)
		if st := d.R.Stats(); st.OpenConnections != c.want || st.Idle != c.want {
			t.Errorf("IdleReaders %d: %d open, %d idle after the burst, want %d", c.idle, st.OpenConnections, st.Idle, c.want)
		}
		count(t, d)
		burst(t, d, 6)
		count(t, d)
	}
}

// Readers that close take their statements with them: statements in use
// when the pool trims them, and the ones a new connection prepares. Half
// the readers hold a connection until all of them do, so the pool opens
// eight however many goroutines run at once.
func TestIdleReadersUnderLoad(t *testing.T) {
	d := open(t, Options{Readers: 8, IdleReaders: 1, Statements: 4, Migrations: oneRow})
	ctx := context.Background()
	var held, wg sync.WaitGroup
	held.Add(8)
	all := make(chan struct{})
	for g := range 16 {
		wg.Go(func() {
			if g < 8 {
				c, err := d.R.Conn(ctx)
				if err != nil {
					t.Error(err)
					held.Done()
					return
				}
				var n int
				if err := c.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 1 {
					t.Errorf("%d rows, %v", n, err)
				}
				held.Done()
				<-all
				c.Close()
			}
			for range 50 {
				var n int
				if err := d.R.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != 1 {
					t.Errorf("%d rows, %v", n, err)
					return
				}
			}
		})
	}
	held.Wait()
	close(all)
	wg.Wait()
	// The pool opens connections for waiters on a goroutine of its own, which
	// may still be under way when the last reader returns its connection.
	deadline := time.Now().Add(5 * time.Second)
	for d.R.Stats().OpenConnections != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("after the load: %+v", d.R.Stats())
		}
		time.Sleep(time.Millisecond)
	}
	if st := d.R.Stats(); st.MaxIdleClosed < 7 {
		t.Errorf("after the load: %+v", st)
	}
	count(t, d)
}

// Options reports IdleReaders as the pool keeps them: no more than
// Readers.
func TestIdleReadersOptions(t *testing.T) {
	for _, c := range []struct{ readers, idle, want int }{{2, 0, 2}, {2, 1, 1}, {2, 10, 2}, {2, -1, -1}} {
		d := open(t, Options{Readers: c.readers, IdleReaders: c.idle, Migrations: oneRow})
		if got := d.Options().IdleReaders; got != c.want {
			t.Errorf("Readers %d, IdleReaders %d: Options reports %d, want %d", c.readers, c.idle, got, c.want)
		}
		burst(t, d, c.readers)
		if st := d.R.Stats(); st.Idle != max(0, c.want) {
			t.Errorf("Readers %d, IdleReaders %d: %d idle after a burst, want %d", c.readers, c.idle, st.Idle, max(0, c.want))
		}
	}
}

// ReaderIdleTime closes what sits idle, and leaves the burst's connections
// open until then.
func TestReaderIdleTime(t *testing.T) {
	d := open(t, Options{Readers: 4, ReaderIdleTime: 50 * time.Millisecond, Migrations: oneRow})
	burst(t, d, 4)
	if got := d.R.Stats().OpenConnections; got != 4 {
		t.Fatalf("%d open right after the burst, want 4", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for d.R.Stats().OpenConnections != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("still open after 5 s: %+v", d.R.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	count(t, d)
}
