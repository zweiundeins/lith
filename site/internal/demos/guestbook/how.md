## The command

Every signature is a `Sign` command, and the writer is the only code that writes. `Apply` reads the newest entry and decides in the same transaction, so two people who send the same words at once get one entry, and the second hears why.

```go title=commands.go
func (c Sign) Apply(tx *writer.Tx) error {
	name, message := clean(c.Name), clean(c.Message)
	state := TabState{Op: c.Op}
	var lastName, lastMessage string
	err := tx.QueryRow(`SELECT name, message FROM guestbook_entries ORDER BY id DESC LIMIT 1`).Scan(&lastName, &lastMessage)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	case links.MatchString(name + " " + message):
		state.Refusal = "Links stay out of this guestbook."
	case err == nil && lastName == name && lastMessage == message:
		state.Refusal = "That is the newest entry already."
	default:
		if _, err := tx.Exec(`INSERT INTO guestbook_entries (name, message, at) VALUES (?, ?, ?)`,
			name, message, c.At.Unix()); err != nil {
			return err
		}
		tx.Touch(Topic)
	}
	return demo.SaveTab(tx, Slug, c.Viewer, state, c.At)
}
```

A refusal is not an error: the command still commits, with the reason in the tab's row and no entry. `demo.SaveTab` writes that row of `site_tabs` and touches the tab's topic, so only this tab's view renders the refusal.

## The action

The site mounts the action at `/cmd/guestbook/sign` behind its rate limits. The handler waits for the commit and answers 204; `writer.Once` makes a request that the network sends twice sign once.

```go title=routes.go
func mount(k *demo.Kit) {
	k.Command("sign", demo.Rate{Every: signEvery, Burst: signBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab, Op       string
			Name, Message string
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok || !demo.ValidID(req.Op) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		cmd := Sign{Viewer: v, Op: req.Op, Name: req.Name, Message: req.Message, At: time.Now()}
		// A retried request (a flaky network) signs once.
		k.Do(w, r, writer.Once(Slug+":"+v.Session+":"+req.Op, cmd))
	})
}
```

## The views

The list is one view for everyone: its key has nothing of the viewer in it. It renders once per tick in which the `guestbook` topic changed, and every tab gets the same frame.

```go title=live.go
func entriesView() live.View {
	return live.View{
		Key:    Slug + ":entries",
		Topics: []string{Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			list, total, err := latest(ctx, demo.Querier(ctx, tx), Shown)
			if err != nil {
				return nil, err
			}
			return demo.Animated(ctx, Entries(list, total, time.Now()), sse.Patch{ViewTransitionSelector: "#guestbook-entries"})
		},
	}
}
```

`demo.Animated` renders the frame twice over: the first frame a tab gets morphs at once, and every later one inside a view transition (Datastar's `useViewTransition`). The tab's own view, keyed by its tab id, renders the line under the button and patches the signal `$_guestbook.ack` with the op id of the outcome.

## The page

The page keeps its state in signals under `$_guestbook`, which stay in the browser. Pressing **Sign** makes an op id and posts the form; the inputs turn read-only and the button busy while `$_guestbook.op` is set.

```go title=expr.go
const sign = `$_guestbook.op === '' && $_guestbook.name.trim() && $_guestbook.message.trim() && (
	$_guestbook.problem = '',
	$_guestbook.op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
	@post('/cmd/guestbook/sign', {payload: {tab: $_tab, op: $_guestbook.op, name: $_guestbook.name, message: $_guestbook.message}})
)`
```

When the tab's view brings back the same op id, the pending state ends, and an accepted message is cleared:

```go title=expr.go
const settle = `$_guestbook.op !== '' && $_guestbook.ack === $_guestbook.op && ($_guestbook.accepted && ($_guestbook.message = ''), $_guestbook.op = '')`
```

## The arrival

Each entry has a view-transition name of its own, so when the list's frame arrives, the entries already there glide down to their new places. A new entry starts from its `@starting-style`: blurred, a little higher, its edge glowing, and settles in.

```css title=guestbook.css
.gb__entry[data-fresh] {
	border-inline-start-color: var(--lith-accent);

	@starting-style {
		opacity: 0;
		translate: 0 -0.75rem;
		filter: blur(6px);
		border-inline-start-color: var(--lith-ember-1);
		box-shadow: 0 0 28px var(--lith-glow-color), inset 0 0 0 1px var(--lith-accent);
	}
}
```

An entry counts as fresh for eight seconds after it was signed. The list is the same for every tab, so a tab that opens in those seconds sees the entry arrive too.
