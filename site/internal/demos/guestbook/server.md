When you press **Sign**, the page posts your name, your message, its tab id and a new op id to `/cmd/guestbook/sign`. The handler checks the rate limits for your address and your session, wraps a `Sign` command in `writer.Once` and waits for the commit. Then it answers 204, an answer with nothing in it for the page.

The writer applies `Sign` in one transaction with whatever else arrived meanwhile, in a savepoint of its own. The command adds the entry, or refuses it, and stores the outcome in your tab's row of `site_tabs`. It touches two topics: `guestbook`, and your tab's.

In the next tick, at most 50 ms later, the hub renders the list once and sends that frame to every tab that shows it, the landing page's slot included. Your tab's own view renders as well: the line under the button, and the op id that ends the pending state. Each stream compresses its frames with a zstd window of its own.

Every five minutes a sweep keeps the newest 200 entries, and forgets the state of tabs that have been quiet for a day.
