While you drag, `sb-pixel-board` collects the cells you cross and every 80 ms posts them to `/cmd/canvas/paint` with your brush and your tab id. The handler checks two token buckets of pixels, 20 a second with a burst of 60 for your session and four times that for your address, and waits for the writer. It answers 204 and nothing else.

The writer applies `Paint` in one transaction with everything else that arrived meanwhile. The board is one row in SQLite: 2,048 bytes, two cells to a byte, whose hex is the board's `cells` attribute. The command sets the cells' colours in that row, starts each cell's day in `canvas_painted`, and counts your pixels for the last minute.

In the next tick, at most 50 ms later, the hub renders the board once and sends the same frame to every painter: 4.8 KB of HTML, about 40 bytes on the wire after a one-pixel paint, since each stream's zstd window still holds the frame before. Your faded pixels turn solid when that frame brings them back.

The list of painters is the hub's own: the streams watching it, each shown by a hash of its tab id. Every five minutes a sweep fades the pixels painted more than a day ago, and forgets the painters gone for a day.
