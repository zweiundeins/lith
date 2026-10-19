---
title: Ticks
headline: Ticks: one render a tick for every viewer
summary: Commits mark views dirty, and once per tick each dirty view renders once, for every stream that watches it.
description: Commits touch topics, topics mark views dirty, and once per tick each dirty view renders once, in one read transaction, for every stream that watches it.
order: 3
---

The hub decides when to render, not the request that changed something. A commit marks views dirty, and the hub renders dirty views in ticks: at most one tick per `Options.Tick`, 50 ms by default. However many commits land in one tick, each changed view renders once.

## Topics

A command names what it changed with `tx.Touch("board:42")`. After the batch commits, the writer passes the batch's topics to `OnCommit`, which is `hub.Changed`, and the hub marks every view that lists one of them in its `Topics` dirty. Topics are paths: a commit to `board:42` counts for `board` as well, and marks a view that lists `board` ([A topic hierarchy](/docs/guides/views#a-topic-hierarchy)).

A view's key says who shares it: every stream that watches `board:42` watches the same view, and while it is watched, the definition it was first given is the one used.

```go
live.View{
	Key:    "board:42",
	Topics: []string{"board:42"},
	Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		return renderBoard(ctx, tx, 42)
	},
}
```

A command that touches nothing leaves every page stale. A view that listens to a topic every commit touches renders on every commit.

## The tick

The hub's loop sleeps while nothing is dirty and starts a tick at once after a quiet spell. In a tick, the dirty views go onto one queue that the render workers drain. Each worker holds a connection of the read pool for the hub's life and renders all its views of the tick in one read transaction, on statements it prepared once. When every worker is done, the new frames go out together.

In lith's load test, 200 streams watch one board of 139 KB while 800 commands a second change it: the board renders once per tick, 19.8 ticks a second, and no stream misses a frame.

## The newest frame wins

A stream keeps only the newest frame of each view it watches. A slow client skips frames and holds up no one else; when it catches up, it gets the current state, which is all a page needs. Views that change in the same tick reach a stream together, in the order the stream watches them.

A view does not render for streams that have not sent its last frame: while every stream that watches it still has a frame of it waiting, the view waits too, and the first stream that takes its frame has it rendered in the next tick, as it is then. A client whose writes block costs the server no renders, and a view that one stream keeps up with renders for all of them. A stream paced by its page's acknowledgements (`Spec.Window`) has its frames wait the same way while its window is full ([Streams: pacing a stream by the page](/docs/guides/streams#pacing-a-stream-by-the-page)).

## More than topics

- `Every: time.Second` renders a view again after that long without changes, for a clock or live numbers.
- `Presence: true` marks the view dirty when a stream starts or stops watching it, and `hub.Count(key)` says how many do.
- `hub.Watch(tab, view)` adds a view to an open stream, such as a panel that scrolls into sight. A command that arrives before its stream does is kept for `Options.PendingWatch`, 10 s.
- `Spec.WriteTick` gets all of a stream's new frames of a tick at once, for one compressed write and one flush.

## Measuring

`hub.Stats()` counts ticks, renders, frames sent and dropped, the renders it put off for streams that had a frame waiting, and the renders of each view by its key: a view that renders more often than its topics change listens to too much. When a tick takes longer than the tick, the loop logs the overruns over the next 20 seconds, with their p50, p95, p99 and maximum.
