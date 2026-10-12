# Numbers and knobs

Measured numbers to plan with, the settings they lead to, and how to measure
again. Numbers carry their machine and date; re-measure before quoting one for
another project.

## Writer

kanban's writer bench (i5-13500, `synchronous=FULL`, 2026-10-07, each move
about fifteen statements):

| People moving at once | Moves per second | Moves per transaction |
| --- | --- | --- |
| 1 | 240 to 340 | 1 |
| 50 | about 5,100 | 25 |
| 200 | about 13,800 | 103 |

- One person is bound by the fsync: 3 to 5 ms per commit on that disk. Under
  load the writer drains more commands into each transaction and they share
  the wait. Nothing else needs tuning for that; it is what the batch loop does.
- modernc.org/sqlite parses and plans every statement on every call. At 200
  people that was 41% of the writer's CPU until the writer prepared each SQL
  string once on its own connection (10,000 to 13,800 moves a second). lith's
  writer does this for every `tx.Exec`; build SQL from constants so the cache
  stays small.
- `synchronous=FULL` costs one fsync per batch, which is the latency floor of a
  lone write. `NORMAL` in WAL mode can lose the last commits on power loss,
  not on a process crash.
- Anders Murphy's numbers (a MacBook, two updates per transaction): 44,096
  transactions a second unbatched, 121,922 batched with a savepoint each,
  102,545 with concurrent reads. His workload is smaller than a real command;
  the shape carries over, the figures do not.

## Streams

Per open stream, a synthetic 57 KB frame sent 200 times to 20 streams, one
i5-13500 core, 2026-10-12:

| Encoder | Heap per stream | CPU per frame | Ratio |
| --- | --- | --- | --- |
| brotli q5, default window (datastar-go `WithBrotli` without `WithBrotliLGWin`) | 9.9 MiB | 267 us | 126:1 |
| brotli q5, 2^18 window | 2.4 MiB | 250 us | 124:1 |
| zstd fastest, default window | 4.5 MiB | 112 us | 122:1 |
| zstd fastest, 2^19 window (lith's default, hyperlith's setting) | 1.0 MiB | 36 us | 123:1 |

- Plan memory per open tab, not per request: 1,000 tabs at the default brotli
  window hold about 9.7 GiB, at zstd 2^19 about 1.0 GiB.
- The window must be larger than one frame, or a frame cannot refer back to
  the previous one. Measure the largest frame of each page.
- Once renders are shared, compression is the main CPU cost: in kanban's
  200-tab run most of 4.9 cores went into brotli. Safari decodes zstd from 26.3
  (on OS 26.3 and later); older browsers get brotli.
- Never hold a lock or a slot while writing to the network; give every write a
  deadline.

## Render loop

- The tick (50 ms by default) bounds the work: N commits in one tick cost one
  render per changed view, whatever N is. A slower tick saves CPU; a faster one
  lowers latency. Measure with the overrun log before changing it.
- Each render worker opens one read transaction per tick for all the views it
  renders: one WAL check per worker per tick instead of one per query.
- Shared views render once for every viewer. Put per-user parts in a small
  separate view or a signal, never in the shared one.
- When a tick overruns, the loop logs n, p50, p95, p99 and max over the next
  20 seconds. A steady p99 above the tick means the work per tick is too much.

## Measuring

- In process first: `lith/loadtest` drives the writer and the render loop
  without HTTP, so the load tool takes no CPU from the server and there is no
  back pressure from a slow client.
- End to end: kanban's `cmd/netem` adds a round trip and bandwidth limits to
  open streams too (Chrome's emulation does not), and `cmd/loadtest` simulates
  tabs. Run the load tool on other cores (`taskset`) or another machine.
- Record results in `benchmarks/RESULTS.md` with date, machine, settings and
  the command that produced them.
