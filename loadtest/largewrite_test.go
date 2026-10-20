package loadtest

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zweiundeins/lith/dbmetrics"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// largeRows is the size of the table a large write changes: 45,000
// comments of about 2 KB of text and 2 KB of rendered HTML each.
const largeRows = 45000

const largeSchema = `
CREATE TABLE loadtest_comments (
	id      INTEGER PRIMARY KEY,
	card_id INTEGER NOT NULL,
	seen    INTEGER NOT NULL,
	body    TEXT NOT NULL,
	html    TEXT
) STRICT;
CREATE VIRTUAL TABLE loadtest_comments_fts USING fts5(body, content='loadtest_comments', content_rowid='id');
`

// BenchmarkLargeWrite runs one large write while the board of
// BenchmarkSharedBoard is under load: 200 zstd streams on one board of
// 200 cards, and 200 producers that change a card 800 times a second
// between them with Do. The write changes all 45,000 rows of a table of
// comments, about 190 MB: it clears every comment's rendered HTML
// (update), deletes the rows a full pass did not see (delete), or
// rebuilds the comments' FTS5 index (fts). It runs as one command, as
// commands of 1,000 or 5,000 rows by rowid range sent one after the
// other with Do, or in Writer.Exclusive. The database is checkpointed by
// the writer, between batches or, in the cases named aside, beside them
// (writer.Options.CheckpointAside). An iteration is one run: 2 s of load,
// 2 s measured before the write, the write, and 2 s after it.
//
//	touch /tmp/lith-bench.lock
//	flock /tmp/lith-bench.lock taskset -c 0-11 go test ./loadtest -run '^$' -bench LargeWrite -benchtime 1x -count 3 -timeout 1h
func BenchmarkLargeWrite(b *testing.B) {
	type bench struct {
		sync, op string
		chunk    int // rows per command; 0: one command, -1: Exclusive
		aside    bool
	}
	var cases []bench
	for _, op := range []string{"update", "delete"} {
		for _, chunk := range []int{0, 1000, 5000, -1} {
			cases = append(cases, bench{"NORMAL", op, chunk, false})
		}
	}
	cases = append(cases, bench{"FULL", "update", 1000, false}, bench{"FULL", "update", 5000, false}, bench{"NORMAL", "fts", 0, false})
	for _, chunk := range []int{0, 1000, 5000} {
		cases = append(cases, bench{"NORMAL", "update", chunk, true})
	}
	cases = append(cases, bench{"FULL", "update", 1000, true}, bench{"NORMAL", "fts", 0, true})
	for _, c := range cases {
		how := "one"
		switch {
		case c.chunk > 0:
			how = fmt.Sprintf("chunks=%d", c.chunk)
		case c.chunk < 0:
			how = "exclusive"
		}
		if c.aside {
			how += "/aside"
		}
		b.Run(c.sync+"/"+c.op+"/"+how, func(b *testing.B) {
			var r largeReport
			for b.Loop() {
				var err error
				if r, err = largeWrite(b.Context(), c.sync, c.op, c.chunk, c.aside); err != nil {
					b.Fatal(err)
				}
			}
			b.Log("\n" + r.String())
			for _, m := range r.metrics() {
				b.ReportMetric(m.Value, m.Unit)
			}
			b.ReportMetric(0, "ns/op")
		})
	}
}

// largeReport is what one run of a large write measured. The window is
// from the write's start to 2 s after its end.
type largeReport struct {
	op        time.Duration // the large write, from its start to its last commit
	commands  int           // commands of the write
	before    Percentiles   // Do of the producers' commands due in the 2 s before
	during    Percentiles   // ... due while the write ran
	after     Percentiles   // ... due in the 2 s after it
	ticks     Percentiles   // ticks in the window
	tickGap   time.Duration // the longest time between two ticks in the window
	renders   int64
	sent      int64
	dropped   int64
	walMax    int64 // the largest WAL file seen in the window
	walEnd    int64 // the WAL file at the window's end
	ckpts     int   // checkpoints of 1 ms and more in the window
	ckptMax   time.Duration
	dbSize    int64
	errors    int
	maxBatch  int
	walBefore int64
}

func (r largeReport) String() string {
	mb := func(n int64) float64 { return float64(n) / 1e6 }
	return fmt.Sprintf("write: %v in %d commands; database %.0f MB\n", r.op.Round(time.Millisecond), r.commands, mb(r.dbSize)) +
		fmt.Sprintf("Do before: %v\nDo during: %v\nDo after:  %v\n", r.before, r.during, r.after) +
		fmt.Sprintf("ticks: %d, %v, longest gap %v\n", r.ticks.N, r.ticks, r.tickGap.Round(time.Millisecond)) +
		fmt.Sprintf("renders %d, frames %d sent, %d dropped\n", r.renders, r.sent, r.dropped) +
		fmt.Sprintf("WAL: %.1f MB before, %.1f MB at most, %.1f MB at the end; %d checkpoints of 1 ms or more, the longest %v\n",
			mb(r.walBefore), mb(r.walMax), mb(r.walEnd), r.ckpts, r.ckptMax) +
		fmt.Sprintf("writer: at most %d commands a transaction; %d errors", r.maxBatch, r.errors)
}

func (r largeReport) metrics() []Metric {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return []Metric{
		{r.op.Seconds(), "write-s"},
		{ms(r.before.P99), "before-p99-ms"},
		{ms(r.during.P50), "during-p50-ms"},
		{ms(r.during.P99), "during-p99-ms"},
		{ms(r.during.Max), "during-max-ms"},
		{ms(r.after.Max), "after-max-ms"},
		{ms(r.ticks.P99), "tick-p99-ms"},
		{ms(r.ticks.Max), "tick-max-ms"},
		{ms(r.tickGap), "tick-gap-ms"},
		{float64(r.renders), "renders"},
		{float64(r.dropped), "dropped"},
		{float64(r.walMax) / 1e6, "wal-max-MB"},
		{float64(r.walEnd) / 1e6, "wal-end-MB"},
		{ms(r.ckptMax), "ckpt-max-ms"},
	}
}

// checkpoints collects the writer's checkpoints from dbmetrics' slow log.
type checkpoints struct {
	mu   sync.Mutex
	took []time.Time // when each ended
	ms   []int64
}

func (c *checkpoints) Enabled(context.Context, slog.Level) bool { return true }
func (c *checkpoints) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *checkpoints) WithGroup(string) slog.Handler            { return c }
func (c *checkpoints) Handle(_ context.Context, r slog.Record) error {
	var q string
	var ms int64
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "query":
			q = a.Value.String()
		case "duration_ms":
			ms = a.Value.Int64()
		}
		return true
	})
	if strings.HasPrefix(q, "PRAGMA wal_checkpoint") {
		c.mu.Lock()
		c.took = append(c.took, r.Time)
		c.ms = append(c.ms, ms)
		c.mu.Unlock()
	}
	return nil
}

var (
	largeLog      = &checkpoints{}
	largeRegister = sync.OnceValue(func() error {
		return dbmetrics.Register("sqlite-largewrite", "sqlite", dbmetrics.Options{
			SlowThreshold: time.Millisecond,
			Logger:        slog.New(largeLog),
		})
	})
)

func largeMigrations() (fs.FS, error) {
	m := fstest.MapFS{"002_comments.sql": {Data: []byte(largeSchema)}}
	b, err := fs.ReadFile(Board{}.Migrations(), "001_board.sql")
	m["001_board.sql"] = &fstest.MapFile{Data: b}
	return m, err
}

// comment is comment id as it is seeded: about 2 KB of words from a
// vocabulary of a few thousand, and its HTML.
func comment(id int64) (body, html string) {
	const syllables = "ka lo mi ne ru sa ti vo be da fe gi ho ju ly pa qu ri so te"
	syl := strings.Fields(syllables)
	var sb strings.Builder
	for x := mix(uint64(id)); sb.Len() < 2000; x = mix(x) {
		n := 1 + x%3
		for i := range n {
			sb.WriteString(syl[(x>>(8*(i+1)))%uint64(len(syl))])
		}
		sb.WriteByte(' ')
	}
	body = sb.String()
	return body, "<p class=\"comment\">" + body + "</p>"
}

func largeSeed(ctx context.Context, w *writer.Writer) error {
	for lo := int64(1); lo <= largeRows; lo += 5000 {
		err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			for id := lo; id < lo+5000 && id <= largeRows; id++ {
				body, html := comment(id)
				if _, err := tx.Exec(`INSERT INTO loadtest_comments (id, card_id, seen, body, html) VALUES (?, ?, 1, ?, ?)`, id, 1+id%200, body, html); err != nil {
					return err
				}
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	return nil
}

// largeSQL is the write's statement over all rows, and over the rows
// from ?1 to ?2.
func largeSQL(op string) (all, chunk string) {
	switch op {
	case "update":
		return `UPDATE loadtest_comments SET html = NULL WHERE html IS NOT NULL`,
			`UPDATE loadtest_comments SET html = NULL WHERE id BETWEEN ?1 AND ?2 AND html IS NOT NULL`
	case "delete": // a WHERE clause, as DeleteMissing has: without one SQLite truncates the table
		return `DELETE FROM loadtest_comments WHERE seen < 2`,
			`DELETE FROM loadtest_comments WHERE id BETWEEN ?1 AND ?2 AND seen < 2`
	default:
		return `INSERT INTO loadtest_comments_fts (loadtest_comments_fts) VALUES ('rebuild')`, ""
	}
}

func largeWrite(ctx context.Context, synchronous, op string, chunk int, aside bool) (rep largeReport, err error) {
	if err := largeRegister(); err != nil {
		return rep, err
	}
	dir, err := os.MkdirTemp("", "lith-largewrite-")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(dir)
	migrations, err := largeMigrations()
	if err != nil {
		return rep, err
	}
	path := filepath.Join(dir, "large.db")
	db, err := sqlite.Open(ctx, path, sqlite.Options{
		Synchronous: synchronous,
		Migrations:  migrations,
		Checkpoint:  sqlite.CheckpointWriter,
		Driver:      "sqlite-largewrite",
		Readers:     runtime.GOMAXPROCS(0) + 4,
	})
	if err != nil {
		return rep, err
	}
	defer db.Close()

	var tickMu sync.Mutex
	type tick struct {
		at time.Time
		d  time.Duration
	}
	var ticks []tick
	hub := live.New(live.Options{Read: db.R, MaxStreams: 200, OnTick: func(d time.Duration) {
		tickMu.Lock()
		ticks = append(ticks, tick{time.Now(), d})
		tickMu.Unlock()
	}})
	w, err := writer.New(db, writer.Options{OnCommit: hub.Changed, CheckpointAside: aside})
	if err != nil {
		return rep, err
	}
	go w.Run(context.Background())
	defer w.Close()
	hubCtx, stopHub := context.WithCancel(context.Background())
	hubDone := make(chan error, 1)
	go func() { hubDone <- hub.Run(hubCtx) }()
	defer func() { stopHub(); <-hubDone }()

	board := Board{Rows: 200}
	if err := w.Do(ctx, board.Seed()); err != nil {
		return rep, err
	}
	if err := largeSeed(ctx, w); err != nil {
		return rep, err
	}
	if op == "fts" { // the index the rebuild replaces
		all, _ := largeSQL(op)
		if err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error { _, err := tx.Exec(all); return err })); err != nil {
			return rep, err
		}
	}

	// The idle checkpoint truncates the seed's WAL before the load starts.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 200 streams on the board, each with its own client.
	streamCtx, stopStreams := context.WithCancel(context.Background())
	var served sync.WaitGroup
	defer func() { stopStreams(); served.Wait() }()
	clients := make([]*client, 200)
	for i := range clients {
		clients[i] = newClient(sse.Zstd)
		req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", sse.Zstd)
		served.Go(func() { hub.Serve(clients[i], req, board.Stream(i)) })
	}
	for _, c := range clients {
		for c.writes.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}

	// 200 producers, 800 commands a second, each on a goroutine of its own.
	type result struct {
		due    time.Time
		took   time.Duration
		failed bool
	}
	var resMu sync.Mutex
	var results []result
	var inflight sync.WaitGroup
	pctx, stopProducers := context.WithCancel(ctx)
	var producers sync.WaitGroup
	start := time.Now()
	const nprod, rate = 200, 800
	every := time.Second * nprod / rate
	for p := range nprod {
		producers.Go(func() {
			due := start.Add(every * time.Duration(p) / nprod)
			for i := int64(0); pctx.Err() == nil; i++ {
				if d := time.Until(due); d > 0 {
					select {
					case <-pctx.Done():
						return
					case <-time.After(d):
					}
				}
				cmd, at := board.Command(p, i), due
				inflight.Go(func() {
					err := w.Do(ctx, cmd)
					resMu.Lock()
					results = append(results, result{at, time.Since(at), err != nil})
					resMu.Unlock()
				})
				due = due.Add(every)
			}
		})
	}
	defer func() { stopProducers(); producers.Wait(); inflight.Wait() }()

	// The WAL's size, every 10 ms.
	type walSample struct {
		at   time.Time
		size int64
	}
	var walMu sync.Mutex
	var wal []walSample
	walCtx, stopWAL := context.WithCancel(context.Background())
	walDone := make(chan struct{})
	go func() {
		defer close(walDone)
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		for {
			var size int64
			if fi, err := os.Stat(path + "-wal"); err == nil {
				size = fi.Size()
			}
			walMu.Lock()
			wal = append(wal, walSample{time.Now(), size})
			walMu.Unlock()
			select {
			case <-walCtx.Done():
				return
			case <-t.C:
			}
		}
	}()
	defer func() { stopWAL(); <-walDone }()

	err = db.R.QueryRowContext(ctx, `SELECT page_count * page_size FROM pragma_page_count, pragma_page_size`).Scan(&rep.dbSize)
	if err != nil {
		return rep, err
	}
	time.Sleep(4 * time.Second) // 2 s of warmup, 2 s measured before the write
	s0 := hub.Stats()
	opStart := time.Now()
	all, part := largeSQL(op)
	switch {
	case chunk > 0:
		for lo := int64(1); lo <= largeRows; lo += int64(chunk) {
			hi := lo + int64(chunk) - 1
			err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
				_, err := tx.Exec(part, lo, hi)
				return err
			}))
			if err != nil {
				return rep, err
			}
			rep.commands++
		}
	case chunk < 0:
		err = w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, all); err != nil {
				return err
			}
			return tx.Commit()
		})
		rep.commands = 1
	default:
		err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			_, err := tx.Exec(all)
			return err
		}))
		rep.commands = 1
	}
	if err != nil {
		return rep, err
	}
	opEnd := time.Now()
	rep.op = opEnd.Sub(opStart)
	time.Sleep(2 * time.Second)
	windowEnd := time.Now()
	s1 := hub.Stats()
	rep.renders = s1.Renders - s0.Renders
	rep.sent = s1.FramesSent - s0.FramesSent
	rep.dropped = s1.FramesDropped - s0.FramesDropped
	rep.maxBatch = w.Stats().MaxBatch

	stopProducers()
	producers.Wait()
	inflight.Wait()

	var before, during, after []time.Duration
	resMu.Lock()
	for _, r := range results {
		if r.failed {
			rep.errors++
		}
		switch {
		case r.due.Before(opStart.Add(-2*time.Second)) || !r.due.Before(windowEnd):
		case r.due.Before(opStart):
			before = append(before, r.took)
		case r.due.Before(opEnd):
			during = append(during, r.took)
		default:
			after = append(after, r.took)
		}
	}
	resMu.Unlock()
	rep.before, rep.during, rep.after = percentiles(before), percentiles(during), percentiles(after)

	tickMu.Lock()
	var tds []time.Duration
	last := opStart
	for _, t := range ticks {
		if t.at.Before(opStart) || !t.at.Before(windowEnd) {
			continue
		}
		tds = append(tds, t.d)
		rep.tickGap = max(rep.tickGap, t.at.Sub(last))
		last = t.at
	}
	tickMu.Unlock()
	rep.ticks = percentiles(tds)

	walMu.Lock()
	for _, s := range wal {
		if s.at.Before(opStart) {
			rep.walBefore = s.size
			continue
		}
		if s.at.Before(windowEnd) {
			rep.walMax = max(rep.walMax, s.size)
			rep.walEnd = s.size
		}
	}
	walMu.Unlock()

	largeLog.mu.Lock()
	for i, at := range largeLog.took {
		if at.Before(opStart) || !at.Before(windowEnd) {
			continue
		}
		rep.ckpts++
		rep.ckptMax = max(rep.ckptMax, time.Duration(largeLog.ms[i])*time.Millisecond)
	}
	largeLog.took, largeLog.ms = nil, nil
	largeLog.mu.Unlock()

	return rep, nil
}
