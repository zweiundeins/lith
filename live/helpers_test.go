package live

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{header: http.Header{}, cancel: cancel, done: make(chan error, 1), blocked: make(chan struct{})}
	r := httptest.NewRequestWithContext(ctx, "GET", "/live", nil)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	go func() { c.done <- h.Serve(c, r, spec) }()
	return c
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
