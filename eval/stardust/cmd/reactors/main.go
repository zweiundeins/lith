// Command reactors measures live mutations (the reactors plugin) on the
// kanban board: what they cost the write path, how long their output takes
// to become visible, and what a failing round does.
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
	bench     = flag.String("bench", "semantics", "semantics, write, delay or failure")
	reactor   = flag.String("reactor", "none", "none, count, done or both")
	producers = flag.Int("producers", 1, "producers for the write bench")
	warmup    = flag.Duration("warmup", time.Second, "time before measuring")
	duration  = flag.Duration("duration", 5*time.Second, "measured time")
	iters     = flag.Int("iters", 100, "commits for the delay bench")
	gap       = flag.Duration("gap", 60*time.Millisecond, "pause between commits in the delay bench")
	pluginSet = flag.String("plugins", "default", "default (history and reactors) or none")
	batchWin  = flag.Duration("commitbatch", 0, "Options.CommitBatch (0: library default, 20 ms)")
	noSync    = flag.Bool("nosync", false, "delay bench: do not call reactors.sync after each commit")
	forceSync = flag.Bool("forcesync", false, "delay bench: call reactors.sync after each commit even without live mutations")
)

func main() {
	flag.Parse()
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_STACK, &lim); err == nil && lim.Cur == ^uint64(0) {
		fmt.Fprintln(os.Stderr, "reactors: run under `ulimit -s 8192` (an unlimited stack makes every write block)")
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "reactors:", err)
		os.Exit(1)
	}
}

type board struct {
	db    *stardust.Database
	cards []int64 // entity ids, by card index
	lanes []int64
}

func run() error {
	if err := sdx.Activate(*token); err != nil {
		return err
	}
	path := filepath.Join(*dir, fmt.Sprintf("reactors-%s-%s-%d.db", *bench, *reactor, os.Getpid()))
	defer os.Remove(path)
	opts := stardust.Options{CommitBatch: *batchWin, Plugins: sdx.DefaultPlugins()}
	if *pluginSet == "none" {
		opts.Plugins = map[string]any{}
	}
	db, err := stardust.Open(path, opts)
	if err != nil {
		return err
	}
	defer db.Close()
	b, err := seed(db)
	if err != nil {
		return err
	}
	switch *bench {
	case "semantics":
		return semantics(b)
	case "write":
		if err := install(b, *reactor); err != nil {
			return err
		}
		return write(b)
	case "delay":
		if err := install(b, *reactor); err != nil {
			return err
		}
		return delay(b)
	case "failure":
		return failure(b)
	}
	return fmt.Errorf("unknown bench %q", *bench)
}

func seed(db *stardust.Database) (*board, error) {
	patch := map[string]any{}
	for i := range kanban.Cards {
		c := kanban.Seed(i)
		patch["#_c"+strconv.Itoa(i)] = map[string]any{"board": 1, "title": c.Title, "lane": c.Lane, "pos": c.Pos, "version": c.Version}
	}
	for l := range kanban.Lanes {
		patch["#_l"+strconv.Itoa(l)] = map[string]any{"lindex": l, "cards": 0}
	}
	commit, err := stardust.MergePatchValue(db, patch, nil)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	b := &board{db: db}
	for i := range kanban.Cards {
		b.cards = append(b.cards, commit.IDs["#_c"+strconv.Itoa(i)])
	}
	for l := range kanban.Lanes {
		b.lanes = append(b.lanes, commit.IDs["#_l"+strconv.Itoa(l)])
	}
	return b, nil
}

func define(b *board, path, text string) error {
	d, err := stardust.Parse([]byte(text), stardust.DUST)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer d.Close()
	if _, err := b.db.PatchDefinition(stardust.Mutations, path, d); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func install(b *board, which string) error {
	if which == "count" || which == "both" {
		if err := define(b, "kanban/lane-count", laneCountText); err != nil {
			return err
		}
	}
	if which == "done" || which == "both" {
		if err := define(b, "kanban/done-at", doneAtText); err != nil {
			return err
		}
	}
	if which != "none" {
		return sdx.ReactorsSync(b.db, 5*time.Second)
	}
	return nil
}

func query(b *board, text string) (string, error) {
	q, err := b.db.Query(sdx.Doc(text))
	if err != nil {
		return "", err
	}
	defer q.Close()
	rows, err := q.Run(nil, stardust.RunOptions{}, nil)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	all, err := rows.AllPages()
	if err != nil {
		return "", err
	}
	defer all.Close()
	return oneLine(sdx.Text(all)), nil
}

func status(b *board) string {
	s, err := b.db.Reactors().Status()
	if err != nil {
		return "status: " + err.Error()
	}
	defer s.Close()
	return oneLine(sdx.Text(s))
}

func move(b *board, card int, lane int64, version int64) (stardust.Commit, error) {
	return stardust.MergePatchValue(b.db, map[string]any{
		strconv.FormatInt(b.cards[card], 10): map[string]any{"lane": lane, "version": version},
		"#_tx":                               map[string]any{"at": now()},
	}, nil)
}

// semantics checks the definitions on a few moves, including a lane that
// runs empty.
func semantics(b *board) error {
	if err := install(b, "both"); err != nil {
		return err
	}
	show := func(label string) error {
		counts, err := query(b, countsText)
		if err != nil {
			return err
		}
		done, err := query(b, doneText)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n  counts %s\n  done   %d cards stamped\n  status %s\n", label, counts, strings.Count(done, "{#link"), status(b))
		return nil
	}
	if err := show("after install:"); err != nil {
		return err
	}
	c, err := move(b, 0, 3, 1)
	if err != nil {
		return err
	}
	fmt.Printf("moved card 0 to lane 3 in tx %d\n", c.Transaction)
	if err := sdx.ReactorsSync(b.db, 5*time.Second); err != nil {
		return err
	}
	if err := show("after one move:"); err != nil {
		return err
	}
	patch := map[string]any{}
	for i := range kanban.Cards {
		if kanban.Seed(i).Lane == 0 {
			patch[strconv.FormatInt(b.cards[i], 10)] = map[string]any{"lane": 1}
		}
	}
	if _, err := stardust.MergePatchValue(b.db, patch, nil); err != nil {
		return err
	}
	if err := sdx.ReactorsSync(b.db, 5*time.Second); err != nil {
		return err
	}
	return show("after emptying lane 0:")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

type request struct {
	mv    kanban.Move
	start time.Time
	done  chan error
}

// write runs the lith-style batched writer of sdbench: per-card reads, one
// merge patch per batch.
func write(b *board) error {
	point, err := b.db.Query(sdx.Doc("find  [?lane ?version]\nwhere [[?card lane ?lane]\n       [?card version ?version]]"))
	if err != nil {
		return err
	}
	defer point.Close()
	type state struct {
		Lane    int64 `json:"lane"`
		Version int64 `json:"version"`
	}
	read := func(id int64) (state, error) {
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
	queue := make(chan *request, 4096)
	start := time.Now().Add(*warmup)
	w := kanban.Window{Start: start, End: start.Add(*duration)}
	var batches atomic.Int64
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- func() error {
			batch := make([]*request, 0, 512)
			for r := range queue {
				batch = append(batch[:0], r)
			fill:
				for len(batch) < 512 {
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
				overlay := map[int]state{}
				patch := map[string]any{}
				errs := make([]error, len(batch))
				for i, r := range batch {
					st, ok := overlay[r.mv.Card]
					if !ok {
						if st, err = read(b.cards[r.mv.Card]); err != nil {
							return err
						}
					}
					if st.Version != r.mv.Seen {
						errs[i] = kanban.ErrMoved
						continue
					}
					st = state{Lane: r.mv.Lane, Version: st.Version + 1}
					overlay[r.mv.Card] = st
					patch[strconv.FormatInt(b.cards[r.mv.Card], 10)] = st
				}
				patch["#_tx"] = map[string]any{"at": now()}
				if len(patch) > 1 {
					if _, err := stardust.MergePatchValue(b.db, patch, nil); err != nil {
						return err
					}
				}
				if w.In(batch[0].start) {
					batches.Add(1)
				}
				for i, r := range batch {
					r.done <- errs[i]
				}
			}
			return nil
		}()
	}()
	var ru0, ru1 syscall.Rusage
	var cpuOnce sync.Once
	cpuStart := time.AfterFunc(time.Until(w.Start), func() { cpuOnce.Do(func() { syscall.Getrusage(syscall.RUSAGE_SELF, &ru0) }) })
	defer cpuStart.Stop()
	var cmds atomic.Int64
	var wg sync.WaitGroup
	for p := range *producers {
		wg.Go(func() {
			mover := kanban.NewMover(p, *producers)
			done := make(chan error, 1)
			for {
				begin := time.Now()
				if !begin.Before(w.End) {
					return
				}
				mv := mover.Next()
				queue <- &request{mv: mv, start: begin, done: done}
				err := <-done
				mover.Done(mv, err)
				if w.In(begin) {
					cmds.Add(1)
				}
			}
		})
	}
	wg.Wait()
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	close(queue)
	if err := <-writerDone; err != nil {
		return err
	}
	lagStatus := status(b)
	drainStart := time.Now()
	syncErr := sdx.ReactorsSync(b.db, time.Minute)
	drain := time.Since(drainStart)
	secs := w.End.Sub(w.Start).Seconds()
	cpu := time.Duration(ru1.Utime.Nano() + ru1.Stime.Nano() - ru0.Utime.Nano() - ru0.Stime.Nano())
	extra := map[string]float64{
		"cpu ms per command":  float64(cpu) / float64(max(cmds.Load(), 1)) / 1e6,
		"drain after stop ms": float64(drain) / 1e6,
	}
	if c, _, ok := syncTotals(); ok {
		extra["syncs total"] = float64(c)
	}
	kanban.Result{Store: "stardust", Mode: "reactor " + *reactor, Bench: "write", Producers: *producers, Seconds: secs,
		Commands: cmds.Load(), Transactions: batches.Load(), CmdsPerSec: float64(cmds.Load()) / secs,
		CmdsPerTx: float64(cmds.Load()) / float64(max(batches.Load(), 1)), Extra: extra}.Print()
	fmt.Fprintf(os.Stderr, "status when producers stopped: %s\nsync: %v\n", lagStatus, syncErr)
	return nil
}

// delay times a commit and the event of a subscription to the reactor's
// output, one commit at a time.
func delay(b *board) error {
	text := countsText
	if *reactor == "done" {
		text = doneText
	}
	q, err := b.db.Query(sdx.Doc(text))
	if err != nil {
		return err
	}
	defer q.Close()
	s, err := q.Subscribe(nil, stardust.RunOptions{})
	if err != nil {
		return err
	}
	defer s.Close()
	type stamped struct {
		at time.Time
		tx int64
	}
	events := make(chan stamped, 1024)
	go func() {
		defer close(events)
		for {
			ev, err := s.Next(context.Background(), time.Hour)
			if err != nil {
				return
			}
			if ev != nil {
				at := time.Now()
				ev.Result.Close()
				events <- stamped{at, ev.Transaction}
			}
		}
	}()
	<-events
	var commitLat, outLat, syncLat []time.Duration
	var ru0, ru1 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	c0, _, _ := syncTotals()
	for i := range *iters {
		time.Sleep(*gap)
		card := (i * 7) % kanban.Cards
		lane := int64(3)
		if *reactor != "done" {
			lane = int64((i + 1) % kanban.Lanes)
		}
		begin := time.Now()
		c, err := stardust.MergePatchValue(b.db, map[string]any{
			strconv.FormatInt(b.cards[card], 10): map[string]any{"lane": lane, "version": int64(10_000 + i)},
			"#_tx":                               map[string]any{"at": now()},
		}, nil)
		returned := time.Now()
		if err != nil {
			return err
		}
		if (*reactor != "none" || *forceSync) && !*noSync {
			if err := sdx.ReactorsSync(b.db, 2*time.Second); err != nil {
				return fmt.Errorf("sync: %w", err)
			}
		}
		synced := time.Now()
		timeout := time.After(2 * time.Second)
		if *reactor == "none" {
			timeout = time.After(0)
		}
	wait:
		for {
			select {
			case ev := <-events:
				if ev.tx > c.Transaction {
					outLat = append(outLat, ev.at.Sub(returned))
					break wait
				}
			case <-timeout:
				break wait
			}
		}
		commitLat = append(commitLat, returned.Sub(begin))
		syncLat = append(syncLat, synced.Sub(returned))
	}
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	c1, _, _ := syncTotals()
	cp50, cp99 := kanban.Percentiles(commitLat)
	o50, o99 := kanban.Percentiles(outLat)
	s50, s99 := kanban.Percentiles(syncLat)
	cpu := time.Duration(ru1.Utime.Nano() + ru1.Stime.Nano() - ru0.Utime.Nano() - ru0.Stime.Nano())
	kanban.Result{Store: "stardust", Mode: "reactor " + *reactor, Bench: "delay", P50ms: o50, P99ms: o99, Extra: map[string]float64{
		"commit p50ms": cp50, "commit p99ms": cp99, "sync after commit p50ms": s50, "sync after commit p99ms": s99,
		"output events seen": float64(len(outLat)), "commits": float64(len(commitLat)),
		"syncs per commit":  float64(c1-c0) / float64(len(commitLat)),
		"cpu ms per commit": float64(cpu) / float64(len(commitLat)) / 1e6,
	}}.Print()
	return nil
}

// failure installs the lane count and two live mutations that conflict,
// then watches a triggering commit, later commits and the repair.
func failure(b *board) error {
	if err := install(b, "count"); err != nil {
		return err
	}
	counts := func() string {
		s, err := query(b, countsText)
		if err != nil {
			return err.Error()
		}
		return s
	}
	fmt.Println("before:", counts())
	if err := define(b, "kanban/conflict-a", conflictAText); err != nil {
		return err
	}
	if err := define(b, "kanban/conflict-b", conflictBText); err != nil {
		return err
	}
	c, err := move(b, 0, 3, 1)
	fmt.Printf("triggering move: tx %d status %d err %v\n", c.Transaction, c.Status, err)
	err = sdx.ReactorsSync(b.db, 3*time.Second)
	var se *stardust.Error
	if errors.As(err, &se) {
		fmt.Printf("sync: status %d code %q\n", se.Status, se.Code)
	} else {
		fmt.Println("sync:", err)
	}
	fmt.Println("status:", status(b))
	fmt.Println("counts:", counts())
	for i := 1; i <= 3; i++ {
		c, err := move(b, i, 3, 1)
		fmt.Printf("later move %d: tx %d err %v\n", i, c.Transaction, err)
	}
	fmt.Println("counts after later moves:", counts())
	fmt.Println("status:", status(b))
	start := time.Now()
	if err := define(b, "kanban/conflict-b", "priority 1"); err != nil {
		return err
	}
	err = sdx.ReactorsSync(b.db, 10*time.Second)
	fmt.Printf("after raising conflict-b's priority: sync %v in %v\n", err, time.Since(start).Round(time.Millisecond))
	fmt.Println("counts:", counts())
	fmt.Println("status:", status(b))
	return nil
}

// now is the current time in whole 100 ns ticks: the binding refuses a
// time.Time with finer precision (inexact_number).
func now() time.Time { return time.Now().Truncate(100 * time.Nanosecond) }
