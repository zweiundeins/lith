## The report

The numbers ride on your stream itself. The report view renders a probe, one value for every tab, and the site's frame writer keeps your stream's probe from then on. Each time it writes a tick's frames, it first asks the probe for a report, which reads the stream at that moment:

```go title=report.go
func read(st *sse.Stream, m *demo.Meter, now time.Time) Reading {
	r := Reading{Frames: m.Frames, Coding: st.Encoding(), Last: m.Last, FPS: m.Rate(now, 4*time.Second), Beats: m.Beats}
	r.Tier, r.Heap = st.Tier()
	r.Events, r.Wire = st.Sent()
	if b := st.Budget(); b != nil {
		r.Budget = b.Stats()
	}
	return r
}
```

`demo.Meter` is the frame writer's count of the stream's writes: the bytes of each, when they went out, and what the hub sent in between. The report goes out as a patch of `$_stream` in the same write as the frames, so it describes the stream up to the frame before. On this page every frame carries one; in the landing page's slot, one a second at most.

## The coding

`sse.Negotiate` picks the coding with the highest weight in your browser's `Accept-Encoding`, zstd before brotli before gzip when they tie. Chrome, Edge and Firefox accept zstd, Safari from 26.3. zstd sends more bytes than brotli for a sixth of its CPU or less: on one core, a 57 KB page with five rows changed per frame took zstd 36 to 41 µs a frame for 405 bytes on the wire, and brotli at quality 4 251 to 265 µs for 121 bytes. The coding reads none when your browser accepted none, or when the budget had no room for a brotli or gzip encoder.

## The setting from the budget

The hub shares a memory budget among its streams' encoders: half the process's memory limit. This site runs with `GOMEMLIMIT=192MiB`, so the encoders share 96 MiB. A stream gets the largest setting that fits both its fair share, the limit divided by the open streams, and what is left of the limit:

| Setting | Window | Reserved |
| --- | --- | --- |
| `zstd 2^19` | 512 KiB, with twice that as history | 1.66 MiB |
| `zstd 2^19 low memory` | 512 KiB | 1.08 MiB |
| `zstd 2^18 low memory` | 256 KiB | 848 KiB |
| `br q4 2^18` | 256 KiB | 2.14 MiB |

The window is how far back the encoder looks for what a frame repeats. Low memory keeps zstd's history at the window, for more CPU per frame. When a new stream finds no room, every zstd stream above the new fair share moves down at its next write, so your setting can change while you watch; when not even 2^18 fits, a stream goes out uncompressed. `zstd asleep` is a zstd stream that holds no encoder: after a hibernation, or while the budget has no room for it, when its frames go out in raw blocks.

## Reserved for your encoder

The heap that the budget reserves for your setting: what lith measured an encoder of that setting to hold after a session of 142 KB frames, rounded up. The budget adds up these reservations; it does not measure the heap.

## Events and the wire

Events are the Datastar events as the server formats them: `datastar-patch-elements` with your table's HTML and `datastar-patch-signals` with these numbers in JSON. The wire is what your encoder made of them, the bytes the server wrote, before TLS and HTTP framing. The ratio so far divides the one by the other, from the stream's first byte.

## The last frame

A frame here is one write: the views that changed in a tick, compressed and flushed together. Its ratio tells how many bytes of events went into each byte on the wire. The large table's frame carries about 28 KB of events; with calm churn eight of its 1,536 cells change, and the frame takes a few hundred bytes. With storm a quarter of the cells change, and the ratio falls to about 7:1.

The window must hold the frame before, or there is nothing to refer back to. In lith's measurements a board of 142 KB a frame takes 1,471 bytes on the wire with a 2^19 window, 2,703 at 2^18 with low memory, and 10,831 at 2^17, where the frame before is out of reach. A stream's first frame has no history at all, and neither has the first frame after a hibernation: those are the spikes in the line.

## Frames a second

The table view renders every 250 ms, and a little more than that apart in practice: each render is due 250 ms after the tick that ran the one before. The rate counts the writes of the last four seconds.

## Heartbeats

After 20 s without a frame, the hub sends an SSE comment, three bytes of events, so that proxies keep the connection open. The page never sees a comment; the meter finds the heartbeats as the bytes the stream sent between two of its writes. While the table moves there are none. Pick Still, wait half a minute and pick Calm again: the next report counts them.

## Hibernation

After a minute without a frame, the hub hibernates the stream (`live.Options.Hibernate`): the stream ends its zstd frame, gives its encoder to a pool and its reservation back to the budget, and its setting reads `zstd asleep`. A heartbeat then goes out in a zstd frame of its own, 15 bytes instead of 6. The next frame takes an encoder from the pool and starts without history: lith measured a 37 KB list at 3,790 bytes on the wire after waking.

## The site's streams and the budget

Every stream of the hub opens through the budget, which counts them by what they hold: that gives the number of open streams and the list under the meter. The meter is the reserved heap against the limit. With 200 open streams this site measured 110 MB resident, 100 MB of it the encoders' budget, with 115 streams at zstd 2^18 with low memory and the rest uncompressed.

## The payload

Your choice of size and churn is your tab's row in `site_tabs`, written by a command. Still freezes the table at the step it was chosen:

```go title=commands.go
func (c SetPayload) Apply(tx *writer.Tx) error {
	var s TabState
	if err := demo.LoadTab(tx.Context(), tx, Slug, c.Viewer, &s); err != nil {
		return err
	}
	if c.Size != "" {
		s.Size = c.Size
	}
	if c.Churn == Still && s.churn() != Still {
		s.Frozen, s.From = StepAt(c.At), s.churn()
	}
	if c.Churn != "" {
		s.Churn = c.Churn
	}
	s.Op = c.Op
	return demo.SaveTab(tx, Slug, c.Viewer, s, c.At)
}
```

The table at a step is a function of its size, its churn and the step: the cells change in an order that visits every cell before it comes round again, a fixed number of them per step. So the tabs that chose the same share one render of each step, and your tab's view only reads your row and picks that render.

## What the inspector costs the server

Measured on 2026-10-13 on a development machine, an Intel i5-13500, not on the VPS that serves this site, with the site's binary on four cores and `GOMEMLIMIT=192MiB` as deployed: 100 inspectors with the large table, read by a load test on other cores that does not decompress them, over 60 s each:

| Churn | CPU | Bytes a frame | Heap after a collection |
| --- | --- | --- | --- |
| Calm | 0.08 cores | 503 | 87 MiB |
| Busy | 0.09 cores | 1,619 | 87 MiB |
| Storm | 0.16 cores | 4,413 | 89 MiB |

The budget gave all 100 streams `zstd 2^18 low memory`, 82.8 of its 96 MiB, which is most of that heap. The CPU goes into compressing about 400 frames a second; the table renders once a step for all the tabs that share it. Between collections the heap grows to the goal that the memory limit sets, so the process stayed at 190 to 197 MiB resident, below the unit's `MemoryHigh=224M`.
