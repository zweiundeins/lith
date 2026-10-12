package live

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/zweiundeins/lith/sse"
)

// testDB opens a database with a counter table: a write connection and a
// read pool of pool connections.
func testDB(t *testing.T, pool int) (write, read *sql.DB) {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db") + "?_pragma=journal_mode(wal)&_pragma=synchronous(off)&_pragma=busy_timeout(5000)"
	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	write.SetMaxOpenConns(1)
	if _, err := write.Exec(`create table counter (name text primary key, n integer not null); insert into counter values ('a', 0)`); err != nil {
		t.Fatal(err)
	}
	read, err = sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	read.SetMaxOpenConns(pool)
	t.Cleanup(func() {
		read.Close()
		write.Close()
	})
	return write, read
}

// fakeDB is a read pool for views that do not query. A transaction
// begins unless the pool is down or its connection is the broken one,
// and a connection opens unless the pool is down or has opened maxOpen.
type fakeDB struct {
	*sql.DB
	broken  int64 // which connection, counted in the order they open; 0: none
	maxOpen int64 // 0: no limit
	down    atomic.Bool
	opened  atomic.Int64
}

func newFakeDB(t *testing.T, pool int, broken, maxOpen int64) *fakeDB {
	d := &fakeDB{broken: broken, maxOpen: maxOpen}
	d.DB = sql.OpenDB(fakeConnector{d})
	d.SetMaxOpenConns(pool)
	t.Cleanup(func() { d.Close() })
	return d
}

type fakeConnector struct{ d *fakeDB }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	if c.d.down.Load() || c.d.maxOpen > 0 && c.d.opened.Load() >= c.d.maxOpen {
		return nil, errors.New("fake: cannot connect")
	}
	n := c.d.opened.Add(1)
	return &fakeConn{d: c.d, broken: n == c.d.broken}, nil
}

func (c fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake: open with the connector")
}

type fakeConn struct {
	d      *fakeDB
	broken bool
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake: no statements")
}

func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	if c.broken || c.d.down.Load() {
		return nil, errors.New("fake: cannot begin")
	}
	return fakeTx{}, nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

func increment(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`update counter set n = n + 1 where name = 'a'`); err != nil {
		t.Fatal(err)
	}
}

func count(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `select n from counter where name = 'a'`).Scan(&n)
	return n, err
}

// start runs a hub until the test ends.
func start(t *testing.T, o Options) *Hub {
	t.Helper()
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	h := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return h
}

// text is a view whose frame patches <p id=key> with what f returns.
func text(key string, topics []string, f func(ctx context.Context, tx *sql.Tx) (string, error)) View {
	return View{Key: key, Topics: topics, Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		s, err := f(ctx, tx)
		if err != nil {
			return nil, err
		}
		return sse.PatchElements([]byte(`<p id="` + key + `">` + s + `</p>`)), nil
	}}
}

// client is a browser tab: a ResponseWriter with write deadlines that
// can stop taking bytes, like a client that stopped reading.
type client struct {
	header http.Header
	cancel context.CancelFunc
	done   chan error // what Serve returned

	mu       sync.Mutex
	code     int
	body     bytes.Buffer
	deadline time.Time
	stalled  bool
	blocked  chan struct{} // closed when a write first blocks
}

func (c *client) Header() http.Header { return c.header }
func (c *client) FlushError() error   { return nil }

func (c *client) WriteHeader(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.code = code
}

func (c *client) status() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code
}

func (c *client) Write(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if !c.stalled {
			defer c.mu.Unlock()
			return c.body.Write(p)
		}
		select {
		case <-c.blocked:
		default:
			close(c.blocked)
		}
		d := c.deadline
		c.mu.Unlock()
		if !d.IsZero() && !time.Now().Before(d) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *client) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

func (c *client) stall() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stalled = true
}

func (c *client) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.body.Bytes())
}

// serve opens a stream for spec, uncompressed unless acceptEncoding says
// otherwise.
func serve(h *Hub, spec Spec, acceptEncoding string) *client {
	return serveThrough(h, spec, acceptEncoding, nil)
}

// serveThrough is serve with Serve writing to what wrap makes of the
// client.
func serveThrough(h *Hub, spec Spec, acceptEncoding string, wrap func(*client) http.ResponseWriter) *client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{header: http.Header{}, cancel: cancel, done: make(chan error, 1), blocked: make(chan struct{})}
	r := httptest.NewRequestWithContext(ctx, "GET", "/live", nil)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	var w http.ResponseWriter = c
	if wrap != nil {
		w = wrap(c)
	}
	go func() { c.done <- h.Serve(w, r, spec) }()
	return c
}

// returned waits for Serve's result.
func (c *client) returned(t *testing.T) error {
	t.Helper()
	select {
	case err := <-c.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
		return nil
	}
}

// leave ends the stream as a client does that goes away, and waits for
// Serve to return.
func (c *client) leave(t *testing.T) {
	t.Helper()
	c.cancel()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the client left")
	}
}

type event struct {
	name string // "comment" for a comment
	data string // the data lines without their keys, joined by newlines
}

// events parses the uncompressed stream.
func (c *client) events() []event {
	return parse(c.bytes())
}

func parse(b []byte) []event {
	var out []event
	for _, block := range strings.Split(string(b), "\n\n") {
		if block == "" {
			continue
		}
		if strings.HasPrefix(block, ":") {
			out = append(out, event{name: "comment"})
			continue
		}
		var e event
		var data []string
		for l := range strings.SplitSeq(block, "\n") {
			if v, ok := strings.CutPrefix(l, "event: "); ok {
				e.name = v
			} else if v, ok := strings.CutPrefix(l, "data: "); ok {
				_, v, _ = strings.Cut(v, " ")
				data = append(data, v)
			}
		}
		e.data = strings.Join(data, "\n")
		out = append(out, e)
	}
	return out
}

// patches lists the HTML the stream received, in order.
func (c *client) patches() []string {
	var out []string
	for _, e := range c.events() {
		if e.name == "datastar-patch-elements" {
			out = append(out, e.data)
		}
	}
	return out
}

func (c *client) has(html string) bool {
	for _, p := range c.patches() {
		if p == html {
			return true
		}
	}
	return false
}

// unzstd decodes what a zstd stream sent so far.
func unzstd(t *testing.T, b []byte) []byte {
	t.Helper()
	d, err := zstd.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	out, err := io.ReadAll(d)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// logs collects what the hub logs.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logs) logger() *slog.Logger { return slog.New(slog.NewTextHandler(l, nil)) }
