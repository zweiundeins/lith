---
title: Immediate mode
headline: Immediate mode: view = f(state), rendered whole
summary: A region of a page is view = f(state): rendered whole from the database, morphed into the page by Datastar, with nothing in the browser that guesses ahead.
order: 2
---

The name comes from immediate-mode user interfaces, which draw every frame from the current state and keep no record of what is on screen. lith's pages work the same way. A region of a page has one render function. It reads the state from the database, renders all of its HTML, and Datastar morphs that HTML into the page by the elements' ids. The same function draws the region into the GET document and into every frame on the stream.

## Rendered whole

A render never patches a list item by item. When an entry is added, the view renders the whole list again, and Datastar's morph keeps every element that did not change: its DOM node, its focus, the text in its fields. So the server needs no diff, and the browser no reducer.

What that costs is bytes, and the streams' compression takes most of them back: a frame that repeats the last one but for a card costs a few hundred bytes on the wire (see [Hypermedia](/docs/concepts/hypermedia)).

## One snapshot per render

A render reads in one read transaction, so it agrees with itself. In a view, the worker hands `Render` its transaction, and `sqlite.QuerierFrom(ctx)` runs each query on a statement prepared once on the worker's connection. In a GET, `db.View` opens one transaction for the whole document. A store that calls `db.View(ctx, fn)` from inside a render joins the render's transaction instead of opening a second snapshot.

## No guessing

The page never shows an outcome before the server has decided it. A click shows the user's own input at once and marks it pending, the action goes to the server, and the outcome arrives on the stream with the next frame. A refusal comes back the same way and shows next to where the user acted. There is no optimistic update to roll back, because nothing in the browser predicted the server.

The template's guestbook does this with an op id: the page makes one for each signature, the command stores it with the outcome in the tab's row, and the tab's view sends it back as a signal. The page leaves its pending state when the op id it waits for arrives.

## State the page needs

- State that must survive a deploy lives in SQLite, written by commands: a tab's state in a row keyed by its tab id, a preference in a row keyed by the session.
- A part that differs per viewer, such as an outcome, "you" in a list or a toast, renders in a small view keyed by the tab, never in a shared view.
- Ephemeral state that only the browser needs, such as what someone is typing, lives in signals that start with `_`, which Datastar never sends to the server.

## GET never writes

A GET renders from a read transaction and writes nothing: no session row, no counter of visits. The edge hands every browser its session id in a cookie without storing it, and a row appears with the first command that needs one. Crawlers multiply every write a GET makes.
