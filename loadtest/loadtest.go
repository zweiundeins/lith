// Package loadtest measures a project's views and commands under load, in
// process: a writer and a live hub on one database, streams without a
// network, and producers that send commands on a fixed schedule.
//
// The render loop works in ticks, so the whole system can be measured
// without an HTTP server (Anders Murphy, "Discrete time based systems are
// simple to benchmark"). A load tool in another process on the same
// machine takes CPU from the server, and a server with back pressure does
// less work when its load tool falls behind. Here every stream takes its
// bytes at once, after compressing them as a stream to a browser does, and
// each producer sends its commands when they are due, whether or not the
// ones before are done. What is left out is the network: time on the wire
// and slow clients.
//
// Run it before and after a change, with the project's own views and
// commands:
//
//	r, err := loadtest.Run(ctx, loadtest.Config{
//		SQLite:    sqlite.Options{Migrations: migrations},
//		Seed:      seedBoard{},
//		Streams:   200,
//		Stream:    func(i int) live.Spec { return live.Spec{Views: []live.View{boardView(1)}} },
//		Producers: 200,
//		Rate:      800,
//		Command:   func(p int, i int64) writer.Command { return randomMove(p, i) },
//		Warmup:    2 * time.Second,
//		Duration:  10 * time.Second,
//	})
//	fmt.Println(r)
//
// Board is the scenario of lith's own benchmarks.
package loadtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// Config describes a load test. Duration is required, Stream when there
// are Streams, and Command when there is a Rate.
type Config struct {
	// DB is the database to run on, opened and migrated by the caller,
	// who closes it after Run. Nil: Run opens a new one in a temporary
	// directory and removes it afterwards.
	DB *sqlite.DB
	// SQLite opens the temporary database. Synchronous defaults to FULL,
	// Readers to one for each render worker and four more.
	SQLite sqlite.Options
	// Seed, if set, runs once before the streams open, to fill the
	// tables.
	Seed writer.Command

	// Streams is how many streams to open. Stream describes stream i,
	// for 0 <= i < Streams: the views it watches.
	Streams int
	Stream  func(i int) live.Spec
	// AcceptEncoding holds the Accept-Encoding headers the streams send,
	// in turn: stream i sends AcceptEncoding[i%len(AcceptEncoding)]
	// (default zstd).
	AcceptEncoding []string

	// Producers send Rate commands a second between them (default one
	// producer), each command when it is due. Command makes producer p's
	// i-th command, counting from 0; the producers call it concurrently.
	Producers int
	Rate      float64
	Command   func(p int, i int64) writer.Command

	// Warmup is how long the load runs before it is measured, and
	// Duration how long it is measured.
	Warmup   time.Duration
	Duration time.Duration

	// Hub and Writer configure the hub and the writer. Run sets Hub.Read
	// to the database's read pool and raises Hub.MaxStreams to Streams;
	// Hub.OnTick and Writer.OnCommit are called along with Run's own.
	Hub    live.Options
	Writer writer.Options
}

// errEnded stops a run in which a stream ended on its own.
var errEnded = errors.New("loadtest: a stream ended before the run")

// Run starts a writer and a hub on the database, runs Seed, opens the
// streams and waits until each has had its first frame. Then the
// producers send commands for Warmup and Duration, and Run reports on
// Duration. It fails when a stream ends before the run does, or when ctx
// ends.
func Run(ctx context.Context, c Config) (rep Report, err error) {
	if err := c.check(); err != nil {
		return Report{}, err
	}
	db := c.DB
	if db == nil {
		dir, err := os.MkdirTemp("", "lith-loadtest-")
		if err != nil {
			return Report{}, err
		}
		defer os.RemoveAll(dir)
		o := c.SQLite
		if o.Synchronous == "" {
			o.Synchronous = "FULL"
		}
		if o.Readers <= 0 {
			o.Readers = workers(c.Hub) + 4
		}
		if db, err = sqlite.Open(ctx, filepath.Join(dir, "loadtest.db"), o); err != nil {
			return Report{}, err
		}
		defer db.Close()
	}
	r := &run{c: c, ctx: ctx, lost: make(chan struct{})}

	ho := c.Hub
	ho.Read = db.R
	ho.MaxStreams = max(ho.MaxStreams, c.Streams)
	ho.OnTick = r.onTick
	r.hub = live.New(ho)
	wo := c.Writer
	wo.OnCommit = func(topics []string) {
		r.hub.Changed(topics)
		if c.Writer.OnCommit != nil {
			c.Writer.OnCommit(topics)
		}
	}
	if r.w, err = writer.New(db, wo); err != nil {
		return Report{}, err
	}
	go r.w.Run(context.Background())
	defer func() { err = errors.Join(err, r.w.Close()) }()

	hubCtx, stopHub := context.WithCancel(context.Background())
	hubDone := make(chan error, 1)
	go func() { hubDone <- r.hub.Run(hubCtx) }()
	defer func() {
		stopHub()
		err = errors.Join(err, <-hubDone)
	}()

	if c.Seed != nil {
		if err := r.w.Do(ctx, c.Seed); err != nil {
			return Report{}, fmt.Errorf("loadtest: seed: %w", err)
		}
	}
	heapBefore := heapInUse()

	defer func() { err = errors.Join(err, r.closeStreams()) }()
	r.openStreams()
	if err := r.firstFrames(); err != nil {
		return Report{}, err
	}

	start := time.Now()
	r.from = start.Add(c.Warmup)
	pctx, stopProducers := context.WithCancel(ctx)
	var producers sync.WaitGroup
	for p := range c.Producers {
		producers.Go(func() { r.produce(pctx, p, start) })
	}
	stop := sync.OnceFunc(func() {
		stopProducers()
		producers.Wait()
		r.inflight.Wait()
	})
	defer stop()

	if err := r.sleep(time.Until(r.from)); err != nil {
		return Report{}, err
	}
	r.recordTicks(true)
	s0 := r.snapshot()
	if err := r.sleep(c.Duration); err != nil {
		return Report{}, err
	}
	s1 := r.snapshot()
	r.recordTicks(false)
	stop()
	r.settle()
	return r.report(s0, s1, heapInUse()-heapBefore), nil
}

func (c *Config) check() error {
	switch {
	case c.Duration <= 0:
		return errors.New("loadtest: Config.Duration must be positive")
	case c.Warmup < 0 || c.Streams < 0 || c.Producers < 0 || c.Rate < 0:
		return errors.New("loadtest: Config has a negative Warmup, Streams, Producers or Rate")
	case c.Streams > 0 && c.Stream == nil:
		return errors.New("loadtest: Config.Stream is required with Streams")
	case c.Rate > 0 && c.Command == nil:
		return errors.New("loadtest: Config.Command is required with a Rate")
	case c.Rate == 0 && c.Producers > 0:
		return errors.New("loadtest: Config.Rate is required with Producers")
	}
	if c.Rate > 0 && c.Producers == 0 {
		c.Producers = 1
	}
	return nil
}

func workers(o live.Options) int {
	if o.Workers > 0 {
		return o.Workers
	}
	return runtime.GOMAXPROCS(0)
}

type run struct {
	c   Config
	ctx context.Context
	hub *live.Hub
	w   *writer.Writer

	clients  []*client
	served   sync.WaitGroup
	closing  atomic.Bool
	errs     []error // what each stream's Serve returned
	early    []bool  // streams that ended before closeStreams
	lost     chan struct{}
	lostOnce sync.Once
	stopAll  context.CancelFunc

	from     time.Time // when the measured time starts
	inflight sync.WaitGroup

	ticksMu   sync.Mutex
	recording bool
	ticks     []time.Duration

	cmdsMu sync.Mutex
	cmds   []result
}

// result is one command that was due in the measured time.
type result struct {
	due    time.Time
	took   time.Duration // from due to commit
	failed bool
}

func (r *run) openStreams() {
	n := r.c.Streams
	r.clients = make([]*client, n)
	r.errs = make([]error, n)
	r.early = make([]bool, n)
	ctx, cancel := context.WithCancel(context.Background())
	r.stopAll = cancel
	for i := range n {
		ae := sse.Zstd
		if k := len(r.c.AcceptEncoding); k > 0 {
			ae = r.c.AcceptEncoding[i%k]
		}
		coding := sse.Negotiate(ae)
		if coding == "" {
			coding = "identity"
		}
		cl := newClient(coding)
		r.clients[i] = cl
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", ae)
		spec := r.c.Stream(i)
		r.served.Go(func() {
			r.errs[i] = r.hub.Serve(cl, req, spec)
			if !r.closing.Load() {
				r.early[i] = true
				r.lostOnce.Do(func() { close(r.lost) })
			}
		})
	}
}

func (r *run) closeStreams() error {
	if r.stopAll == nil {
		return nil
	}
	r.closing.Store(true)
	r.stopAll()
	r.served.Wait()
	var errs []error
	for i, err := range r.errs {
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("loadtest: stream %d: %w", i, err))
		case r.early[i]:
			errs = append(errs, fmt.Errorf("loadtest: stream %d ended before the run", i))
		}
	}
	return errors.Join(errs...)
}

// firstFrames waits until every stream has had a write: its views'
// first frames.
func (r *run) firstFrames() error {
	deadline := time.Now().Add(time.Minute)
	for i := 0; i < len(r.clients); {
		if r.clients[i].writes.Load() > 0 {
			i++
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("loadtest: stream %d had no frame after a minute", i)
		}
		if err := r.sleep(5 * time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// sleep waits for d unless ctx ends or a stream does.
func (r *run) sleep(d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-r.ctx.Done():
		return context.Cause(r.ctx)
	case <-r.lost:
		return errEnded
	}
}

// produce sends producer p's commands, Producers/Rate apart, the first
// ones staggered so that the producers take turns.
func (r *run) produce(ctx context.Context, p int, start time.Time) {
	every := time.Duration(float64(time.Second) * float64(r.c.Producers) / r.c.Rate)
	due := start.Add(every * time.Duration(p) / time.Duration(r.c.Producers))
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for i := int64(0); ctx.Err() == nil; i++ {
		if wait := time.Until(due); wait > 0 {
			t.Reset(wait)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
		r.send(p, i, due)
		due = due.Add(every)
	}
}

// send runs a command on a goroutine of its own, as a request handler
// would, so that a slow writer delays no later command.
func (r *run) send(p int, i int64, due time.Time) {
	cmd := r.c.Command(p, i)
	r.inflight.Go(func() {
		err := r.w.Do(r.ctx, cmd)
		if due.Before(r.from) {
			return
		}
		res := result{due: due, took: time.Since(due), failed: err != nil}
		r.cmdsMu.Lock()
		r.cmds = append(r.cmds, res)
		r.cmdsMu.Unlock()
	})
}

func (r *run) onTick(d time.Duration) {
	r.ticksMu.Lock()
	if r.recording {
		r.ticks = append(r.ticks, d)
	}
	r.ticksMu.Unlock()
	if r.c.Hub.OnTick != nil {
		r.c.Hub.OnTick(d)
	}
}

func (r *run) recordTicks(on bool) {
	r.ticksMu.Lock()
	r.recording = on
	r.ticksMu.Unlock()
}

// settle waits until the hub has sent the frames of the last commands:
// three quiet ticks in a row, or forty ticks at most.
func (r *run) settle() {
	tick := r.c.Hub.Tick
	if tick <= 0 {
		tick = 50 * time.Millisecond
	}
	last := r.hub.Stats()
	for quiet, n := 0, 0; quiet < 3 && n < 40; n++ {
		time.Sleep(tick)
		s := r.hub.Stats()
		if s.Ticks == last.Ticks && s.FramesSent == last.FramesSent && s.FramesDropped == last.FramesDropped {
			quiet++
		} else {
			quiet = 0
		}
		last = s
	}
}

type snapshot struct {
	at     time.Time
	cpu    time.Duration
	hub    live.Stats
	writer writer.Stats
	writes []int64
	bytes  []int64
}

func (r *run) snapshot() snapshot {
	s := snapshot{
		at:     time.Now(),
		cpu:    cpuTime(),
		hub:    r.hub.Stats(),
		writer: r.w.Stats(),
		writes: make([]int64, len(r.clients)),
		bytes:  make([]int64, len(r.clients)),
	}
	for i, c := range r.clients {
		s.writes[i] = c.writes.Load()
		s.bytes[i] = c.bytes.Load()
	}
	return s
}

func (r *run) report(s0, s1 snapshot, heap int64) Report {
	rep := Report{
		Streams:       r.c.Streams,
		Producers:     r.c.Producers,
		Rate:          r.c.Rate,
		Duration:      s1.at.Sub(s0.at),
		GOMAXPROCS:    runtime.GOMAXPROCS(0),
		Ticks:         s1.hub.Ticks - s0.hub.Ticks,
		Overruns:      s1.hub.Overruns - s0.hub.Overruns,
		Renders:       s1.hub.Renders - s0.hub.Renders,
		FramesSent:    s1.hub.FramesSent - s0.hub.FramesSent,
		FramesDropped: s1.hub.FramesDropped - s0.hub.FramesDropped,
		Wire:          map[string]Wire{},
		Commands:      s1.writer.Commands - s0.writer.Commands,
		Batches:       s1.writer.Batches - s0.writer.Batches,
		MaxBatch:      s1.writer.MaxBatch,
		P99Commit:     s1.writer.P99Commit,
	}
	if s0.cpu > 0 && rep.Duration > 0 {
		rep.CPU = float64(s1.cpu-s0.cpu) / float64(rep.Duration)
	}
	r.ticksMu.Lock()
	rep.Tick = percentiles(r.ticks)
	r.ticksMu.Unlock()
	for i, c := range r.clients {
		w := rep.Wire[c.coding]
		w.Streams++
		w.Writes += s1.writes[i] - s0.writes[i]
		w.Bytes += s1.bytes[i] - s0.bytes[i]
		rep.Wire[c.coding] = w
	}
	if r.c.Streams > 0 {
		rep.HeapPerStream = max(0, heap) / int64(r.c.Streams)
	}
	r.cmdsMu.Lock()
	var took []time.Duration
	for _, c := range r.cmds {
		if c.due.Before(s0.at) || !c.due.Before(s1.at) {
			continue
		}
		rep.Sent++
		if c.failed {
			rep.Errors++
		}
		took = append(took, c.took)
	}
	r.cmdsMu.Unlock()
	rep.Latency = percentiles(took)
	return rep
}

// heapInUse is the heap in use after a full collection.
func heapInUse() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}
