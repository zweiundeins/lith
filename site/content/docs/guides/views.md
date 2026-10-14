---
title: Views and the hub
nav: Views
headline: Views and the hub: keys, topics and live streams
summary: A view is a region of a page with a key, topics and a render function. Define the board's views, a tab's and a session's, add one to an open stream, tailor frames per stream, and keep the streams alive.
description: A view is a region of a page with a key, topics and a render function: shared views, a tab's and a session's, views added to open streams, heartbeats.
order: 4
---

The hub, `live.Hub`, renders views and streams their frames. A commit marks the views of its topics dirty, and in the next tick a worker renders each dirty view once, in one read transaction, for every stream that watches it ([Ticks](/docs/concepts/ticks) explains the loop). This page builds the board's views.

## A shared view

A `live.View` has a key, the topics that mark it dirty, and a render function:

```go source=site/internal/docs/example/web/views.go
// boardView is the board, one frame for everyone who watches it: its key
// and its topic are the board's, and nothing in it depends on the viewer.
func boardView(id int64) live.View {
	key := board.Topic(id)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx) // the worker's prepared statements, in this render's transaction
			b, err := board.Load(ctx, q, id)
			if err != nil {
				return nil, err
			}
			return renderBoard(ctx, b)
		},
	}
}
```

The key says who shares the view: every stream that watches `board:1` watches one view, rendered once per tick. While anyone watches it, the definition it was first given is the one used, so two definitions of one key must render the same thing. Nothing about the viewer goes into a shared view: no name, no "you", no pending state, no outcome of the viewer's last action. Those go into a view of the tab, or into signals.

A frame is usually `[]byte`: one or more Datastar events, formatted once by `sse.PatchElements` or `sse.PatchSignals`, and the same bytes go to every stream. A frame of another type needs a `Spec.Write` or `Spec.WriteTick` that turns it into events per stream ([below](#tailoring-frames-per-stream)). Once returned, a frame must not change.

When `Render` fails, the hub logs the error and the previous frame stays. A nil frame keeps the previous one as well, and so does a `[]byte` frame equal to it: the streams send nothing. A panic in `Render` is recovered and logged like an error. `Options.RenderTimeout`, 2 s by default, ends the render's context and logs the render; nothing can stop a render that ignores its context, and the tick waits for it.

## Topics

A topic names a part of the state that commands change: `board:1`, `card:7`, `tab:<id>`, `sid:<id>`. A command touches the topics of what it changed, and a view lists the topics of what it shows. The writer calls `hub.Changed(topics)` after each commit, through `writer.Options.OnCommit`.

- Give each thing a page shows a topic of its own. A view that listens to a topic every commit touches renders on every commit.
- A command that touches nothing leaves every page as it was, however much it changed.
- `hub.MarkAll()` marks every view dirty, after a change no topic tracks: an `Exclusive` import, a migration at runtime.
- `hub.Stats().RendersByKey` counts the renders of each view: a view that renders more often than its topics change listens to too much.

## Views of a tab and of a session

What differs per viewer goes into a small view keyed by the tab, or by the session. The tab's view renders the tab's row: why its last action changed nothing, and the op id that ends the page's pending state, as signals:

```go source=site/internal/docs/example/web/views.go
func tabView(sid, tab string) live.View {
	key := board.TabTopic(tab)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			s, err := board.Tab(ctx, q, sid, tab)
			if err != nil {
				return nil, err
			}
```

A preference applies in every tab of a session, so its view is keyed by the session and shared by those tabs:

```go source=site/internal/docs/example/web/views.go
// prefsView patches the session's preferences into every tab of the
// session: one view per session, which SetCompact's topic marks dirty.
func prefsView(sid string) live.View {
	key := board.SessionTopic(sid)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			p, err := board.SessionPrefs(ctx, q, sid)
			if err != nil {
				return nil, err
			}
			signals, err := json.Marshal(p)
			if err != nil {
				return nil, err
			}
			return sse.PatchSignals(signals, false), nil
		},
	}
}
```

The rows behind both views are written by commands, so they survive a deploy ([Pages and Datastar](/docs/guides/pages#tab-state-in-sqlite)).

## Opening a stream

The stream's handler builds the tab's views and hands them to `hub.Serve`, which streams until the client leaves or the hub stops:

```go source=site/internal/docs/example/web/server.go
views := []live.View{boardView(id), presenceView(s.hub, id), tabView(sid, tab), prefsView(sid)}
if state.Panel != 0 {
	views = append(views, cardView(state.Panel))
}
err = s.hub.Serve(w, r, live.Spec{ID: tab, Views: views, WriteTick: tailor(), Reconnect: StreamID})
```

`Serve` sends each view's current frame at once, or its first one when it has none yet, then every new frame. A stream keeps only the newest frame of each view, so a slow client skips frames and holds up no one else. Views that change in the same tick reach the stream together, in the order of `Spec.Views`.

`live.Spec` describes the stream:

| Field | What it does |
| --- | --- |
| `ID` | names the stream for `Watch` and `Unwatch`, usually the tab id. A new stream with the ID of an open one replaces it; an older request that arrives late ends at once |
| `Views` | the views the stream watches from the start |
| `Write`, `WriteTick` | tailor frames per stream ([below](#tailoring-frames-per-stream)) |
| `Reconnect` | the id of the element whose `data-init` opened the stream: when the hub stops, the stream's last event makes the page open it again ([Pages and Datastar](/docs/guides/pages#reconnecting-after-a-deploy)) |

## Who is watching

A view with `Presence: true` is marked dirty whenever a stream starts or stops watching it, and `hub.Count(key)` says how many do. It needs no topic:

```go source=site/internal/docs/example/web/views.go
func presenceView(hub *live.Hub, id int64) live.View {
	key := "presence:" + board.Topic(id)
	return live.View{
		Key:      key,
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			return patch(ctx, Presence(hub.Count(key)))
		},
	}
}
```

## Views that change with time

`Every` renders a view again after that long without changes, for a clock or numbers that move without a commit:

```go source=site/internal/docs/example/web/hub.go
func statsView(w *writer.Writer, hub *live.Hub) live.View {
	return live.View{
		Key:   "stats",
		Every: time.Second,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			return patch(ctx, Stats(w.Stats(), hub.Stats(), hub.Budget().Stats()))
		},
	}
}
```

## Adding a view to an open stream

A tab opens a card's panel, and its stream should start rendering that card. `hub.Watch(tab, view)` adds a view to the open stream with that ID, and `hub.Unwatch(tab, key)` takes one away. The panel's handler records the open panel in the tab's row first, so a stream that reconnects after a deploy watches the card from its start:

```go source=site/internal/docs/example/web/server.go
old, err := s.db.ViewValue(ctx, func(tx *sql.Tx) (board.TabState, error) { return board.Tab(ctx, tx, sid, a.Tab) })
if err == nil {
	err = s.w.Do(ctx, board.OpenPanel{Session: sid, Tab: a.Tab, Card: a.Card, At: time.Now()})
}
if err != nil {
	s.fail(w, r, err)
	return
}
if old.Panel != 0 && old.Panel != a.Card {
	s.hub.Unwatch(a.Tab, board.CardTopic(old.Panel))
}
if a.Card != 0 {
	s.hub.Watch(a.Tab, cardView(a.Card))
}
```

A command can arrive before its stream: a page that opens a panel while it loads. `Watch` then keeps the view for `Options.PendingWatch`, 10 s by default, and the stream that opens with that ID within that time watches it from its start, after the views of its `Spec`. `Watch` reports whether the stream was open, so there is no retry loop to write.

## Tailoring frames per stream

By default, a stream writes the new `[]byte` frames of its views together, one compressed write per round. Two `Spec` fields let a stream decide what to send:

- `Write(s, key, frame)` gets each new frame of a view on its own, and each call is a write of its own.
- `WriteTick(s, frames)` gets all of the stream's new frames at once, in the order the stream watches the views: one call, one `s.Write`, one compression and one flush.

The board's frame is not `[]byte`: it holds each lane as an event of its own, with a hash of its HTML, and the whole board. Its stream sends only the lanes it has not seen, and the whole board when lanes came or went:

```go source=site/internal/docs/example/web/views.go
func tailor() func(*sse.Stream, []live.Frame) error {
	var shape string
	sent := map[int64]uint64{}
	var events [][]byte
	return func(s *sse.Stream, frames []live.Frame) error {
		events = events[:0]
		for _, f := range frames {
			switch v := f.Value.(type) {
			case []byte:
				events = append(events, v)
			case boardFrame:
				if v.Shape != shape {
					shape = v.Shape
					clear(sent)
					events = append(events, v.Whole)
					for _, l := range v.Lanes {
						sent[l.ID] = l.Hash
					}
					continue
				}
				for _, l := range v.Lanes {
					if sent[l.ID] != l.Hash {
						sent[l.ID] = l.Hash
						events = append(events, l.Event)
					}
				}
			}
		}
		if len(events) == 0 {
			return nil
		}
		return s.Write(events...)
	}
}
```

The render formats every event once, and the tailor only picks among them: no stream formats anything. The stream handler makes a new tailor for each stream, since it remembers what its stream sent. `frames` is valid during the call only.

A stream that sends only what changed costs less CPU to compress and less work for the browser's morph; the compressor alone already sends an unchanged lane in a few bytes, since it repeats the frame before. [Load testing](/docs/guides/load-testing) runs this board, tailor and all, with 200 tabs.

`live.Cache` keeps rendered fragments by a key that stands for their content, such as a card's id and version, so a render copies the cards that did not change instead of rendering them again. It is bounded in bytes and admits a fragment by how often its key comes back.

## The heartbeat and the page's watchdog

A stream that has been silent for `Options.Heartbeat`, 20 s by default, sends a comment, so proxies keep the connection open. The page never sees a comment. With `Options.HeartbeatSignal` set, the heartbeat patches that signal instead, every 20 s, frames or not, with a number that grows:

```go source=site/internal/docs/example/web/hub.go
HeartbeatSignal: "_ping",     // the page's watchdog reads it (expr.go)
```

The page notes each heartbeat, and opens the stream again when none came for 50 s, as after a laptop's sleep:

```go source=site/internal/docs/example/web/expr.go
// beat notes a sign of life: the hub patches $_ping every 20 s
// (live.Options.HeartbeatSignal), frames or not.
const beat = `$_ping, $_beat = Date.now()`

// watchdog opens the stream again when no heartbeat came for 50 s: a
// connection that went quiet without closing, as a laptop's after sleep.
func watchdog(b int64) string {
	return "Date.now() - $_beat > 50000 && ($_beat = Date.now(), " + openStream(b) + ")"
}
```

## How many streams

`Options.MaxStreams`, 2000 by default, caps the open streams: `Serve` answers 503 to the next one and returns `live.ErrFull`. The template sets 200 and this site 300, from their memory: each compressed stream holds its encoder's window for as long as it is open ([Streams and compression](/docs/guides/streams#the-memory-budget)). A stream refused for want of memory gets a 503 too, and a page that opened its stream with `retry: 'error'` asks again. An app whose budget refuses streams (`sse.BudgetOptions.Refuse`) can let memory be the cap: the budget books what each stream holds, its encoder, 32 KiB and its views' frames, and turns away the stream it has no room for, compressed or not. `MaxStreams` then stays at its default as a backstop for a process without a memory limit ([Refusing streams](/docs/guides/streams#refusing-streams)).

## The hub's options

| `live.Options` | Default | What it does |
| --- | --- | --- |
| `Read` | none | the read pool; each worker holds a connection for the hub's life |
| `Tick` | 50 ms | the shortest time from one tick to the next |
| `Workers` | GOMAXPROCS | render workers |
| `MaxStreams` | 2000 | open streams at most |
| `PendingWatch` | 10 s | how long `Watch` keeps a view for a stream not open yet |
| `Heartbeat`, `HeartbeatSignal` | 20 s, none | the silence before a heartbeat, and the signal it patches |
| `Hibernate` | never | how long a stream may stay idle before its encoder goes back ([Streams](/docs/guides/streams#hibernation)) |
| `RenderTimeout` | 2 s | how long a render may take before its context ends |
| `OnTick` | none | gets each tick's duration |
| `SSE` | | the streams' options and their memory budget ([Streams](/docs/guides/streams)) |

The board's settings:

```go source=site/internal/docs/example/web/hub.go
return live.Options{
	Read:            db.R,
	MaxStreams:      500,
	HeartbeatSignal: "_ping",     // the page's watchdog reads it (expr.go)
	Hibernate:       time.Minute, // an idle tab gives its zstd encoder back
	Log:             log,
	SSE:             sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{Refuse: true})},
}
```

`hub.Stats()` counts ticks, renders, frames sent and dropped, overruns, open streams, watched views and the workers' read transactions. When a tick takes longer than `Tick`, the hub samples every tick for 20 s and logs their count, p50, p95, p99 and maximum ([Metrics and monitoring](/docs/guides/monitoring)).
