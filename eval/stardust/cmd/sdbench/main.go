// Command sdbench runs the kanban workload on Stardust: writes (each
// command alone, or batched by one writer goroutine), the board read, and
// the latency and cost of live subscriptions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"stardust"
	"stardusteval/internal/kanban"
	"stardusteval/internal/sdx"
)

var (
	token     = flag.String("token", "license.token", "licence token file")
	dir       = flag.String("dir", "/tmp/sdeval-db", "directory for database files")
	bench     = flag.String("bench", "write", "write, read, sub or subscale")
	mode      = flag.String("mode", "alone", "write mode: alone (one mutation per command, called by each producer) or batched (one writer goroutine, one merge patch per batch)")
	producers = flag.Int("producers", 1, "producers for the write bench")
	warmup    = flag.Duration("warmup", time.Second, "time before measuring")
	duration  = flag.Duration("duration", 5*time.Second, "measured time")
	maxBatch  = flag.Int("maxbatch", 512, "most commands per batch in batched mode")
	plugins   = flag.String("plugins", "default", "default (all included) or none")
	iters     = flag.Int("iters", 500, "iterations for read and sub")
	subs      = flag.Int("subs", 200, "subscriptions for subscale")
	gap       = flag.Duration("gap", 60*time.Millisecond, "pause between commits in sub and subscale")
	batchWin  = flag.Duration("commitbatch", 0, "Options.CommitBatch (0: library default)")
	noSub     = flag.Bool("nosub", false, "sub bench: commit without a subscription, to time the commits alone")
	batchRead = flag.String("batchread", "card", "batched mode: card (each command reads its card) or board (one board read per batch)")
)

const casText = `query {find  [?card ?to ?next]
       where [[?card version ?seen]
              [?card lane ?old]
              [[+ ?seen 1] ?next]
              [[+ ?lane 0] ?to]]}
patch {?card {lane    ?to
              version ?next}}`

const pointText = `find  [?lane ?version]
where [[?card lane ?lane]
       [?card version ?version]]`

const boardText = `find    [?e ?title ?lane ?pos ?version]
where   [[?e board 1]
         [?e title ?title]
         [?e lane ?lane]
         [?e pos ?pos]
         [?e version ?version]]
orderBy [?lane ?pos]`

const boardAsOfText = boardText + "\nbind    {with {db {asOf ?at}}}"

type row struct {
	E       stardust.Link `json:"e"`
	Title   string        `json:"title"`
	Lane    int64         `json:"lane"`
	Pos     int64         `json:"pos"`
	Version int64         `json:"version"`
}

type state struct {
	Lane    int64 `json:"lane"`
	Version int64 `json:"version"`
}

func main() {
	flag.Parse()
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_STACK, &lim); err == nil && lim.Cur == ^uint64(0) {
		fmt.Fprintln(os.Stderr, "sdbench: the stack limit is unlimited; libstardust then fails to start its commit thread and every write blocks. Run under `ulimit -s 8192`.")
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sdbench:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := sdx.Activate(*token); err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(*dir, fmt.Sprintf("sd-%s-%s-%d-%d.db", *bench, *mode, *producers, os.Getpid()))
	opts := stardust.Options{CommitBatch: *batchWin}
	if *plugins == "none" {
		opts.Plugins = map[string]any{}
	}
	db, err := stardust.Open(path, opts)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	defer db.Close()
	ids, err := seed(db)
	if err != nil {
		return err
	}
	switch *bench {
	case "write":
		if *mode == "batched" {
			return writeBatched(db, ids)
		}
		return writeAlone(db, ids)
	case "read":
		return read(db)
	case "sub":
		return sub(db, ids)
	case "subscale":
		return subscale(db, ids)
	case "grow":
		return grow(db, ids, path)
	}
	return fmt.Errorf("unknown bench %q", *bench)
}

func seed(db *stardust.Database) ([]int64, error) {
	patch := map[string]any{}
	for i := range kanban.Cards {
		c := kanban.Seed(i)
		patch["#_c"+strconv.Itoa(i)] = map[string]any{"board": 1, "title": c.Title, "lane": c.Lane, "pos": c.Pos, "version": c.Version}
	}
	commit, err := stardust.MergePatchValue(db, patch, nil)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	ids := make([]int64, kanban.Cards)
	for i := range ids {
		ids[i] = commit.IDs["#_c"+strconv.Itoa(i)]
	}
	return ids, nil
}

func open(db *stardust.Database, text string) (*stardust.Query, error) {
	d := sdx.Doc(text)
	defer d.Close()
	return db.Query(d)
}

// tally collects what the producers measured inside the window.
type tally struct {
	mu                                    sync.Mutex
	lat                                   []time.Duration
	cmds, refused, conflicts, retried, tx int64
}

func (t *tally) add(lat []time.Duration, cmds, refused, conflicts, retried, tx int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lat = append(t.lat, lat...)
	t.cmds += cmds
	t.refused += refused
	t.conflicts += conflicts
	t.retried += retried
	t.tx += tx
}

func (t *tally) result(mode string, w kanban.Window, extra map[string]float64) kanban.Result {
	secs := w.End.Sub(w.Start).Seconds()
	p50, p99 := kanban.Percentiles(t.lat)
	r := kanban.Result{Store: "stardust", Mode: mode, Bench: "write", Producers: *producers, Seconds: secs,
		Commands: t.cmds, Refused: t.refused, Conflicts: t.conflicts, Retried: t.retried, Transactions: t.tx,
		CmdsPerSec: float64(t.cmds) / secs, P50ms: p50, P99ms: p99, Extra: extra}
	if t.tx > 0 {
		r.CmdsPerTx = float64(t.cmds) / float64(t.tx)
	}
	return r
}

func window() kanban.Window {
	start := time.Now().Add(*warmup)
	return kanban.Window{Start: start, End: start.Add(*duration)}
}

// writeAlone runs every command as one CAS mutation, called by its producer:
// Stardust serialises the commits itself.
func writeAlone(db *stardust.Database, ids []int64) error {
	d := sdx.Doc(casText)
	m, err := db.Mutation(d)
	d.Close()
	if err != nil {
		return err
	}
	defer m.Close()
	w := window()
	syncs := syncWindow(w)
	var t tally
	var wg sync.WaitGroup
	var failed atomic.Value
	for p := range *producers {
		wg.Go(func() {
			mover := kanban.NewMover(p, *producers)
			var lat []time.Duration
			var cmds, refused, conflicts, retried, tx int64
			for {
				start := time.Now()
				if !start.Before(w.End) {
					break
				}
				mv := mover.Next()
				params := map[string]any{"?card": stardust.Link(ids[mv.Card]), "?seen": mv.Seen, "?lane": mv.Lane}
				var commit stardust.Commit
				var ex stardust.MutationExplain
				var conflicted int64
				for {
					commit, err = stardust.RunMutationValue(m, params, stardust.RunOptions{}, &ex)
					if isStatus(err, stardust.StatusConflict) {
						conflicted++
						continue
					}
					break
				}
				if err != nil {
					failed.Store(err)
					return
				}
				outcome := error(nil)
				if commit.Status == stardust.NoMatch {
					outcome = kanban.ErrMoved
				}
				mover.Done(mv, outcome)
				if w.In(start) {
					lat = append(lat, time.Since(start))
					cmds++
					conflicts += conflicted
					retried += int64(max(ex.Attempts-1, 0))
					if outcome != nil {
						refused++
					} else if commit.Status == stardust.Committed {
						tx++
					}
				}
			}
			t.add(lat, cmds, refused, conflicts, retried, tx)
		})
	}
	wg.Wait()
	if err, _ := failed.Load().(error); err != nil {
		return err
	}
	extra := threadsExtra()
	syncs(extra, t.tx)
	t.result("alone", w, extra).Print()
	return nil
}

type request struct {
	mv    kanban.Move
	start time.Time
	done  chan error
}

// writeBatched is lith's writer pattern on Stardust: one goroutine takes
// what is queued, checks each command against what it read and what the
// batch already changed, and commits the batch as one merge patch.
func writeBatched(db *stardust.Database, ids []int64) error {
	point, err := open(db, pointText)
	if err != nil {
		return err
	}
	defer point.Close()
	board, err := open(db, boardText)
	if err != nil {
		return err
	}
	defer board.Close()
	index := map[int64]int{}
	for i, id := range ids {
		index[id] = i
	}
	queue := make(chan *request, 4096)
	var batches, batchCmds atomic.Int64
	var commitNanos, readNanos atomic.Int64
	w := window()
	syncs := syncWindow(w)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- func() error {
			batch := make([]*request, 0, *maxBatch)
			for r := range queue {
				batch = append(batch[:0], r)
			fill:
				for len(batch) < *maxBatch {
					select {
					case r, ok := <-queue:
						if !ok {
							break fill
						}
						batch = append(batch, r)
					default:
						break fill
					}
				}
				readStart := time.Now()
				overlay := map[int]state{}
				if *batchRead == "board" {
					cards, err := readBoard(board, nil)
					if err != nil {
						return err
					}
					for _, c := range cards {
						overlay[index[c.ID]] = state{Lane: c.Lane, Version: c.Version}
					}
				}
				patch := map[string]any{}
				errs := make([]error, len(batch))
				for i, r := range batch {
					st, ok := overlay[r.mv.Card]
					if !ok {
						st, err = readCard(point, ids[r.mv.Card])
						if err != nil {
							return err
						}
					}
					if st.Version != r.mv.Seen {
						errs[i] = kanban.ErrMoved
						continue
					}
					st = state{Lane: r.mv.Lane, Version: st.Version + 1}
					overlay[r.mv.Card] = st
					patch[strconv.FormatInt(ids[r.mv.Card], 10)] = st
				}
				commitStart := time.Now()
				readNanos.Add(int64(commitStart.Sub(readStart)))
				if len(patch) > 0 {
					if _, err := stardust.MergePatchValue(db, patch, nil); err != nil {
						return err
					}
				}
				end := time.Now()
				if w.In(batch[0].start) {
					batches.Add(1)
					batchCmds.Add(int64(len(batch)))
					commitNanos.Add(int64(end.Sub(commitStart)))
				}
				for i, r := range batch {
					r.done <- errs[i]
				}
			}
			return nil
		}()
	}()
	var t tally
	var wg sync.WaitGroup
	for p := range *producers {
		wg.Go(func() {
			mover := kanban.NewMover(p, *producers)
			done := make(chan error, 1)
			var lat []time.Duration
			var cmds, refused int64
			for {
				start := time.Now()
				if !start.Before(w.End) {
					break
				}
				mv := mover.Next()
				queue <- &request{mv: mv, start: start, done: done}
				err := <-done
				mover.Done(mv, err)
				if w.In(start) {
					lat = append(lat, time.Since(start))
					cmds++
					if err != nil {
						refused++
					}
				}
			}
			t.add(lat, cmds, refused, 0, 0, 0)
		})
	}
	wg.Wait()
	close(queue)
	if err := <-writerDone; err != nil {
		return err
	}
	t.tx = batches.Load()
	extra := threadsExtra()
	if n := batches.Load(); n > 0 {
		extra["commit_ms_per_batch"] = float64(commitNanos.Load()) / float64(n) / 1e6
		extra["read_ms_per_batch"] = float64(readNanos.Load()) / float64(n) / 1e6
		extra["cmds_per_batch_writer"] = float64(batchCmds.Load()) / float64(n)
	}
	syncs(extra, t.tx)
	name := "batched"
	if *batchRead == "board" {
		name = "batched, board read per batch"
	}
	t.result(name, w, extra).Print()
	return nil
}

func readCard(point *stardust.Query, id int64) (state, error) {
	rows, err := stardust.RunQueryValue(point, map[string]any{"?card": stardust.Link(id)}, stardust.RunOptions{}, nil)
	if err != nil {
		return state{}, err
	}
	defer rows.Close()
	for st, err := range stardust.Values[state](rows) {
		return st, err
	}
	return state{}, fmt.Errorf("card %d not found", id)
}

func readBoard(q *stardust.Query, params any) ([]kanban.Card, error) {
	rows, err := stardust.RunQueryValue(q, params, stardust.RunOptions{}, nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cards := make([]kanban.Card, 0, kanban.Cards)
	for r, err := range stardust.Values[row](rows) {
		if err != nil {
			return nil, err
		}
		cards = append(cards, kanban.Card{ID: int64(r.E), Title: r.Title, Lane: r.Lane, Pos: r.Pos, Version: r.Version})
	}
	return cards, nil
}

// read times the render-shaped read: the whole board in one query run.
func read(db *stardust.Database) error {
	q, err := open(db, boardText)
	if err != nil {
		return err
	}
	defer q.Close()
	qa, err := open(db, boardAsOfText)
	if err != nil {
		return err
	}
	defer qa.Close()
	head, err := headTx(db)
	if err != nil {
		return err
	}
	variants := []struct {
		name string
		fn   func() error
	}{
		{"prepared+decode", func() error {
			cards, err := readBoard(q, nil)
			if err == nil && len(cards) != kanban.Cards {
				err = fmt.Errorf("board has %d cards", len(cards))
			}
			return err
		}},
		{"prepared, document only", func() error {
			rows, err := q.Run(nil, stardust.RunOptions{}, nil)
			if err != nil {
				return err
			}
			defer rows.Close()
			all, err := rows.AllPages()
			if err == nil {
				all.Close()
			}
			return err
		}},
		{"parse+open+decode", func() error {
			q2, err := open(db, boardText)
			if err != nil {
				return err
			}
			defer q2.Close()
			_, err = readBoard(q2, nil)
			return err
		}},
		{"asOf head, prepared+decode", func() error {
			cards, err := readBoard(qa, map[string]any{"?at": head})
			if err == nil && len(cards) != kanban.Cards {
				err = fmt.Errorf("board has %d cards", len(cards))
			}
			return err
		}},
	}
	extra := map[string]float64{}
	for _, v := range variants {
		for range 50 {
			if err := v.fn(); err != nil {
				return fmt.Errorf("%s: %w", v.name, err)
			}
		}
		lat := make([]time.Duration, 0, *iters)
		for range *iters {
			start := time.Now()
			if err := v.fn(); err != nil {
				return err
			}
			lat = append(lat, time.Since(start))
		}
		p50, p99 := kanban.Percentiles(lat)
		extra[v.name+" p50ms"] = p50
		extra[v.name+" p99ms"] = p99
	}
	var ex stardust.QueryExplain
	rows, err := q.Run(nil, stardust.RunOptions{}, &ex)
	if err != nil {
		return err
	}
	rows.Close()
	extra["explain compile ms"] = float64(ex.Compile) / 1e6
	extra["explain plan ms"] = float64(ex.Plan) / 1e6
	extra["explain execute ms"] = float64(ex.Execute) / 1e6
	extra["explain order ms"] = float64(ex.Order) / 1e6
	extra["explain total wall ms"] = float64(ex.Total.Wall) / 1e6
	p50 := extra["prepared+decode p50ms"]
	kanban.Result{Store: "stardust", Mode: "board", Bench: "read", P50ms: p50, P99ms: extra["prepared+decode p99ms"], Extra: extra}.Print()
	return nil
}

// headTx commits nothing and learns the newest transaction from a
// subscription's first event, the one public place that reports the head.
func headTx(db *stardust.Database) (int64, error) {
	q, err := open(db, pointText)
	if err != nil {
		return 0, err
	}
	defer q.Close()
	s, err := stardust.QueryValue(db, sdx.Doc("find  [?e]\nwhere [[?e board 1]]"))
	if err != nil {
		return 0, err
	}
	defer s.Close()
	sub, err := s.Subscribe(nil, stardust.RunOptions{})
	if err != nil {
		return 0, err
	}
	defer sub.Close()
	ev, err := sub.Next(context.Background(), 5*time.Second)
	if err != nil || ev == nil {
		return 0, fmt.Errorf("first event: %v", err)
	}
	ev.Result.Close()
	return ev.Transaction, nil
}

type stamped struct {
	at time.Time
	tx int64
}

// sub measures the time from a commit to the event of a subscription to
// the board query, with a pause between commits so each gets its own event.
func sub(db *stardust.Database, ids []int64) error {
	var events chan stamped
	if !*noSub {
		q, err := open(db, boardText)
		if err != nil {
			return err
		}
		defer q.Close()
		s, err := q.Subscribe(nil, stardust.RunOptions{})
		if err != nil {
			return err
		}
		defer s.Close()
		events = make(chan stamped, 1024)
		go func() {
			defer close(events)
			for {
				ev, err := s.Next(context.Background(), time.Hour)
				if err != nil {
					return
				}
				if ev == nil {
					continue
				}
				at := time.Now()
				ev.Result.Close()
				events <- stamped{at, ev.Transaction}
			}
		}()
		<-events
	}
	var commitLat, afterReturn, afterStart []time.Duration
	var early int
	sync0, _, _ := syncTotals()
	for i := range *iters {
		time.Sleep(*gap)
		card := i % kanban.Cards
		start := time.Now()
		c, err := stardust.MergePatchValue(db, map[string]any{strconv.FormatInt(ids[card], 10): map[string]any{"lane": int64((i + card) % kanban.Lanes), "version": nextValue()}}, nil)
		returned := time.Now()
		if err != nil {
			return err
		}
		commitLat = append(commitLat, returned.Sub(start))
		if events == nil {
			continue
		}
		timeout := time.After(2 * time.Second)
	wait:
		for {
			select {
			case ev := <-events:
				if ev.tx < c.Transaction {
					continue
				}
				if ev.at.Before(returned) {
					early++
				}
				afterReturn = append(afterReturn, max(ev.at.Sub(returned), 0))
				afterStart = append(afterStart, ev.at.Sub(start))
				break wait
			case <-timeout:
				return fmt.Errorf("no event for tx %d", c.Transaction)
			}
		}
	}
	sync1, _, _ := syncTotals()
	c50, c99 := kanban.Percentiles(commitLat)
	r50, r99 := kanban.Percentiles(afterReturn)
	s50, s99 := kanban.Percentiles(afterStart)
	mode := "board subscription"
	if *noSub {
		mode = "commits only"
	}
	kanban.Result{Store: "stardust", Mode: mode, Bench: "sub", P50ms: r50, P99ms: r99, Extra: map[string]float64{
		"commit p50ms": c50, "commit p99ms": c99, "from commit start p50ms": s50, "from commit start p99ms": s99,
		"events before the commit call returned": float64(early), "commits": float64(len(commitLat)),
		"syncs per commit": float64(sync1-sync0) / float64(len(commitLat)),
	}}.Print()
	return nil
}

// subscale opens many subscriptions and measures what one commit costs:
// none (the baseline), identical board subscriptions, and one point
// subscription per card. Each commit changes one card's version.
func subscale(db *stardust.Database, ids []int64) error {
	for _, kind := range []string{"none", "identical board", "one per card"} {
		if err := subscaleKind(db, ids, kind); err != nil {
			return err
		}
	}
	return nil
}

func subscaleKind(db *stardust.Database, ids []int64, kind string) error {
	n := *subs
	text := boardText
	switch kind {
	case "none":
		n = 0
	case "one per card":
		text = pointText
	}
	q, err := open(db, text)
	if err != nil {
		return err
	}
	defer q.Close()
	threads0 := threads()
	received := make([]atomic.Int64, n)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	var all []*stardust.Subscription
	defer func() {
		cancel()
		for _, s := range all {
			s.Close()
		}
		wg.Wait()
	}()
	for i := range n {
		var p *stardust.Document
		if kind == "one per card" {
			if p, err = stardust.FromValue(map[string]any{"?card": stardust.Link(ids[i%kanban.Cards])}); err != nil {
				return err
			}
		}
		s, err := q.Subscribe(p, stardust.RunOptions{})
		p.Close()
		if err != nil {
			return err
		}
		all = append(all, s)
		wg.Go(func() {
			for {
				ev, err := s.Next(ctx, time.Hour)
				if err != nil {
					return
				}
				if ev != nil {
					ev.Result.Close()
					received[i].Add(1)
				}
			}
		})
	}
	time.Sleep(time.Second)
	threads1 := threads()
	const commits = 50
	var lat []time.Duration
	var notified int64
	var cpu time.Duration
	for i := range commits {
		time.Sleep(*gap)
		before := make([]int64, n)
		for k := range before {
			before[k] = received[k].Load()
		}
		card := (i * 7) % kanban.Cards
		var ru0, ru1 syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
		if _, err := stardust.MergePatchValue(db, map[string]any{strconv.FormatInt(ids[card], 10): map[string]any{"version": nextValue()}}, nil); err != nil {
			return err
		}
		returned := time.Now()
		deadline := returned.Add(2 * time.Second)
		for n > 0 && time.Now().Before(deadline) {
			got := 0
			for k := range before {
				if received[k].Load() > before[k] {
					got++
				}
			}
			if got == n {
				lat = append(lat, time.Since(returned))
				break
			}
			time.Sleep(100 * time.Microsecond)
		}
		time.Sleep(20 * time.Millisecond)
		syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
		cpu += time.Duration(ru1.Utime.Nano() + ru1.Stime.Nano() - ru0.Utime.Nano() - ru0.Stime.Nano())
		for k := range before {
			if received[k].Load() > before[k] {
				notified++
			}
		}
	}
	p50, p99 := kanban.Percentiles(lat)
	kanban.Result{Store: "stardust", Mode: kind, Bench: "subscale", Producers: n, P50ms: p50, P99ms: p99, Extra: map[string]float64{
		"threads before":                    float64(threads0),
		"threads with subscriptions":        float64(threads1),
		"subscriptions notified per commit": float64(notified) / commits,
		"all notified within 2s, commits":   float64(len(lat)),
		"process cpu ms per commit":         float64(cpu) / commits / 1e6,
	}}.Print()
	return nil
}

func threads() int {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Threads:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

func threadsExtra() map[string]float64 {
	return map[string]float64{"threads": float64(threads())}
}

func isStatus(err error, s stardust.Status) bool {
	var e *stardust.Error
	return errors.As(err, &e) && e.Status == s
}

var counter atomic.Int64

// nextValue is a version no commit has written yet, so every patch commits.
func nextValue() int64 { return 1_000_000 + counter.Add(1) }

// grow measures the file: after the seed, after commits that each move ten
// cards, and after as many again with history retention of one transaction.
func grow(db *stardust.Database, ids []int64, path string) error {
	size := func() int64 {
		st, err := os.Stat(path)
		if err != nil {
			return -1
		}
		return st.Size()
	}
	extra := map[string]float64{"bytes after seed": float64(size())}
	commits := *iters
	move := func(i int) error {
		patch := map[string]any{}
		for k := range 10 {
			card := (i*10 + k) % kanban.Cards
			patch[strconv.FormatInt(ids[card], 10)] = map[string]any{"lane": int64((i + k) % kanban.Lanes), "version": nextValue()}
		}
		_, err := stardust.MergePatchValue(db, patch, nil)
		return err
	}
	for i := range commits {
		if err := move(i); err != nil {
			return err
		}
	}
	extra["bytes after moves"] = float64(size())
	extra["facts written"] = float64(commits * 20)
	extra["bytes per fact"] = (extra["bytes after moves"] - extra["bytes after seed"]) / extra["facts written"]
	retain := map[string]any{}
	for _, id := range ids {
		retain[strconv.FormatInt(id, 10)] = map[string]any{"stardust/history": map[string]any{"retain": 1}}
	}
	if _, err := stardust.MergePatchValue(db, retain, nil); err != nil {
		return fmt.Errorf("retention: %w", err)
	}
	time.Sleep(2 * time.Second)
	extra["bytes after retention setting"] = float64(size())
	for i := range commits {
		if err := move(commits + i); err != nil {
			return err
		}
	}
	time.Sleep(2 * time.Second)
	extra["bytes after moves with retention"] = float64(size())
	kanban.Result{Store: "stardust", Mode: "file size", Bench: "grow", Transactions: int64(2 * commits), Extra: extra}.Print()
	return nil
}
