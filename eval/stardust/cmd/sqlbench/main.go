// Command sqlbench runs the kanban workload through lith on SQLite: writes
// through lith's writer, the board read in one read transaction, and the
// time from a commit to the board's frame on a stream of lith's live hub.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing/fstest"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"stardusteval/internal/kanban"
)

var (
	dir       = flag.String("dir", "/tmp/sdeval-db", "directory for database files")
	bench     = flag.String("bench", "write", "write, read or sub")
	syncMode  = flag.String("sync", "FULL", "synchronous: FULL or NORMAL")
	producers = flag.Int("producers", 1, "producers for the write bench")
	maxBatch  = flag.Int("maxbatch", 512, "writer MaxBatch (1: every command alone)")
	warmup    = flag.Duration("warmup", time.Second, "time before measuring")
	duration  = flag.Duration("duration", 5*time.Second, "measured time")
	iters     = flag.Int("iters", 500, "iterations for read and sub")
	gap       = flag.Duration("gap", 60*time.Millisecond, "pause between commits in sub")
)

const schema = `
CREATE TABLE cards (
	id      INTEGER PRIMARY KEY,
	board   INTEGER NOT NULL,
	title   TEXT NOT NULL,
	lane    INTEGER NOT NULL,
	pos     INTEGER NOT NULL,
	version INTEGER NOT NULL
) STRICT;
CREATE INDEX cards_board ON cards (board, lane, pos);
`

const boardSQL = `SELECT id, title, lane, pos, version FROM cards WHERE board = 1 ORDER BY lane, pos`

type move struct{ kanban.Move }

func (c move) Apply(tx *writer.Tx) error {
	var lane, version int64
	if err := tx.QueryRow(`SELECT lane, version FROM cards WHERE id = ?`, c.Card+1).Scan(&lane, &version); err != nil {
		return err
	}
	if version != c.Seen {
		return kanban.ErrMoved
	}
	if _, err := tx.Exec(`UPDATE cards SET lane = ?, version = ? WHERE id = ?`, c.Lane, version+1, c.Card+1); err != nil {
		return err
	}
	tx.Touch("board:1")
	return nil
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sqlbench:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(*dir, fmt.Sprintf("sq-%s-%s-%d-%d.db", *bench, *syncMode, *producers, os.Getpid()))
	defer func() {
		for _, s := range []string{"", "-wal", "-shm"} {
			os.Remove(path + s)
		}
	}()
	db, err := sqlite.Open(ctx, path, sqlite.Options{
		Synchronous: *syncMode,
		Migrations:  fstest.MapFS{"001_cards.sql": {Data: []byte(schema)}},
		Readers:     runtime.GOMAXPROCS(0) + 4,
		Checkpoint:  sqlite.CheckpointWriter,
	})
	if err != nil {
		return err
	}
	defer db.Close()
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	var hub *live.Hub
	onCommit := func([]string) {}
	if *bench == "sub" {
		hub = live.New(live.Options{Read: db.R, Log: quiet})
		onCommit = hub.Changed
	}
	w, err := writer.New(db, writer.Options{MaxBatch: *maxBatch, OnCommit: onCommit, Log: quiet})
	if err != nil {
		return err
	}
	go w.Run(ctx)
	defer w.Close()
	err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		for i := range kanban.Cards {
			c := kanban.Seed(i)
			if _, err := tx.Exec(`INSERT INTO cards (id, board, title, lane, pos, version) VALUES (?, 1, ?, ?, ?, ?)`, i+1, c.Title, c.Lane, c.Pos, c.Version); err != nil {
				return err
			}
		}
		return nil
	}))
	if err != nil {
		return err
	}
	switch *bench {
	case "write":
		return write(ctx, w)
	case "read":
		return read(ctx, db)
	case "sub":
		return sub(ctx, w, hub)
	case "grow":
		return grow(ctx, w, path)
	}
	return fmt.Errorf("unknown bench %q", *bench)
}

func mode() string {
	if *maxBatch == 1 {
		return *syncMode + ", alone"
	}
	return *syncMode + ", batched"
}

func write(ctx context.Context, w *writer.Writer) error {
	start := time.Now().Add(*warmup)
	win := kanban.Window{Start: start, End: start.Add(*duration)}
	var s0 writer.Stats
	t := time.AfterFunc(time.Until(win.Start), func() { s0 = w.Stats() })
	defer t.Stop()
	var mu sync.Mutex
	var lat []time.Duration
	var cmds, refused int64
	var failed atomic.Value
	var wg sync.WaitGroup
	for p := range *producers {
		wg.Go(func() {
			mover := kanban.NewMover(p, *producers)
			var mine []time.Duration
			var n, r int64
			for {
				begin := time.Now()
				if !begin.Before(win.End) {
					break
				}
				mv := mover.Next()
				err := w.Do(ctx, move{mv})
				if err != nil && !errors.Is(err, kanban.ErrMoved) {
					failed.Store(err)
					return
				}
				mover.Done(mv, err)
				if win.In(begin) {
					mine = append(mine, time.Since(begin))
					n++
					if err != nil {
						r++
					}
				}
			}
			mu.Lock()
			lat = append(lat, mine...)
			cmds += n
			refused += r
			mu.Unlock()
		})
	}
	wg.Wait()
	s1 := w.Stats()
	if err, _ := failed.Load().(error); err != nil {
		return err
	}
	secs := win.End.Sub(win.Start).Seconds()
	batches := s1.Batches - s0.Batches
	p50, p99 := kanban.Percentiles(lat)
	r := kanban.Result{Store: "sqlite", Mode: mode(), Bench: "write", Producers: *producers, Seconds: secs,
		Commands: cmds, Refused: refused, Transactions: batches, CmdsPerSec: float64(cmds) / secs, P50ms: p50, P99ms: p99,
		Extra: map[string]float64{"commit_ms_per_batch": float64(s1.Busy-s0.Busy) / float64(max(batches, 1)) / 1e6}}
	if batches > 0 {
		r.CmdsPerTx = float64(s1.Commands-s0.Commands) / float64(batches)
	}
	r.Print()
	return nil
}

func readBoard(ctx context.Context, tx *sql.Tx) ([]kanban.Card, error) {
	rows, err := tx.QueryContext(ctx, boardSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cards := make([]kanban.Card, 0, kanban.Cards)
	for rows.Next() {
		var c kanban.Card
		if err := rows.Scan(&c.ID, &c.Title, &c.Lane, &c.Pos, &c.Version); err != nil {
			return nil, err
		}
		cards = append(cards, c)
	}
	return cards, rows.Err()
}

func read(ctx context.Context, db *sqlite.DB) error {
	once := func() error {
		return db.View(ctx, func(tx *sql.Tx) error {
			cards, err := readBoard(ctx, tx)
			if err == nil && len(cards) != kanban.Cards {
				err = fmt.Errorf("board has %d cards", len(cards))
			}
			return err
		})
	}
	for range 50 {
		if err := once(); err != nil {
			return err
		}
	}
	lat := make([]time.Duration, 0, *iters)
	for range *iters {
		start := time.Now()
		if err := once(); err != nil {
			return err
		}
		lat = append(lat, time.Since(start))
	}
	p50, p99 := kanban.Percentiles(lat)
	kanban.Result{Store: "sqlite", Mode: "board, db.View", Bench: "read", P50ms: p50, P99ms: p99}.Print()
	return nil
}

// client is the far end of a stream without a network, as in lith's
// loadtest; it notes when each flush that carried bytes happened.
type client struct {
	header  http.Header
	pending int
	flushes chan time.Time
}

func (c *client) Header() http.Header         { return c.header }
func (c *client) WriteHeader(int)             {}
func (c *client) Write(p []byte) (int, error) { c.pending += len(p); return len(p), nil }
func (c *client) SetWriteDeadline(time.Time) error {
	return nil
}
func (c *client) FlushError() error {
	if c.pending > 0 {
		c.pending = 0
		select {
		case c.flushes <- time.Now():
		default:
		}
	}
	return nil
}

type rendered struct {
	at      time.Time
	version int64
}

func sub(ctx context.Context, w *writer.Writer, hub *live.Hub) error {
	renders := make(chan rendered, 1024)
	view := live.View{Key: "board:1", Topics: []string{"board:1"}, Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		cards, err := readBoard(ctx, tx)
		if err != nil {
			return nil, err
		}
		var top int64
		var b bytes.Buffer
		b.WriteString(`<div id="board">`)
		for _, c := range cards {
			top = max(top, c.Version)
			fmt.Fprintf(&b, `<article id="card-%d" data-lane="%d" data-version="%d"><a href="/cards/%d">%s</a></article>`, c.ID, c.Lane, c.Version, c.ID, html.EscapeString(c.Title))
		}
		b.WriteString(`</div>`)
		frame := sse.PatchElements(b.Bytes())
		select {
		case renders <- rendered{time.Now(), top}:
		default:
		}
		return frame, nil
	}}
	hubCtx, stop := context.WithCancel(ctx)
	defer stop()
	go hub.Run(hubCtx)
	c := &client{header: http.Header{}, flushes: make(chan time.Time, 1024)}
	req := httptest.NewRequestWithContext(hubCtx, "GET", "/stream", nil)
	req.Header.Set("Accept-Encoding", "identity")
	go hub.Serve(c, req, live.Spec{ID: "tab", Views: []live.View{view}})
	time.Sleep(500 * time.Millisecond)
	drain := func() {
		for {
			select {
			case <-renders:
			case <-c.flushes:
			default:
				return
			}
		}
	}
	var commitLat, renderLat, frameLat []time.Duration
	for i := range *iters {
		time.Sleep(*gap)
		drain()
		v := int64(1_000_000 + i)
		start := time.Now()
		err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			if _, err := tx.Exec(`UPDATE cards SET lane = ?, version = ? WHERE id = ?`, i%kanban.Lanes, v, i%kanban.Cards+1); err != nil {
				return err
			}
			tx.Touch("board:1")
			return nil
		}))
		returned := time.Now()
		if err != nil {
			return err
		}
		timeout := time.After(2 * time.Second)
		var renderedAt time.Time
	waitRender:
		for {
			select {
			case r := <-renders:
				if r.version >= v {
					renderedAt = r.at
					break waitRender
				}
			case <-timeout:
				return fmt.Errorf("no render for version %d", v)
			}
		}
		var flushedAt time.Time
	waitFlush:
		for {
			select {
			case f := <-c.flushes:
				if !f.Before(renderedAt) {
					flushedAt = f
					break waitFlush
				}
			case <-timeout:
				return fmt.Errorf("no frame for version %d", v)
			}
		}
		commitLat = append(commitLat, returned.Sub(start))
		renderLat = append(renderLat, max(renderedAt.Sub(returned), 0))
		frameLat = append(frameLat, max(flushedAt.Sub(returned), 0))
	}
	c50, c99 := kanban.Percentiles(commitLat)
	r50, r99 := kanban.Percentiles(renderLat)
	f50, f99 := kanban.Percentiles(frameLat)
	kanban.Result{Store: "sqlite", Mode: *syncMode + ", lith hub", Bench: "sub", P50ms: f50, P99ms: f99, Extra: map[string]float64{
		"commit p50ms": c50, "commit p99ms": c99, "render done p50ms": r50, "render done p99ms": r99,
	}}.Print()
	return nil
}

// grow measures the file after commits that each move ten cards; SQLite
// updates rows in place, so this is the size without history.
func grow(ctx context.Context, w *writer.Writer, path string) error {
	size := func() int64 {
		var n int64
		for _, s := range []string{"", "-wal"} {
			if st, err := os.Stat(path + s); err == nil {
				n += st.Size()
			}
		}
		return n
	}
	extra := map[string]float64{"bytes after seed": float64(size())}
	for i := range *iters {
		err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			for k := range 10 {
				card := (i*10+k)%kanban.Cards + 1
				if _, err := tx.Exec(`UPDATE cards SET lane = ?, version = version + 1 WHERE id = ?`, (i+k)%kanban.Lanes, card); err != nil {
					return err
				}
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	time.Sleep(1500 * time.Millisecond)
	extra["bytes after moves (db and wal)"] = float64(size())
	kanban.Result{Store: "sqlite", Mode: "file size", Bench: "grow", Transactions: int64(*iters), Extra: extra}.Print()
	return nil
}
