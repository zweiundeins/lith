package loadtest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// open opens a database with the board's schema, seeded.
func open(t *testing.T, b Board) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "board.db"), sqlite.Options{Synchronous: "NORMAL", Migrations: b.Migrations()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	w, err := writer.New(db, writer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(t.Context())
	defer w.Close()
	if err := w.Do(t.Context(), b.Seed()); err != nil {
		t.Fatal(err)
	}
	return db
}

func render(t *testing.T, db *sqlite.DB, v live.View) []byte {
	t.Helper()
	var f any
	err := db.View(t.Context(), func(tx *sql.Tx) (err error) {
		f, err = v.Render(t.Context(), tx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return f.([]byte)
}

// A run reports on every part: the ticks, the streams by encoding, the
// heap they hold, and the commands, sent on schedule.
func TestRun(t *testing.T) {
	b := Board{Rows: 20}
	r, err := Run(t.Context(), Config{
		SQLite:         sqlite.Options{Synchronous: "NORMAL", Migrations: b.Migrations()},
		Seed:           b.Seed(),
		Streams:        6,
		Stream:         b.Stream,
		AcceptEncoding: []string{sse.Zstd, sse.Brotli, ""},
		Producers:      3,
		Rate:           300,
		Command:        b.Command,
		Warmup:         100 * time.Millisecond,
		Duration:       500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(r)
	if r.Streams != 6 || r.Producers != 3 || r.Rate != 300 || r.Duration < 500*time.Millisecond {
		t.Fatalf("the report does not describe the run: %+v", r)
	}
	if r.Ticks == 0 || r.Renders < r.Ticks || r.FramesSent == 0 {
		t.Fatalf("ticks %d, renders %d, frames %d", r.Ticks, r.Renders, r.FramesSent)
	}
	if n := int64(r.Tick.N); n < r.Ticks-1 || n > r.Ticks+1 || r.Tick.Max < r.Tick.P50 {
		t.Fatalf("tick durations %+v for %d ticks", r.Tick, r.Ticks)
	}
	for _, c := range []string{sse.Zstd, sse.Brotli, "identity"} {
		if w := r.Wire[c]; w.Streams != 2 || w.Writes == 0 || w.Bytes == 0 {
			t.Fatalf("%s: %+v", c, w)
		}
	}
	if len(r.Wire) != 3 {
		t.Fatalf("wire %v", r.Wire)
	}
	if plain := r.Wire["identity"].PerWrite(); r.Wire[sse.Zstd].PerWrite() >= plain || r.Wire[sse.Brotli].PerWrite() >= plain {
		t.Fatalf("compressed writes are no smaller than plain ones: %v", r.Wire)
	}
	if r.HeapPerStream <= 0 {
		t.Fatalf("heap per stream %d", r.HeapPerStream)
	}
	if r.Commands == 0 || r.Batches == 0 || r.Errors != 0 {
		t.Fatalf("writer: %d commands in %d transactions, %d errors", r.Commands, r.Batches, r.Errors)
	}
	if due := r.Rate * r.Duration.Seconds(); math.Abs(float64(r.Sent)-due) > due/10+3 {
		t.Fatalf("%d commands were due in %v at %v a second", r.Sent, r.Duration, r.Rate)
	}
	if int64(r.Latency.N) != r.Sent {
		t.Fatalf("%d latencies for %d commands", r.Latency.N, r.Sent)
	}
	if !strings.Contains(r.String(), "wire, br: 2 streams") {
		t.Fatalf("String:\n%s", r)
	}
}

// The producers keep to their schedule when the writer falls behind:
// commands wait for it, and the latency shows how long.
func TestNoBackPressure(t *testing.T) {
	slow := writer.Func(func(*writer.Tx) error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})
	r, err := Run(t.Context(), Config{
		SQLite:    sqlite.Options{Synchronous: "NORMAL"},
		Producers: 2,
		Rate:      200,
		Command:   func(int, int64) writer.Command { return slow },
		Duration:  300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(r)
	if r.Sent < 50 {
		t.Fatalf("%d commands sent in %v at 200 a second", r.Sent, r.Duration)
	}
	if r.Commands > r.Sent*3/4 {
		t.Fatalf("a writer that takes 10ms a command ran %d of %d", r.Commands, r.Sent)
	}
	if r.Latency.Max < 100*time.Millisecond {
		t.Fatalf("the backlog does not show in the latency: %v", r.Latency)
	}
}

func TestConfigErrors(t *testing.T) {
	ok := Config{Duration: time.Millisecond}
	for name, mod := range map[string]func(*Config){
		"no duration":          func(c *Config) { c.Duration = 0 },
		"negative warmup":      func(c *Config) { c.Warmup = -1 },
		"streams and no spec":  func(c *Config) { c.Streams = 1 },
		"rate and no command":  func(c *Config) { c.Rate = 1 },
		"producers, no rate":   func(c *Config) { c.Producers = 1; c.Command = func(int, int64) writer.Command { return nil } },
		"negative rate":        func(c *Config) { c.Rate = -1 },
		"negative streams":     func(c *Config) { c.Streams = -1 },
		"no synchronous value": func(c *Config) { c.SQLite.Synchronous = "OFF" },
	} {
		c := ok
		mod(&c)
		if _, err := Run(t.Context(), c); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Run(t.Context(), ok); err != nil {
		t.Fatalf("a run of nothing: %v", err)
	}
}

// A stream that ends before the run does makes the run fail: streams with
// the same ID replace each other.
func TestStreamEndsEarly(t *testing.T) {
	b := Board{Rows: 1}
	_, err := Run(t.Context(), Config{
		SQLite:   sqlite.Options{Synchronous: "NORMAL", Migrations: b.Migrations()},
		Seed:     b.Seed(),
		Streams:  3,
		Stream:   func(int) live.Spec { return live.Spec{ID: "tab", Views: []live.View{b.View(0)}} },
		Duration: 5 * time.Second,
	})
	if !errors.Is(err, errEnded) || !strings.Contains(err.Error(), "ended before the run") {
		t.Fatalf("err = %v", err)
	}
}

// A caller's database is used as it is and stays open, and its write
// connection is free again after the run.
func TestCallerDatabase(t *testing.T) {
	b := Board{Rows: 5}
	db := open(t, b)
	r, err := Run(t.Context(), Config{
		DB:       db,
		Streams:  2,
		Stream:   b.Stream,
		Rate:     100,
		Command:  b.Command,
		Duration: 200 * time.Millisecond,
		Hub:      live.Options{Workers: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.R.QueryRow(`SELECT sum(n) FROM loadtest_cards`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < r.Commands || r.Commands == 0 {
		t.Fatalf("the cards count %d changes, the writer ran %d commands", n, r.Commands)
	}
	w, err := writer.New(db, writer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
}

// The client counts what a stream sends it: one write per flush that
// carried bytes. Its deadlines fail writes the way a connection's do.
func TestClient(t *testing.T) {
	c := newClient(sse.Zstd)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, br, zstd")
	s, err := sse.Open(c, req, sse.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Encoding() != sse.Zstd || c.writes.Load() != 0 {
		t.Fatalf("encoding %q, %d writes after the headers", s.Encoding(), c.writes.Load())
	}
	frame := sse.PatchElements(bytes.Repeat([]byte(`<p id="x">x</p>`), 1000))
	for range 3 {
		if err := s.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	_, wire := s.Sent()
	if c.writes.Load() != 3 || c.bytes.Load() != wire || wire >= int64(len(frame)) {
		t.Fatalf("%d writes, %d bytes; the stream sent %d", c.writes.Load(), c.bytes.Load(), wire)
	}
	s.Close()

	c.SetWriteDeadline(time.Now().Add(-time.Second))
	if _, err := c.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a write after the deadline: %v", err)
	}
	if err := c.FlushError(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a flush after the deadline: %v", err)
	}
	c.SetWriteDeadline(time.Time{})
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("a write without a deadline: %v", err)
	}
}

// The board is kanban's: 200 cards make about 140 KB of HTML, and a
// command changes its board's frame.
func TestBoard(t *testing.T) {
	b := Board{Boards: 2}
	db := open(t, b)
	v := b.View(1)
	before := render(t, db, v)
	html := 0
	for l := range bytes.Lines(before) {
		if e, ok := bytes.CutPrefix(l, []byte("data: elements ")); ok {
			html += len(e) - 1
		}
	}
	if html < 130_000 || html > 150_000 {
		t.Fatalf("200 cards make %d bytes of HTML", html)
	}
	if got := b.Stream(3).Views[0].Key; got != "board:1" {
		t.Fatalf("stream 3 watches %s", got)
	}

	var cmd bump
	for i := int64(0); cmd.topic != "board:1"; i++ {
		cmd = b.Command(0, i).(bump)
	}
	if cmd.id <= 200 || cmd.id > 400 {
		t.Fatalf("board 1 has no card %d", cmd.id)
	}
	if b.Command(5, 7) != b.Command(5, 7) {
		t.Fatal("the same producer and number made different commands")
	}
	w, err := writer.New(db, writer.Options{OnCommit: func(topics []string) {
		if len(topics) != 1 || topics[0] != "board:1" {
			t.Errorf("topics %v", topics)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(t.Context())
	if err := w.Do(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	w.Close()
	after := render(t, db, v)
	if bytes.Equal(before, after) || len(after) != len(before) {
		t.Fatalf("a command changed the frame from %d to %d bytes", len(before), len(after))
	}
	if !bytes.Contains(after, []byte(`title="Changed 1 times">1<`)) {
		t.Fatal("the card's counter did not change")
	}
}

func TestPercentiles(t *testing.T) {
	var d []time.Duration
	for i := 100; i > 0; i-- {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	want := Percentiles{N: 100, P50: 50 * time.Millisecond, P95: 95 * time.Millisecond, P99: 99 * time.Millisecond, Max: 100 * time.Millisecond}
	if got := percentiles(d); got != want {
		t.Fatalf("got %+v", got)
	}
	if got := percentiles(nil); got != (Percentiles{}) {
		t.Fatalf("no samples: %+v", got)
	}
}

func TestRunEndsWithItsContext(t *testing.T) {
	b := Board{Rows: 1}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, Config{
		SQLite:   sqlite.Options{Synchronous: "NORMAL", Migrations: b.Migrations()},
		Seed:     b.Seed(),
		Streams:  2,
		Stream:   b.Stream,
		Rate:     50,
		Command:  b.Command,
		Duration: time.Minute,
	})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}
