Your tab opens `GET /demos/stream/stream`, and the stream takes its content coding from the request's `Accept-Encoding`: zstd before brotli before gzip. The hub's memory budget gives its encoder a setting, the largest window that its share of the budget allows, and reserves the encoder's heap for as long as the stream is open.

Every 250 ms your tab's table view renders. It reads your size and churn from your tab's row in `site_tabs` and takes that step's table, which renders once for all the tabs that chose the same. The frame is the whole table, about 28 KB of HTML for the large one; the encoder finds most of it in the frame before and sends what changed.

The site's frame writer sends a tick's frames in one write. Before each write it asks the inspector's probe for a report, which reads the stream itself (`Encoding`, `Tier`, `Sent`), its earlier writes and its budget's `Stats`, and goes out in the same write as a patch of `$_stream`. So the numbers you see describe your stream up to the frame before.

A click on a size or a churn posts it to `/cmd/stream/payload`. The command stores it in your tab's row and touches your tab's topic; the next tick renders the new table and brings the op id back, which ends the button's pending state.

With Still, the table view has nothing new to render, and the stream sends nothing. A second later your last frame gets a report of its own. After 20 s of silence the hub sends a heartbeat, and after a minute it hibernates the stream: the encoder goes back to a pool and its reservation back to the budget, until the next frame wakes it.
