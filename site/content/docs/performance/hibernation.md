---
title: Hibernation
headline: Hibernation: idle SSE streams give back their encoder
summary: An hour of 200 tabs, simulated second by second: what an idle zstd stream saves when it gives its encoder back, what its next frame costs, and what browsers make of it.
description: An hour of 200 tabs, simulated second by second: what an idle zstd stream saves by giving its encoder back, and what its next frame costs.
order: 5
---

Most open tabs sit still. With `live.Options.Hibernate` set, a zstd stream that sent no frame for that long ends its zstd frame and gives its encoder back to a pool, and its window back to the budget; its next frame takes an encoder from the pool, with the window the budget then allows, and starts without the history of the frames before. A budget with `Refuse` keeps the smallest window that holds the stream's frame booked for it while it sleeps, so it wakes with at least that window. Heartbeats go out without waking it. It is off unless an app sets it.

## An hour of 200 tabs

`TestHibernation` (`hibernate.csv`) simulates an hour of 200 tabs, second by second, with [the page workload's](/docs/performance/budget) frames: each person changes something every three minutes on average, a modal and then, a second later, the list it changed, and the hub sends a heartbeat after 20 s of silence. The heap per tab is the mean of samples every five minutes after the first ten, each after a collection, which also empties the pool of encoders. Measured on 2026-10-13 on the development machine; a run with lith v0.2.0 on 2026-10-14 gave the same bytes, and the same heap within 0.001 MiB a tab.

```chart
kind: bars
title: Heap per tab, by how long a stream waits before it hibernates
x: hibernate after
y: MiB per tab
source: bench/budget/hibernate.csv
---
zstd 2^17 low memory,never,0.640
zstd 2^17 low memory,120 s,0.308
zstd 2^17 low memory,30 s,0.106
zstd 2^19,never,1.647
zstd 2^19,120 s,0.787
zstd 2^19,30 s,0.268
```

| Setting | Hibernate after | Heap per tab | Tabs awake | Bytes per frame | Heartbeats per tab and hour |
| --- | --- | --- | --- | --- | --- |
| zstd 2^17, low memory | never | 0.640 MiB | 100% | 433 | 1,010 B |
| | 30 s | 0.106 MiB | 16% | 1,533 | 2,412 B |
| | 120 s | 0.308 MiB | 48% | 1,103 | 1,798 B |
| zstd 2^19 | never | 1.647 MiB | 100% | 277 | 1,010 B |
| | 30 s | 0.268 MiB | 16% | 1,532 | 2,412 B |
| | 120 s | 0.787 MiB | 48% | 1,093 | 1,798 B |

## What the next frame costs

After 30 s, a sixth of the encoders stay, and a frame takes about 1.1 KB more: 41% of the frames start a zstd frame without history, 1,947 bytes on average, and the list after a modal finds only the modal in its frame. That is about 45 KB more per tab and hour. A heartbeat of a hibernating stream goes out in a zstd frame of its own, 15 bytes instead of 6.

Waking takes 36 to 62 µs for the page's 37 KB list with an encoder from the pool, and 101 to 117 µs with a new one; 124 µs and 303 to 467 µs for the board's frame (`TestWakeCost`). The list then takes 3,790 bytes on the wire, the board 10,930.

## What browsers decode

Headless Chromium 154 (`TestChromiumDecodesFrames` in `sse`) decodes a zstd body of several frames event by event, each as soon as it is sent: frames that hibernation ended, raw frames while a budget had no room, frames from pooled encoders. A brotli body cannot do the same: given one that holds a second brotli stream after the first, Chromium decodes the first and drops the second without an error. So only zstd streams hibernate.

## Choosing a time

A hibernating tab trades heap for bytes: the 30 s setting cuts the heap to a sixth and adds 45 KB an hour per tab. Where tabs stay open for hours and change rarely, a public site, a dashboard on a wall, that trade is cheap; this site hibernates after a minute. Where every open tab changes every few seconds, hibernation rarely starts, and costs nothing either.

## Measuring again

```sh
LITH_BUDGET=1 taskset -c 0-11 go test ./bench/budget -run 'Hibernation|WakeCost' -v -timeout 1h
go test ./sse -run TestChromiumDecodesFrames -v
```

The Chromium test finds Chromium on the `PATH`, or in `CHROME`; on a machine without a sandbox for it, set `LITH_CHROME_NO_SANDBOX=1`.
