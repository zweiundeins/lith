When you press **Start a race**, the page posts your choices, its tab id and a new op id to `/cmd/writer-race/start`. A `Start` command on the site's writer puts the race in line, unless your session has a race in line or on the track already, or six races wait. Either way it stores the outcome in your tab's row of `site_tabs`, and the handler answers 204.

One race runs at a time on the whole site. A goroutine of the demo takes the first race in line onto the track: it opens a SQLite file of the race's own next to the site's database, with the `synchronous` you chose, and runs a second writer on it. Then 1, 50 or 200 producers send transfers to that writer for three seconds, each waiting for its commit before it sends the next. The writer takes whatever waits into one transaction, with a savepoint for each command.

Every 50 ms the demo reads the race writer's `Stats`: commands and transactions so far, the commands waiting, the 99th percentile of the last 1,024 commits, and the transactions counted by size. Each reading becomes a `Record` command on the site's writer, which stores a sample and touches `live:writer-race`. In the next tick the hub renders the track once and sends the frame to every tab that watches it, the landing page's slot included.

When the three seconds are over, the race's writer closes, its file is deleted, and a `Finish` command stores the result. Two seconds later the next race in line starts. The race's writer runs on a thread with a lower CPU and disk priority, so the site and the other services on this server come first.

Every five minutes a sweep keeps the newest 100 races and the samples of the newest 12.
