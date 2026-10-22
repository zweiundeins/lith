---
title: Views and the hub
nav: Views
headline: Views and the hub: keys, topics and live streams
summary: A view is a region of a page with a key, topics and a render function. Define the board's views, a tab's and a session's, views per person in a shared database, add one to an open stream, tailor frames per stream, and keep the streams alive.
description: A view is a region of a page with a key, topics and a render function: shared views, a tab's, a session's and a person's, views added to streams, heartbeats.
order: 5
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

When `Render` fails, the hub logs the error and the streams send nothing: the page keeps the frame it has. So they do for a nil frame, and for a `[]byte` frame equal to the one before. A panic in `Render` is recovered and logged like an error. `Options.RenderTimeout`, 2 s by default, ends the render's context and logs the render; nothing can stop a render that ignores its context, and the tick waits for it.

## Topics

A topic names a part of the state that commands change: `board:1`, `card:7`, `tab:<id>`, `sid:<id>`. A command touches the topics of what it changed, and a view lists the topics of what it shows. The writer calls `hub.Changed(topics)` after each commit, through `writer.Options.OnCommit`; an app with a store of its own calls it after its own commits ([Wiring](/docs/getting-started/wiring#without-liths-writer)).

Topics are paths: their segments, joined by `:`, go from the general to the particular. A commit to a topic counts for every topic above it, so `board:42` is also `board`, and a view on `board` renders for every board's commit. A commit to `board` reaches no view on `board:42` ([A topic hierarchy](#a-topic-hierarchy)).

- Give each thing a page shows a topic of its own. A view that listens to a topic every commit touches renders on every commit, and so does one on a topic above them all, such as `board`.
- A command that touches nothing leaves every page as it was, however much it changed.
- `hub.MarkAll()` marks every view dirty, after a change no topic tracks: an `Exclusive` import, a migration at runtime.
- `hub.Stats().RendersByKey` counts the renders of each view: a view that renders more often than its topics change listens to too much.

## A topic hierarchy

Name a topic from the general to the particular, one segment per level, joined by `:`: first the kind of data or the system it comes from, then what holds it, then the item. A board is `board:42`, a card on it `board:42:card:7`; a wiki page synced from outside is `confluence:SWE:123`, page 123 of the space SWE. A commit to a topic counts for every level above it: one to `board:42:card:7` touches `board:42` and `board` as well. Each view listens at the level of what it shows:

- a page's view listens to `confluence:SWE:123`, and renders when that page changes;
- a space's list listens to `confluence:SWE`, and renders when any of its pages changes;
- search and the counts per source listen to `confluence`, and render when anything in the source changes.

### State per tab, session and presence gets a root of its own

A view on a topic renders for every commit below it. State that changes per tab, per session or with who is present therefore goes under a root of its own, never under the root of the shared data: `tab:<demo>:<tab>`, not `<demo>:tab:<tab>`.

The site's board demo shows why. Beside the shared board, the page shows who else has it open: a row of avatars, the presence view. Each tab that opens the page sends a hello, a command that records the tab and touches the presence topic; leaving or a new avatar touches it too. That topic was `board:here`, below `board`, which the shared board listens to. So every hello also rendered the board for everyone, and the board's view transition started under a card that someone was dragging, which broke the drag. The presence topic is `here:board` now, a root of its own, and a hello renders only the avatars.

### Everything below a level

A commit to a level does not reach the levels below it: one to `confluence:SWE`, a space's own settings, renders the list and search, and no page. When everything below a level changed, as when a sync rewrote a space, the command touches the level with `:*`, once, instead of each page:

```go
func (c SyncSpace) Apply(tx *writer.Tx) error {
	for _, p := range c.Pages {
		if _, err := tx.Exec(`INSERT INTO pages (source, space, id, title, body) VALUES ('confluence', ?, ?, ?, ?)
			ON CONFLICT (source, space, id) DO UPDATE SET title = excluded.title, body = excluded.body`,
			c.Space, p.ID, p.Title, p.Body); err != nil {
			return err
		}
	}
	tx.Touch(live.Below(live.Topic("confluence", c.Space))) // confluence:SWE:*
	return nil
}
```

`confluence:SWE:*` marks the views of the space and of every page below it, and counts for `confluence` above it. `hub.Version` of each page in the space reads at least that commit. A topic of `*` alone touches everything, as `hub.MarkAll()` does. `Tx.Touch`, `OnCommit` and `hub.Changed` take these topics as they take any other.

### Ids from outside the app

An id inside a topic must not contain `:`, or it makes levels of its own: an iCalendar UID such as `urn:uuid:…` in `cal:` + uid is three levels, not two. Ids from outside the app go through `live.Topic(parts...)`, which escapes `:`, `*` and `%` in each part as `%3A`, `%2A` and `%25` and joins the parts with `:`. A part without them comes out as it is, so `live.Topic("board", "42")` is `board:42`, the topic an app already builds by hand. An empty part becomes `%`. `live.Below(topic)` adds the `:*`.

### What a view may listen to

A view's topics have no empty segment and no `*`: `Serve` and `Watch` panic on `a::b`, `a:` or `a:*`, and `Depends` fails its render. A view on `confluence:SWE` already hears everything below it. `hub.Changed` does not check its topics, since it runs for every commit.

### Aggregates on a level

Cached aggregates key on the level they cover: a space's count on `hub.Version("confluence:SWE")`, the counts per source on `hub.Version("confluence")`. The hub keeps the versions of the levels above the topics commits name apart from theirs (`live.Options.PrefixVersions`, 32,768 by default), so a sync of 79,000 pages pushes pages out of `Options.Versions` and leaves the spaces and sources: a count of a quiet space keeps its value through a sync of another one ([caching against a version](/docs/guides/reads#caching-an-aggregate-against-a-version)).

### What a level costs

Each level costs a commit a little more. The hub records a version for each topic above the one named, and finds the views of the topic and of each level above it in a tree of the topics views listen to, which drops a level once no view listens to it or below it. On 1,000 views, a commit of two topics costs 150 ns at one level (`board7`), 190 ns at two (`board:7`) and 220 ns at three (`src:0:7`); before the hierarchy, one at two levels cost 130 ns. A `:*` costs a walk over the views below the level, about 50 µs for 1,000 of them (`BenchmarkChanged`, `BenchmarkChangedBelow` in `live`).

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

## Views in a shared database, per person

In a database that people share, two people on the same route may see different things: a restricted page is in one person's list and not in the other's. A view whose `Render` filters by the person then needs the person in its key, together with everything else it filters by: the route, the person and the filters the page asked for. The wiki in `site/internal/docs/example/access` keys the list of a space's pages that way:

```go source=site/internal/docs/example/access/access.go
// PagesView is the list of a space's pages for one person, filtered as
// the page asked. Its key holds everything Render filters by: the space
// (the route), the person and the filter. The person reaches Render
// through the closure, and the key keeps two people's lists apart. A
// space that does not exist says so in the list's place.
func PagesView(person, space int64, filter string) live.View {
	return live.View{
		Key:    fmt.Sprintf("pages:%d:person:%d:q=%q", space, person, filter),
		Topics: []string{SpaceTopic(space), GrantsTopic(person)},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			var exists bool
			if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM spaces WHERE id = ?)`, space).Scan(&exists); err != nil {
				return nil, err
			}
			if !exists {
				return NoSpace.Render(ctx, tx)
			}
			pages, err := Readable(ctx, q, person, space, filter)
			if err != nil {
				return nil, err
			}
			return renderPages(pages), nil
		},
	}
}
```

Views with the same key are one view. While it is watched, the hub uses the definition it was first given, renders it once a tick and sends the frame to every stream that watches the key. With a key of the route alone, `pages:1`, the first person to open the space decides what the view renders, and everyone who opens the space after them gets that person's list, restricted titles included, until nobody watches it any more. The example's `TestRouteKeyLeaks` shows the leak, and `TestPagesPerPerson` shows two people on one route getting two frames, and a grant reaching only its person's list.

`Render` gets a context and a transaction: no request, no stream, no session. That is on purpose, since one render serves every stream that watches the view, and the key is the only way anything about the viewer reaches it. The stream's handler reads the person from the session and builds the view with them, so the person is closed over and named in the key:

```go source=site/internal/docs/example/access/access.go
// Stream is the stream of the route /spaces/{space}. The person comes
// from the session, never from the URL: whoever could put another
// person's id in the URL would watch that person's list. A request
// without a person, or with a space id that is no number, gets a stream
// all the same, whose view says so in the list's place: the page shows
// no answer of 401 or 404, and retry 'error' would ask again for ever.
func (s *Server) Stream(w http.ResponseWriter, r *http.Request) {
	view := SignedOut
	if person, ok := s.Person(r); ok {
		view = NoSpace
		if space, err := strconv.ParseInt(r.PathValue("space"), 10, 64); err == nil {
			view = PagesView(person, space, r.URL.Query().Get("q"))
		}
	}
	s.Hub.Serve(w, r, live.Spec{Views: []live.View{view}})
}
```

A request from someone signed out, or for a space that does not exist, still gets a stream: a 401 or a 404 would show the page nothing, and `retry: 'error'` would ask again every 10 seconds for ever ([Pages and Datastar](/docs/guides/pages#reconnecting-after-a-deploy)). Its view says so in the list's place:

```go source=site/internal/docs/example/access/access.go
// SignedOut is the list's place for a request without a person: a link
// to sign in. Nothing in it depends on the viewer, so one view serves
// everyone signed out.
var SignedOut = live.View{
	Key: "pages:signed-out",
	Render: func(context.Context, *sql.Tx) (any, error) {
		return sse.PatchElements([]byte(`<p id="pages"><a href="/signin">Sign in</a> to read this space's pages.</p>`)), nil
	},
}

// NoSpace is the list's place for a space that does not exist.
var NoSpace = live.View{
	Key: "pages:no-space",
	Render: func(context.Context, *sql.Tx) (any, error) {
		return sse.PatchElements([]byte(`<p id="pages">No such space.</p>`)), nil
	},
}
```

Code that builds a view from its key alone, such as a handler that calls `hub.Watch` with a key the page sent, parses the person out of the key and checks it against the request's session first. A view never takes the person from the URL or from a signal.

The view listens to the topics of what it shows: the space's, for its pages, and the person's grants. A command touches what it changed. A new page touches the space, since anyone on it may be allowed to read it; a grant touches only its person's topic, so it renders one list again, not everyone's:

```go source=site/internal/docs/example/access/access.go
// Grant lets a person read a restricted page. Only that person's lists
// change, so it touches the person's topic, not the space's.
type Grant struct{ Person, Page int64 }

func (c Grant) Apply(tx *writer.Tx) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO grants (person, page_id) VALUES (?, ?)`, c.Person, c.Page); err != nil {
		return err
	}
	tx.Touch(GrantsTopic(c.Person))
	return nil
}
```

A command does not touch a topic per person: it would have to know who watches. The cost is a render per person: a change to a space that thirty people watch renders thirty lists in the next tick, where a shared view would render one, and `hub.Stats().RendersByKey` shows a key per person. Keep those views small. The parts that are the same for everyone who may see them, such as a page's body, go into a view keyed by the page and shared: the handler checks that the person may open the page before the stream watches it, and whatever takes the right away also ends that watch in the person's streams with `hub.Unwatch`. `live.Cache` keeps fragments by their content, so the per-person lists copy a row's HTML instead of rendering it again.

Sharing one frame between people whose views are nearly the same cannot be done safely in general. The key would have to name what those people have in common, a role say, instead of the person, and the render would have to filter by that and nothing else: no name, no "you", no button that depends on one person's grant. A change to a person's grants would have to move their streams to another key with `Unwatch` and `Watch`, in the same order as the commit, or they keep watching what they may no longer see. A filter in `Spec.Write` instead, after the render, reads the grants outside the render's snapshot and runs for every stream at every tick. Render per person unless a measurement shows that the renders cost too much, and then share by role, with the role in the key.

## A per-tab view that navigates

A tab that can show any route keeps the route in its row, and one view keyed by the tab renders whatever the route names: a day reads merge requests, issues, todos and the calendar, an issue page one issue and its comments. What the view reads depends on the route, so its topics do too. The tab's own topic stays in `Topics`, and the render declares the rest with `live.Depends` as it reads them:

```go
func tabView(sid, tab string) live.View {
	key := "tab:" + tab
	return live.View{
		Key:    key,
		Topics: []string{key}, // the tab's row, with its route
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			st, err := queries.Tab(ctx, q, sid, tab)
			if err != nil {
				return nil, err
			}
			var page templ.Component
			switch st.Route.Kind {
			case "day":
				live.Depends(ctx, "mrs", "issues", "todos", "calendar")
				d, err := queries.Day(ctx, q, st.Route.Day)
				if err != nil {
					return nil, err
				}
				page = views.Day(d)
			case "issue":
				live.Depends(ctx, "issue:"+st.Route.ID)
				is, err := queries.Issue(ctx, q, st.Route.ID)
				if err != nil {
					return nil, err
				}
				page = views.Issue(is)
			}
			var buf bytes.Buffer
			if err := page.Render(ctx, &buf); err != nil {
				return nil, err
			}
			return sse.PatchElements(buf.Bytes(), sse.Patch{Selector: "#main"}), nil
		},
	}
}
```

A link is a command that writes the route and touches the tab's topic. The view renders the new route and declares its topics, and once the render returns they replace the old route's: a commit to an issue the tab shows now renders it, a commit to the day it showed before does not. The stream keeps watching the same view, so there is no `Unwatch` and `Watch` to write, and no route in the key.

- A query helper can declare its own topic next to its query, so the topics follow the reads wherever the helper is called. A topic declared twice counts once, and outside a render `Depends` does nothing.
- A commit that touches a topic the render declares for the first time can land after the render's snapshot and before the hub takes the topic on, when no `Changed` would find the view for it. The hub compares each new topic's version with the render's `live.Seq(ctx)`, the last commit its snapshot is sure to hold, and renders the view again when the topic changed since ([caching against a version](/docs/guides/reads#caching-an-aggregate-against-a-version) has the numbers).
- A render that fails keeps the topics of the one before. What every route reads, such as the tab's row and the session's preferences, stays in `Topics`.
- A render that declares the topics the one before declared costs a sort and a compare of them. One that changes four topics for four others costs about 2 µs more under the hub's lock, measured with 1,000 views that all navigate in the same tick (`BenchmarkTickDepends` in `live`).

## Opening a stream

The stream's handler builds the tab's views and hands them to `hub.Serve`, which streams until the client leaves or the hub stops:

```go source=site/internal/docs/example/web/server.go
views := []live.View{boardView(id), presenceView(s.hub, id), tabView(sid, tab), prefsView(sid)}
if state.Panel != 0 {
	views = append(views, cardView(state.Panel))
}
err = s.hub.Serve(w, r, live.Spec{ID: streamID(sid, tab), Views: views, WriteTick: tailor(), Reconnect: StreamID})
```

`Serve` sends each view's newest frame at once when the hub keeps it, else a frame rendered in the next tick, then every new frame. The hub keeps the newest frame of a view that two or more streams watch, such as the board, and of any view whose frame is not `[]byte`. Of a view that one stream watches, such as the tab's, it keeps only the frame's length and hash once the stream has the frame: a render that repeats it still goes to no stream, and a tab's frame costs no memory between its writes ([the measurements](/docs/performance/budget#joining-a-view)). A stream keeps only the newest frame of each view, so a slow client skips frames and holds up no one else. Views that change in the same tick reach the stream together, in the order of `Spec.Views`.

`live.Spec` describes the stream:

| Field | What it does |
| --- | --- |
| `ID` | names the stream for `Watch`, `Unwatch` and `Notify`: the session and the tab id, so that an action reaches only its own session's streams. A new stream with the ID of an open one replaces it, and renders the views only that ID watches afresh, so a commit the hub never heard of (another process's, during a deploy) shows; a view other streams watch comes from the hub's kept frame and shows such a commit only with the next one that touches its topics, so a new process that overlapped an old one calls `hub.MarkAll()` once the old one has stopped; an older request that arrives late ends at once |
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

A tab opens a card's panel, and its stream should start rendering that card. `hub.Watch(id, view)` adds a view to the open stream with that ID, and `hub.Unwatch(id, key)` takes one away. The panel's handler records the open panel in the tab's row first, so a stream that reconnects after a deploy watches the card from its start:

```go source=site/internal/docs/example/web/server.go
old, err := s.db.ViewValue(ctx, func(tx *sql.Tx) (board.TabState, error) { return board.Tab(ctx, tx, sid, a.Tab) })
if err != nil {
	s.failed(w, r, a, err)
	return
}
if !s.do(w, r, a, board.OpenPanel{Session: sid, Tab: a.Tab, Card: a.Card, At: time.Now()}) {
	return
}
if old.Panel != 0 && old.Panel != a.Card {
	s.hub.Unwatch(streamID(sid, a.Tab), board.CardTopic(old.Panel))
}
if a.Card != 0 {
	s.hub.Watch(streamID(sid, a.Tab), cardView(a.Card))
}
```

A command can arrive before its stream: a page that opens a panel while it loads. `Watch` then keeps the view for `Options.PendingWatch`, 10 s by default, and the stream that opens with that ID within that time watches it from its start, after the views of its `Spec`. `Watch` reports whether the stream was open, so there is no retry loop to write.

## Sending one stream a notice

`hub.Notify(streamID, key, frame)` sends a frame to one open stream once, after the frames of the renders pending for the stream's views when it is called ([Commands](/docs/guides/commands#the-boards-handlers) has the details), and keeps nothing: no view renders it again, and a stream that reconnects starts without it. It is for what a person must know that no command decided, so no view can render it from the database: the writer failed, a rate limit turned an action away ([Commands](/docs/guides/commands#outcomes-notices-and-the-pages-own-words)). The frame goes through the stream's `WriteTick` or `Write` under the key, like a view's frame, or out as it is when it is `[]byte` events. The stream keeps one notice per key, so give a kind of notice a fixed key, never the op id: a later notice with the same key replaces one that has not gone out yet, in its place. A stream keeps `live.MaxNotices`, 64, waiting at most: a notice with a new key beyond them drops the oldest, so keys a client names (an op key) cannot pile up. A pending render that fails holds a notice no longer than its tick: the notice goes out without that view's frame, since nothing renders the view again until a commit touches its topics, which a failing database may not bring for a while. A notice for a stream that is not open does nothing, and `Notify` reports whether the stream was open: unlike `Watch`, it keeps nothing for a stream to come, so a tab id a client made up costs nothing. A page whose stream was down learns of it when the stream connects again and the tab's row lacks the op ([Pages and Datastar](/docs/guides/pages#op-ids-and-the-pending-state)). Render a notice where one of the stream's views renders, such as the tab's outcome: the view's next frame replaces it.

`Options.Stopping(spec)` returns the events each stream sends when the hub stops, before its last event, `sse.Reconnect` for a stream with `Spec.Reconnect`: a notice such as "The server is updating. Back in a moment.", or nil for a stream that should send none. They go out as they are, after the `[]byte` notices the stream had not sent yet. Every stream calls it from its own goroutine as the hub stops, so it runs concurrently; a panic in it is logged, and that stream sends none. The new process's first frame of the view whose region the notice took replaces it once the page is back.

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

`Options.MaxStreams`, 2000 by default, caps the open streams: `Serve` answers 503 to the next one and returns `live.ErrFull`. The template sets 200 and this site 300, from their memory: each compressed stream holds its encoder's window for as long as it is open ([Streams and compression](/docs/guides/streams#the-memory-budget)). A stream refused for want of memory gets a 503 too, and a page that opened its stream with `retry: 'error'` asks again. An app whose budget refuses streams (`sse.BudgetOptions.Refuse`) can let memory be the cap: the budget books what each stream holds, its encoder, 32 KiB and the frames of the views it shares with other streams, and turns away the stream it has no room for, compressed or not. `MaxStreams` then stays at its default as a backstop for a process without a memory limit ([Refusing streams](/docs/guides/streams#refusing-streams)).

## The hub's options

| `live.Options` | Default | What it does |
| --- | --- | --- |
| `Read` | none | the read pool; each worker holds a connection for the hub's life |
| `Tick` | 50 ms | the shortest time from one tick to the next |
| `Workers` | GOMAXPROCS | render workers |
| `MaxStreams` | 2000 | open streams at most |
| `PendingWatch` | 10 s | how long `Watch` keeps a view for a stream not open yet |
| `Stopping` | none | the events each stream sends when the hub stops, before its last ([above](#sending-one-stream-a-notice)) |
| `Heartbeat`, `HeartbeatSignal` | 20 s, none | the silence before a heartbeat, and the signal it patches |
| `Hibernate` | never | how long a stream may stay idle before its encoder goes back ([Streams](/docs/guides/streams#hibernation)) |
| `RenderTimeout` | 2 s | how long a render may take before its context ends |
| `OnTick` | none | gets each tick's duration |
| `Versions`, `PrefixVersions` | 32,768 each | the versions kept of the topics commits name, and of the levels above them ([Reads](/docs/guides/reads#caching-an-aggregate-against-a-version)) |
| `SSE` | | the streams' options and their memory budget ([Streams](/docs/guides/streams)) |

The board's settings:

```go source=site/internal/docs/example/web/hub.go
return live.Options{
	Read:            db.R,
	MaxStreams:      500,
	Stopping:        stopping,
	HeartbeatSignal: "_ping",     // the page's watchdog reads it (expr.go)
	Hibernate:       time.Minute, // an idle tab gives its zstd encoder back
	Log:             log,
	SSE:             sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{Refuse: true})},
}
```

`hub.Stats()` counts ticks, renders, frames sent and dropped, renders deferred while every stream watching the view had a frame of it waiting, overruns, open streams, watched views and the workers' read transactions. When a tick takes longer than `Tick`, the hub samples every tick for 20 s and logs their count, p50, p95, p99 and maximum ([Metrics and monitoring](/docs/guides/monitoring)).
