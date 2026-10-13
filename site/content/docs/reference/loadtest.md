---
title: loadtest
summary: Measures a project's views and commands under load, in process: a writer and a live hub on one database, streams without a network, and producers that send commands on a fixed schedule.
order: 8
---

Import path `github.com/zweiundeins/lith/loadtest`. Generated from the doc comments in [loadtest/](https://github.com/zweiundeins/lith/tree/main/loadtest).

The render loop works in ticks, so the whole system can be measured without an HTTP server (Anders Murphy, "Discrete time based systems are simple to benchmark"). A load tool in another process on the same machine takes CPU from the server, and a server with back pressure does less work when its load tool falls behind. Here every stream takes its bytes at once, after compressing them as a stream to a browser does, and each producer sends its commands when they are due, whether or not the ones before are done. What is left out is the network: time on the wire and slow clients.

Run it before and after a change, with the project's own views and commands:

```go
r, err := loadtest.Run(ctx, loadtest.Config{
	SQLite:    sqlite.Options{Migrations: migrations},
	Seed:      seedBoard{},
	Streams:   200,
	Stream:    func(i int) live.Spec { return live.Spec{Views: []live.View{boardView(1)}} },
	Producers: 200,
	Rate:      800,
	Command:   func(p int, i int64) writer.Command { return randomMove(p, i) },
	Warmup:    2 * time.Second,
	Duration:  10 * time.Second,
})
fmt.Println(r)
```

Board is the scenario of lith's own benchmarks.

### Example {#example}

Ten tabs on one board of 50 cards, which two producers change a hundred times a second. The numbers differ from run to run; fmt.Println(r) lays them all out.

```go
b := loadtest.Board{Rows: 50}
r, err := loadtest.Run(context.Background(), loadtest.Config{
	SQLite:    sqlite.Options{Migrations: b.Migrations()},
	Seed:      b.Seed(),
	Streams:   10,
	Stream:    b.Stream,
	Producers: 2,
	Rate:      100,
	Command:   b.Command,
	Duration:  500 * time.Millisecond,
})
if err != nil {
	log.Fatal(err)
}
fmt.Println(r.Wire[sse.Zstd].Streams, "zstd streams")
fmt.Println(r.Errors, "failed commands")
fmt.Println(r.Ticks > 0 && r.FramesSent > 0)
```

It prints:

```text
10 zstd streams
0 failed commands
true
```

## Types

### type Board {#Board}

```go
type Board struct {
	Boards int // default 1
	Rows   int // cards per board, default 200
}
```

Board is the scenario of lith's own benchmarks: Boards boards of Rows cards each. A board is one view, keyed "board:\<n>", that lists its cards as kanban does, in about 700 bytes of HTML each: 200 cards make the 140 KB of kanban's board. A command adds one to a card's counter and touches its board, so every command changes a view. With one row, each board is a counter: one view per key.

#### func (Board) Command {#Board.Command}

```go
func (b Board) Command(p int, i int64) writer.Command
```

Command adds one to a card, for Config.Command. Its board and card follow from p and i alone, so a run sends the same commands every time.

#### func (Board) Migrations {#Board.Migrations}

```go
func (Board) Migrations() fs.FS
```

Migrations is the schema, for Config.SQLite.Migrations.

#### func (Board) Seed {#Board.Seed}

```go
func (b Board) Seed() writer.Command
```

Seed fills the boards, for Config.Seed.

#### func (Board) Stream {#Board.Stream}

```go
func (b Board) Stream(i int) live.Spec
```

Stream watches board i%Boards, for Config.Stream.

#### func (Board) View {#Board.View}

```go
func (b Board) View(n int) live.View
```

View is board n, for 0 \<= n \< Boards. Its cards are the rows n\*Rows+1 to (n+1)\*Rows.

### type Config {#Config}

```go
type Config struct {
	// DB is the database to run on, opened and migrated by the caller,
	// who closes it after Run. Nil: Run opens a new one in a temporary
	// directory and removes it afterwards.
	DB *sqlite.DB
	// SQLite opens the temporary database. Readers defaults to one for
	// each render worker and four more.
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
```

Config describes a load test. Duration is required, Stream when there are Streams, and Command when there is a Rate.

### type Metric {#Metric}

```go
type Metric struct {
	Value float64
	Unit  string
}
```

Metric is one number of a report, named for testing.B.ReportMetric.

### type Percentiles {#Percentiles}

```go
type Percentiles struct {
	N                  int
	P50, P95, P99, Max time.Duration
}
```

Percentiles describe a distribution of durations.

#### func (Percentiles) String {#Percentiles.String}

```go
func (p Percentiles) String() string
```

### type Report {#Report}

```go
type Report struct {
	Streams    int
	Producers  int
	Rate       float64       // commands a second, as configured
	Duration   time.Duration // the measured time, as it ran
	GOMAXPROCS int
	// CPU is the CPU time the process used, user and system, in cores
	// (0 where the platform does not say).
	CPU float64

	// The render loop, from live.Stats and live.Options.OnTick.
	Ticks         int64
	Tick          Percentiles // the duration of each tick
	Overruns      int64
	Renders       int64
	FramesSent    int64
	FramesDropped int64

	// Wire is what went out to the streams, by content coding
	// ("identity" for none).
	Wire map[string]Wire

	// HeapPerStream is the heap that stays in use once the streams have
	// had their frames, beyond what the run held before they opened,
	// divided by Streams.
	HeapPerStream int64

	// The writer, from writer.Stats: the commands it ran and the
	// transactions it ran them in.
	Commands  int64
	Batches   int64
	MaxBatch  int           // the most commands one transaction took in the whole run
	P99Commit time.Duration // of the last 1024 transactions

	// The producers' commands that were due in the measured time.
	Sent    int64
	Errors  int64       // commands whose Do returned an error
	Latency Percentiles // from when a command was due until its commit
}
```

Report is what a run measured. Counts cover the measured time only, after the warmup.

#### func Run {#Run}

```go
func Run(ctx context.Context, c Config) (rep Report, err error)
```

Run starts a writer and a hub on the database, runs Seed, opens the streams and waits until each has had its first frame. Then the producers send commands for Warmup and Duration, and Run reports on Duration. It fails when a stream ends before the run does, or when ctx ends.

##### Example {#example-Run}

A project measures its own views and commands: here a count on every page and a command that adds to it, with half the streams on brotli.

```go
migrations := fstest.MapFS{"001_count.sql": {Data: []byte(`
	CREATE TABLE count (id INTEGER PRIMARY KEY, n INTEGER NOT NULL);
	INSERT INTO count VALUES (1, 0);`)}}
count := live.View{
	Key:    "count",
	Topics: []string{"count"},
	Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT n FROM count WHERE id = 1`).Scan(&n); err != nil {
			return nil, err
		}
		return sse.PatchElements(fmt.Appendf(nil, `<p id="count">%d</p>`, n)), nil
	},
}
add := writer.Func(func(tx *writer.Tx) error {
	_, err := tx.Exec(`UPDATE count SET n = n + 1 WHERE id = 1`)
	tx.Touch("count")
	return err
})

r, err := loadtest.Run(context.Background(), loadtest.Config{
	SQLite:         sqlite.Options{Synchronous: "NORMAL", Migrations: migrations},
	Streams:        20,
	Stream:         func(int) live.Spec { return live.Spec{Views: []live.View{count}} },
	AcceptEncoding: []string{"gzip, br, zstd", "gzip, br"},
	Producers:      4,
	Rate:           200,
	Command:        func(int, int64) writer.Command { return add },
	Warmup:         100 * time.Millisecond,
	Duration:       500 * time.Millisecond,
})
if err != nil {
	log.Fatal(err)
}
fmt.Println(r.Wire[sse.Zstd].Streams, "zstd and", r.Wire[sse.Brotli].Streams, "brotli streams")
fmt.Println(math.Round(r.RendersPerTick()), "render per tick")
```

It prints:

```text
10 zstd and 10 brotli streams
1 render per tick
```

#### func (Report) CommandsPerBatch {#Report.CommandsPerBatch}

```go
func (r Report) CommandsPerBatch() float64
```

CommandsPerBatch is how many commands a transaction took on average.

#### func (Report) CommandsPerSecond {#Report.CommandsPerSecond}

```go
func (r Report) CommandsPerSecond() float64
```

CommandsPerSecond is how many commands the writer ran a second.

#### func (Report) Metrics {#Report.Metrics}

```go
func (r Report) Metrics() []Metric
```

Metrics lists the report's main numbers with units that benchstat can compare, one wire size per content coding:

```go
for _, m := range r.Metrics() {
	b.ReportMetric(m.Value, m.Unit)
}
```

#### func (Report) RendersPerTick {#Report.RendersPerTick}

```go
func (r Report) RendersPerTick() float64
```

RendersPerTick is the mean number of views a tick rendered.

#### func (Report) String {#Report.String}

```go
func (r Report) String() string
```

String lays the report out in lines, for a log or a benchmark's output.

### type Wire {#Wire}

```go
type Wire struct {
	Streams int
	Writes  int64
	Bytes   int64
}
```

Wire is what the streams of one content coding received. A write carries the frames that reach a stream together: one frame for a stream that watches one view.

#### func (Wire) PerWrite {#Wire.PerWrite}

```go
func (w Wire) PerWrite() float64
```

PerWrite is the mean size of a write on the wire, in bytes.
