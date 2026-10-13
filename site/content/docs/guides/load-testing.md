---
title: Load testing
summary: Run an app's own views and commands under load, in process, with lith/loadtest, read its report, and compare runs before and after a change.
order: 12
---

`lith/loadtest` runs a writer and a hub on one database, opens streams without a network, and sends commands on a fixed schedule, all in one process. It measures what the server does: ticks, renders, frames, bytes on the wire, heap per stream, commits and their latency.

## Why in process

The render loop works in ticks, so the whole system can be measured without an HTTP server. A load tool in another process on the same machine takes CPU from the server, and a server with back pressure does less work when its load tool falls behind. In process, every stream takes its bytes at once, after compressing them as a stream to a browser does, and each producer sends its commands when they are due, whether or not the ones before are done. What a run leaves out is the network: time on the wire, and slow clients.

## A run with the app's own views

A run takes the app's migrations, a seed, a description of each stream and a command per producer. The board app's benchmark runs its board view with its tailored stream writer, and renames cards:

```go source=site/internal/docs/example/web/load_test.go
	return loadtest.Config{
		SQLite: sqlite.Options{Migrations: migrations.FS},
		Seed:   seed,
		Stream: func(int) live.Spec {
			return live.Spec{Views: []live.View{boardView(1)}, WriteTick: tailor()}
		},
		Streams:   streams,
		Producers: streams,
		Rate:      rate,
		Command: func(p int, i int64) writer.Command {
			return board.RenameCard{Card: 1 + (int64(p)*7+i)%100, Title: fmt.Sprint("renamed ", i), At: time.Now()}
		},
		Warmup:   warmup,
		Duration: d,
	}
```

`Command(p, i)` makes producer `p`'s `i`-th command; the producers call it concurrently, `Rate` commands a second between them. Without `DB`, `Run` opens a new database in a temporary directory and removes it afterwards; `Hub` and `Writer` take the app's own options. The benchmark runs it and reports the numbers:

```go source=site/internal/docs/example/web/load_test.go
func BenchmarkBoard(b *testing.B) {
	var r loadtest.Report
	for b.Loop() {
		var err error
		if r, err = loadtest.Run(b.Context(), load(200, 800, 2*time.Second, 10*time.Second)); err != nil {
			b.Fatal(err)
		}
	}
	b.Log("\n" + r.String())
	for _, m := range r.Metrics() {
		b.ReportMetric(m.Value, m.Unit)
	}
	b.ReportMetric(0, "ns/op")
}
```

`TestLoad` runs the same configuration with ten streams for under a second, so the tests keep the benchmark working.

## Reading the report

`Report.String()` lays a run out in lines. One run of the benchmark above, on 2026-10-13 on an Intel i5-13500 with GOMAXPROCS 20, not pinned, while other test suites ran on the machine:

```text
200 streams, 200 producers at 800 commands/s, measured for 10s, GOMAXPROCS 20, 0.38 cores busy
ticks: 161, 1.00 renders each, p50 750µs, p95 1.69ms, p99 2.06ms, max 2.33ms, 0 overruns
frames: 32003 sent, 0 dropped
wire, zstd: 200 streams, 32025 writes, 223 bytes per write
heap: 1.65 MiB per stream
writer: 8001 commands, 800/s, 1.02 per transaction, at most 19, p99 commit 500µs
commands: 8000 due, 0 errors, due to commit p50 650µs, p95 1.21ms, p99 9.36ms, max 28.77ms
```

- **cores busy**: the CPU the whole process used, writer, hub and encoders, without HTTP.
- **ticks**: one render a tick means each change rendered once for all 200 streams; overruns are ticks longer than the tick.
- **frames dropped**: frames a newer one replaced before their stream sent them. In process no client is slow, so drops here mean the loop fell behind.
- **bytes per write**: what a stream sends per tick, compressed. Here a rename changes one lane, and the tailor sends only that lane.
- **heap per stream**: the heap that stays in use once the streams have their frames, divided by the streams: the encoders, mostly.
- **due to commit**: from when a command was due to its commit, the latency a user's action sees in the writer.

## lith's own scenario

`loadtest.Board` is the scenario of lith's benchmarks: boards of cards in about 700 bytes of HTML each, a view per board, and a command that adds one to a card's counter, so every command changes a view. 200 cards make the 140 KB of kanban's board. It provides `Migrations`, `Seed`, `Stream`, `View` and `Command` for a `Config`, and its commands follow from `p` and `i` alone, so every run sends the same ones. The numbers on [Performance](/docs/performance/render-loop) come from it.

## Comparing runs

- Run before and after a change, with `-count 3` or more, and compare with `benchstat`: `Report.Metrics()` names each number with a unit benchstat understands, one wire size per content coding.
- Pin the runs to the same cores, the performance cores of a hybrid CPU: `taskset -c 0-11 go test ./... -run '^$' -bench Board -count 3`. A process that leaves cores idle between ticks runs slower on cores that keep falling idle, so compare runs made under the same conditions.
- Write the numbers into the project's `benchmarks/RESULTS.md`, with the date, the machine, the settings and the command that produced them.

An end-to-end test adds what a run in process leaves out: kanban's `cmd/netem` adds a round trip and bandwidth limits to open streams, and its `cmd/loadtest` drives tabs over HTTP from other cores or another machine.
