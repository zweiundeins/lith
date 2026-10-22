---
title: The first command and view
nav: First command and view
headline: Your first command and live view in lith
summary: Add a feature the lith way: a command that changes state, a handler that answers 204, a view that renders it for everyone, and the stream that brings it.
order: 2
---

This page adds a counter that everyone shares to the template: a button adds one, and every open tab shows the new count in the next tick. It touches each part once.

## The table

A migration adds the state. Migrations are numbered files in `migrations/`, applied in order at startup. Add a file for every change, and never edit one that has shipped.

```sql title=migrations/002_counter.sql
CREATE TABLE counter (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    count INTEGER NOT NULL
);
INSERT INTO counter (id, count) VALUES (1, 0);
```

## The command

A command is a value with an `Apply` method. The writer runs it inside its batch's transaction, in a savepoint of its own, and nothing else writes to the database.

```go title=internal/counter/counter.go
// Topic is touched by every change to the count.
const Topic = "counter"

// Add adds N to the count.
type Add struct{ N int }

func (c Add) Validate() error {
	if c.N < 1 || c.N > 10 {
		return errors.New("add 1 to 10 at a time")
	}
	return nil
}

func (c Add) Apply(tx *writer.Tx) error {
	if _, err := tx.Exec(`UPDATE counter SET count = count + ? WHERE id = 1`, c.N); err != nil {
		return err
	}
	tx.Touch(Topic)
	return nil
}
```

`Validate` runs before the command is queued, and `Do` returns its error wrapped in `writer.ErrInvalid`. `Touch` names what changed: after the commit, every view that listens to `counter` renders again. A command that touches nothing leaves every page as it was.

## The handler

```go title=internal/web/server.go
mux.HandleFunc("POST /cmd/add", func(w http.ResponseWriter, r *http.Request) {
	switch err := s.w.Do(r.Context(), counter.Add{N: 1}); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, writer.ErrInvalid):
		w.WriteHeader(http.StatusBadRequest) // the handler sets N: a bug
	default:
		s.fail(w, r, err) // logs, and answers 500
	}
})
```

`Do` waits for the commit and returns the command's own error. The answer carries nothing for the page: the new count reaches every tab, this one included, on its stream. Its status carries nothing a person reads either: what the person must hear of reaches the tab on its stream too ([Commands](/docs/guides/commands#outcomes-notices-and-the-pages-own-words)). A route under `/cmd/` passes the CSRF check only with the `Datastar-Request` header that Datastar sends, which a form on another site cannot.

## The query

Query code takes a `sqlite.Querier`. The same function then serves a GET, a view and a command: `*sql.Tx`, `*writer.Tx` and a render's querier all satisfy it.

```go title=internal/counter/counter.go
// Count reads the count.
func Count(ctx context.Context, q sqlite.Querier) (n int, err error) {
	err = q.QueryRowContext(ctx, `SELECT count FROM counter WHERE id = 1`).Scan(&n)
	return n, err
}
```

## The view

A view is one region of a page: a key, the topics it listens to, and a function that renders it. Everyone who sees the same thing shares the key, and the hub renders the view once per tick for all of them.

```go title=internal/web/live.go
func counterView() live.View {
	return live.View{
		Key:    "counter",
		Topics: []string{counter.Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx) // the worker's prepared statements, in this render's snapshot
			n, err := counter.Count(ctx, q)
			if err != nil {
				return nil, err
			}
			var buf bytes.Buffer
			if err := views.Counter(n).Render(ctx, &buf); err != nil {
				return nil, err
			}
			return sse.PatchElements(buf.Bytes()), nil // morphs #counter into the page
		},
	}
}
```

The frame is formatted once, and the same bytes go to every stream. Anything about the viewer, such as a name, "you" in a list, or a pending action, belongs in a small view keyed by the tab, never in a shared one.

## The page

```templ title=internal/views/counter.templ
templ Counter(n int) {
	<p id="counter">{ strconv.Itoa(n) }</p>
}
```

The page's GET renders the counter from one read transaction (`db.View`), the same `views.Counter` the view renders. An element with an id opens the tab's stream, and a button sends the action:

```html
<div id="stream" hidden data-init="@get('/stream?tab=' + $_tab, {retry: 'error', retryMaxCount: Infinity})"></div>
<button data-on:click="@post('/cmd/add')">Add one</button>
```

The stream's handler adds the view to the tab's stream:

```go
s.hub.Serve(w, r, live.Spec{ID: sid + "." + tab, Client: edge.ClientKey(r), Views: []live.View{counterView(), tabView(sid, tab)}, Reconnect: "stream"})
```

`ID` names the stream after the session and the tab, so that an action reaches only its own session's streams. `Client` counts the stream under the client's address, so that one address cannot hold every stream (`live.Options.MaxStreamsPerClient`). `Reconnect` names the element that opened the stream. When the server stops for a deploy, the stream's last event replaces that element with a copy, the copy's `data-init` runs again, and `retry: 'error'` keeps the request trying through the restart until the next process answers.

## Pending and refused

The template's guestbook shows the rest of an action's life: an op id for each action, a view of the tab's own that brings back the op's outcome, and a button that stays pending until it arrives. The site's [guestbook demo](/demos/guestbook) is the same code with its listings beside it, and [Immediate mode](/docs/concepts/immediate-mode) explains why the page never guesses the outcome.
