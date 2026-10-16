When you click a tile, the page posts the tile, your tool, the tile's version as your page shows it, its tab id and a new op id to `/cmd/town/place`. The writer applies a `Place` command: `events.Execute` reads the tile's state row, checks the version, lets the decider turn your change into an event, appends it to the tile's stream in `lith_events` and runs two projectors, one for the town now and one for its history. All of it commits in one savepoint, or none of it does.

In the next tick, at most 50 ms later, the town now renders once for every tab at the slider's end, and the slider's end moves for every tab.

When you drag the slider, the page posts a numbered position at most once a tick. A `Seek` command stores it as your tab's state and moves your tab's stream from the views of one position to those of the next. The town as of a position is a view of its own, shared by every tab at that position and read from the history table with one query. New events leave it alone.

Every five minutes a sweep keeps the newest 5,000 events, and the slider starts at the oldest of them.
