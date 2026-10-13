When your pointer moves over the floor, the page posts its place to `/cmd/cursor-party/move`: two fractions, of the floor's width and of its height, with the tab id and the number of the move. Datastar's throttle lets one move through every 67 ms, about 15 a second, and a debounce sends the place where the pointer comes to rest.

The handler checks the rate limits for your address and your session, queues a `Move` command with `w.Send` and answers 204 without waiting for the commit, since the next move replaces this one.

The writer applies the moves in the order they arrive. A move updates your tab's row in `cursor_party_cursors`; the first one inserts it, with a name and one of six colours that follow from a hash of the tab id. A move with a lower number than the row's changes nothing, because one request can overtake another on the way. Each move counts itself in the current second and touches the topic `cursor-party`.

In the next tick, at most 50 ms later, the hub renders the layer once: the 64 newest cursors that moved in the last 6 seconds, an `<i>` each, with its place in a style attribute. Every tab at the party gets the same frame, on this page and behind the landing page's title. Each stream compresses it with a zstd window of its own, so a frame of 50 cursors, 4.6 KB of HTML, takes about 360 bytes on the wire.

That frame holds your own cursor as well. The page came with a `<style>` that hides it by its id, which the server makes from a hash of your tab id, so the id gives no tab away.

A cursor that has not moved for 3 seconds fades, after 6 it leaves the layer, and after 2 minutes its row goes.

Fifty pointers moving at the full rate, 746 moves a second, cost the writer, the renders and the compression for their 50 tabs 0.19 of a core, in process on an Intel i5-13500. Each move takes 8 µs through the site's edge.
