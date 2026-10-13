The million boxes are 3,907 rows of `checkboxes_chunks`, each a chunk of 256 boxes in a 32-byte BLOB under an integer key. Your page shows a chunk as a tile, inside Starbase's `sb-virtual-scroll`, which keeps only the twelve tiles around the view.

When you click a box, the page posts its number and the state it should take to `/cmd/checkboxes/set`, ten clicks a second for your session after a burst of thirty. The writer applies a `SetBox` command in a batch with whatever else arrived: it reads the chunk, flips the bit unless the box is so already, writes the chunk back, counts the flip, and touches one topic, `checkboxes:chunk:<n>`. The handler answers 204.

In the next tick, at most 50 ms later, the hub renders that chunk's view once and sends the frame to every tab that watches the chunk, and to no other. The frame is that tile alone: about 8.6 KB of HTML, and about 50 bytes on the wire once the tab's zstd stream has seen the tile before. Your box blinks until it arrives.

When you scroll, the list asks for a window. The page posts it to `/cmd/checkboxes/window`; a `SetWindow` command stores it in your tab's row of `site_tabs`, and after the commit moves your tab's stream: `hub.Unwatch` for the chunks you left, `hub.Watch` for the new ones, which send their newest frame at once. Then your tab's own view sends the list at its new offset.

The numbers render once a second for every tab. Every five minutes a sweep keeps the last hour of per-second counts, and forgets the windows of tabs quiet for a day.
