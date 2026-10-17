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
  writer does this for every `tx.Exec` and keeps the 512 statements used
  last; build SQL from constants, and send SQL built at runtime through
  `tx.ExecUncached` and its siblings, so the cache keeps what repeats.
- `synchronous=NORMAL` is lith's default. Beware: it syncs the WAL only at
  checkpoints (with CheckpointWriter after a second without commands, and
  every 1000 pages), so a power loss or kernel crash can undo the commits
  since, up to about 35 s of them with Linux's default writeback
  (`vm.dirty_expire_centisecs` 3000, flushed every 5 s). A crash of the
  process loses none. `FULL` costs one fsync per batch, the latency floor of a
  lone write (1.9 ms on the bench host); set it where an acknowledged commit
  must survive a power loss.
- `events.Execute` with one inline projector (i5-13500, 2026-10-16, NORMAL,
  a busy host): 6,675 events a second for one producer and 18,342 for 200,
  against 24,404 and 81,087 transfers of `BenchmarkWriter` in the same hour.
  An event writes six B-trees (its row, two indexes, `sqlite_sequence`, the
  state row, the projection), so plan on about four times a plain command's
  cost (`bench/RESULTS.md`).
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
| zstd fastest, 2^19 window, history at the window (hyperlith's setting, lith's `ZstdLowMemory`) | 1.0 MiB | 36 us | 123:1 |

- Plan memory per open tab, not per request: 1,000 tabs at the default brotli
  window hold about 9.7 GiB, at zstd 2^19 about 1.0 GiB with `ZstdLowMemory`
  and 1.6 GiB with lith's default, which keeps twice the window as history.
- The window must hold the frame, or a frame cannot refer back to the
  previous one (at 2^18: a 259 KB frame 249 bytes on the wire, a 266 KB one
  29,719). With a budget that has a limit, each stream's first write picks a
  window that holds its largest write with a sixteenth to spare. Without one,
  measure the largest frame of each page and size the window to it. A page
  that sends one view after another on one stream refers back further than a
  frame: a business app's frames (2 to 37 KB) cost 9% more bytes at 2^17 than at
  2^19 and 19% more at 2^16; set `MinWindow` for such pages. kanban's 139 KB
  board needs 2^18. (datastar-go's brotli q3 held 8.5 MiB per tab of that
  app after the first render.)
- Once renders are shared, compression is the main CPU cost: in kanban's
  200-tab run most of 4.9 cores went into brotli. Safari decodes zstd from 26.3
  (on OS 26.3 and later); older browsers get brotli.
- A live hub shares a memory budget among its streams by default: half the
  process's memory limit (GOMEMLIMIT, else the cgroup's), for each stream's
  encoder, 32 KiB per stream besides (hub state, stacks, net/http buffers) and
  the frames the hub keeps (its length + 8 KiB, once per view; a non-[]byte
  frame by its `Len() int`). The hub keeps the newest frame of a view that two
  or more streams watch, and any non-[]byte frame; of a []byte view one stream
  watches (a tab's own) it keeps only the length and a hash once the stream
  has the frame, and a stream that joins it gets a render in the next tick (a
  join without the frame: 1.2 ms and 1 MiB of garbage for a 233 KB table,
  against 69 us with it). The other half is GC headroom: a 230 KB render
  allocates about 1 MiB of garbage. zstd takes the largest tier within its
  fair share and shrinks while open, down to its frame's window, then 2^15,
  then raw blocks (with `Refuse`, never below its frame's window). brotli
  keeps its tier: its frame's window (no larger) at q4 while the budget is at
  most half full, q3 after, q3 2^16 only when that does not fit (never with
  `Refuse`), uncompressed last. Without a memory limit nothing changes.
  Configure it with
  `live.Options{SSE: sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{...})}}`:
  `Limit` in bytes (-1: none), `MinWindow` for frames that refer back further
  than one write, `MaxWindow`, and `Refuse` to answer 503 instead of sending
  uncompressed or with a window below the frame, for pages with large frames
  (kanban's board). With `Refuse` a stream gets in only with room for 32 KiB
  and the smallest tier whose window holds its frame (`sse.Options.Frame`:
  `hub.Serve` opens a stream once its first frames are rendered and passes
  their length, so its headers go out with the first frame, up to a tick
  later), an uncompressed one with room for its 32 KiB. Every stream let in
  keeps such a window until it closes: zstd keeps it booked while asleep and
  moves no lower, brotli takes no smaller window, and when a stream is
  turned away the zstd streams above their frame's window move down to it.
  Frames that later outgrow the window cost more bytes, never the stream. In
  120 MiB, 134 zstd tabs with a 233 KB view each get in at 2^18, or 91
  brotli tabs; the rest get 503. Memory is then the stream cap: leave
  `MaxStreams` at its default (2000) as a backstop for a process without a
  memory limit, and open streams with retry 'error'. A stream's own
  ZstdWindow and BrotliWindow cap what it gets. A brotli stream below its
  frame's window (or uncompressed) with `Spec.Reconnect` set is ended with
  `sse.Reconnect` once room frees up, and the page reopens on a better tier
  (one stream a second, a tab once a minute; `sse.Stream.Upgrade` outside a
  hub). `hub.Budget().Stats()` tells what the streams hold (`Frames`,
  `Reopened`); `SetLimit` moves the limit at runtime; `Budget.Hold` books
  frames kept outside a hub. 150 tabs with a 230 KB view each need about 200
  MiB of budget with brotli, 140 MiB with zstd. Giving an idle stream's
  encoder back is opt-in: `live.Options.Hibernate` (zero, the default, never
  does). bench/budget has the numbers.
- Never hold a lock or a slot while writing to the network; give every write a
  deadline.

`sse.Options.ZstdLevel` trades bytes on the wire for CPU and heap. lith's
200-tab board in process (`BenchmarkBoardZstdLevels` in `loadtest`, 140 KB of
HTML, about 4,000 frames a second, i5-13500, 2026-10-14; brotli from
`BenchmarkSharedBoard` the same day), medians of three runs:

| Encoder | Bytes per frame | Cores busy | Heap per stream |
| --- | --- | --- | --- |
| zstd fastest (lith's default) | 2,213 | 1.84 | 1.66 MiB |
| zstd default | 1,087 | 1.74 | 2.76 MiB |
| zstd better compression | 365 | 3.15 | 5.56 MiB |
| brotli quality 4 | 854 | 3.42 | 1.56 MiB |

- zstd's default level halves the bytes of the fastest for no more CPU, and
  holds 1.1 MiB more per stream: about 220 MiB more for 200 tabs.
- Better compression sends fewer bytes than brotli for about brotli's CPU, at
  5.6 MiB a stream.
- `sse.Options.ZstdLowMemory` keeps zstd's history at its window: 1.0 MiB a
  stream instead of 1.65 MiB, for 40% more CPU per frame with 20 streams
  (`TestHeapPerStream`, `bench/RESULTS.md`).

Slow links: the page measures once into `$_slow` (navigator.connection's
saveData or 2g/3g; where it is missing, Navigation Timing: first byte over
300 ms after the request, or under 100 KB/s), and its stream's request
sends `Save-Data: on` while `$_slow` is true, at the same URL. Documents
send `Accept-CH: ECT, RTT, Downlink` (`edge.NetworkHints`);
`edge.SlowLink(r)` reads `Save-Data: on`, those hints (ECT 2g/3g, RTT over
300, Downlink under 0.8) and net=slow (pages from before Save-Data), and
the handler passes it as `live.Spec.Slow`. A slow stream gets each view's
newest frame 250 ms apart, about 4 a second (a view's first frame, or one
of a view quiet for 250 ms, goes at once and takes the waiting frames
along; heartbeats unchanged). On lith's site, a Game of Life tab on a phone
(CPU 4x, Slow 4G or 3G) went from 89 to 90% of its main thread busy to 54
to 56%, its stream from 5.2 to 6.2 KB/s to 2.7 to 2.9; the network was
never the limit. Back-pressure does not tell a phone too slow for the page
on a fast link: at CPU 15x, unthrottled, behind Caddy (HTTP/2) or not, the
tab applied about 7 of 10 frames a second with no frame dropped and no
write waiting (Chromium grants an HTTP/2 stream 6 MiB; over HTTP/1.1 the
kernel buffers held it). With the corrected `TestLifeDrift`, which takes
the lag from times the page notes (the first version read the page only
when its main thread was free, after 7 s tasks), it fell 27.15 s behind at
the median and 50.0 s in 150 s, painting about every 7 s (2026-10-16,
`bench/drift/RESULTS.md`). The start-up measurement sees the link, not
the CPU; `newest-frame.js` (`live.NewestFrame`, `data-newest-frame__reopen`
on body, vendored from github.com/mbolli/datastar-attribute-newest-frame)
sees the page's own lag: it applies each view's newest frame, waits after
a batch half as long as the batch took, and after dropping over half the
frames for 3 s sets `$_slow` and reopens the stream. The wait caps
applying frames at two thirds of the main thread, not the rendering that
follows: at 15x the main thread stayed 97% busy, half morphing and half
layout and paint, but painted every applied frame. Chromium hands a page
that is behind 2 MiB per read (45 frames in one 7.2 s task, two paints in
20 s); Firefox holds the backlog in its parent process, one frame per
task, and never makes a write wait. With `__reopen`, at CPU 15x behind
Caddy, the tab stayed 0.64 s behind at the median, 1.16 s at most, at 3.20
KB/s; without `__reopen` 0.32 s at 7.09 KB/s (150 s, 3 runs); at 300 ms
extra per frame, Chromium 154 and Firefox 158 switched after 2.8 s and
stayed within 0.8 s (`TestNewestFrameSlowStream`, 2026-10-16). A frame's
cost to the page is mostly its elements and their layout and paint, not
its bytes or the morph alone: at 15x the site's board costs about 440 ms
a rendered frame, 130 of it the morph; 4,000 bare `<p>` halve the HTML
but are already behind at 12x (the site's at 10x); one SVG with a path per
colour, 34 elements, keeps up to 120x; a terminal of a span per character
breaks at 15x, with merged colour runs at 50x. Frames sent as signal
patches (a canvas) are out of the plugin's reach: only `__reopen` or
pacing help (`bench/frame-cost/RESULTS.md`, 2026-10-17).

Pacing by the page (streams guide, "Pacing a stream by the page"):
`live.Spec.Window` sends at most W writes of frames the page has not
acknowledged; each write ends with the event `Spec.Ack(token, n)` makes
(on lith's site an `<i>` with `data-init` posting `/ack`, patched inner
into the stream's element), and the app passes the post to
`Hub.Ack(id, token, n)`. Frames wait in the stream's slot and renders defer
as for any stream with a frame waiting; `live.AckTimeout` (10 s) sends
anyway. A stream with `Spec.Ack` and no Window starts unpaced;
`Hub.Pace(id, w)` paces it from its next write, which the page asks for
from `data-on:newest-frame-behind` on `<body>` (never on the stream's
element: its `@post` would cancel the stream's `@get`), with the plugin
without `__reopen`. `Spec.Release` unpaces a page whose acknowledgements
all came before the next frame was ready for that long, doubling per
release up to 8x. At W=1 it is long polling over the open stream: one
frame per round trip, newest state only, nothing while the view is still.
Measured (2026-10-17, `bench/hybrid/RESULTS.md`): at CPU 15x paced from
2.2 s in, 0.34 s behind at the median at 2.19 KB/s (`__reopen` 0.62 s,
3.36 KB/s); fast phone at 300 ms RTT never paced, 10.07 frames a second
(ack pacing from the start 2.82); slow for 60 s then fast, released 5 to 6
s later; 1,000 in-process streams with a tenth slow, 250 acks a second and
186% of a core against 9,200 and 452% pacing every stream. `Release`
alone never releases on a round trip of a tick or more; `Spec.ReleaseSlack`
with `Hub.RoundTrip` (the page reports its round trip from Resource
Timing) does, 5 to 6 s after the page is fast at 100 and 300 ms, and
never released a page that stayed slow (`bench/gaps/RESULTS.md`). At 15x
on a 300 ms link a paced page gets 1.45 frames a second, 0.60 s behind,
where the plain plugin stays 0.47 s behind at 7 KB/s. Input at 15x: 118 to
142 ms to the handler at the median with any fix, Event Timing p98 448 to
480 ms; 3 s and 6.4 s without. `/ack` and `/pace` need a session check and
a rate limit.

## Render loop

- The tick (50 ms by default) bounds the work: N commits in one tick cost one
  render per changed view, whatever N is. A slower tick saves CPU; a faster one
  lowers latency. Measure with the overrun log before changing it.
- A dirty view does not render while every stream that watches it still has
  a frame of it waiting (`Stats.RendersDeferred`); the stream that takes its
  frame marks it dirty for the next tick. A stream whose writes block costs
  no renders, a view only slow streams watch renders 4 times a second. 50
  tabs, a 34 KB view each changing every tick: slow streams 1.6 to 1.7 cores
  before, 0.4 after; clients that stopped reading 1.4 to 1.7 before, 0.03 to
  0.05 after; streams that keep up unchanged (`TestRenderCost`,
  bench/RESULTS.md).
- Each render worker opens one read transaction per tick for all the views it
  renders: one WAL check per worker per tick instead of one per query.
- Each render worker holds one read connection for the hub's life and keeps
  its read statements prepared on it. A render that queries through
  `sqlite.QuerierFrom(ctx)` skips modernc's parse and plan, which took 41% of
  the writer's CPU at 200 people until the writer cached its statements.
- Every read connection keeps the 128 statements it ran last
  (`sqlite.Options.Statements`, -1: none, about 4 KiB each), so a GET's
  `db.View` and a Render's tx skip the parse and plan too: from one
  goroutine, kanban's board read takes 10% less time and the guestbook
  template's page 19% less (i5-13500, 2026-10-13, bench/RESULTS.md). SQLite
  still prepares a statement again at every run when a parameter steers its
  plan: a bound `LIMIT ?`, and, once a full `ANALYZE` fills `sqlite_stat4`, a
  parameter compared with an indexed column. That holds for the writer's and
  the workers' statements too: write a fixed LIMIT into the SQL, and leave
  statistics to `PRAGMA optimize`, which the writer runs and which writes
  `sqlite_stat1` only.
- Size the read pool as the hub's workers plus the reads that run at once
  beside it. `sqlite.Options.Readers` defaults to GOMAXPROCS + 4, at least 8,
  which leaves four readers or more beside the default workers.
- `Spec.WriteTick` writes the frames of the views that change in one tick as
  one compressed write, with one flush, where `Spec.Write` makes a write of
  each view.
- Shared views render once for every viewer. Put per-user parts in a small
  separate view or a signal, never in the shared one.
- When a tick overruns, the loop logs n, p50, p95, p99 and max over the next
  20 seconds. A steady p99 above the tick means the work per tick is too much.
- `hub.Stats()` counts the workers' read transactions (`ReadTxs`) and the
  renders of each view watched now (`RendersByKey`), so a test or a load run
  sees both without wrapping the driver.

## Measuring

- In process first: `lith/loadtest` drives the writer and the render loop
  without HTTP, so the load tool takes no CPU from the server and there is no
  back pressure from a slow client.
- End to end: kanban's `cmd/netem` adds a round trip and bandwidth limits to
  open streams too (Chrome's emulation does not), and `cmd/loadtest` simulates
  tabs. Run the load tool on other cores (`taskset`) or another machine.
- Record results in `benchmarks/RESULTS.md` with date, machine, settings and
  the command that produced them.
