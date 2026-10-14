# Changelog

Until 1.0, a minor release may change the API; its entry says what to change in an app.

## Unreleased

- `sse`: `ReadSignals` reads a request's Datastar signals for any method: the `datastar` query parameter of GET, HEAD and DELETE, the JSON body of the others, `@query`'s QUERY included. It caps the body at 1 MiB (`ReadSignalsLimit` for another cap), takes a request without signals as none, and reads the body to its end, so that over HTTP/1.1 a handler that waits before it answers still notices a client that left. Called after `Open`, it returns `ErrBodyConsumed`.
- `edge`: `Compress` compresses the answers to QUERY as it does GET's.
- `sse`: a budget books what its streams hold, not only their encoders: each stream's tier, 32 KiB for the rest of the stream (its state, its goroutines' stacks, net/http's buffers), and what `Budget.Hold` books, such as the frames a hub keeps. `Limit`, half the process's memory limit by default, now covers all of it; the other half stays for the rest of the app and for the garbage between collections. `BudgetStats.Frames` reports what `Hold` booked. Without `Refuse`, uncompressed streams may take the total beyond the limit.
- `sse`: a stream's tier is picked at its first write, from its largest write: the window must hold that frame with a sixteenth to spare, or a frame cannot refer to the one before. A brotli stream takes that window and no larger one, at quality 4 while the budget stays at most half full, then at quality 3, and only when that does not fit, quality 3 at 2^16, which compresses each frame alone; it goes out uncompressed only when even that does not fit, and `Refuse` turns away only such a stream. A zstd stream takes the largest tier within its fair share, moves down no further than its frame's window, takes 2^15 only when that window does not fit, and moves up again at a write once room frees up. `MinWindow` no longer defaults to 18: unset, the frames decide; set, it is the smallest window a stream's frames need, for pages whose frames refer back further than one write. With a limit, an unset `ZstdWindow` or `BrotliWindow` lets the budget go up to `MaxWindow`. In 150 MiB, 97 brotli tabs with a 230 KB view each hold 2^18, and 133 zstd tabs. The tiers are booked from measurements at 50 to 230 KB: brotli's 7 to 31% lower, zstd's 32 to 48 KiB higher. Apps that set `MinWindow` to their largest frame can drop it.
- `sse`: `Stream.Upgrade` returns a channel that the stream's budget closes when the stream holds a brotli window below its frames, or went out uncompressed, and a stream opened now would get a window that holds them: at most one stream a second. `BudgetStats.Reopened` counts them.
- `live`: the hub books each view's newest frame in the streams' budget, once for all the streams that watch it: a `[]byte` frame at its length and 8 KiB, another by its `Len() int` method. A stream with `Spec.Reconnect` ends with `sse.Reconnect` when its budget has a better tier for it, and the page opens it again; the same stream ID at most once a minute, a stream without an ID after a minute.
- `sse`: with `Refuse`, a budget also turns away a stream that compresses nothing (its client accepts no coding, or `NoCompression` is set) when its 32 KiB do not fit, and a zstd stream keeps 2^15 booked from `Open` to `Close`, before its first write and while it hibernates too, and takes that tier when nothing larger fits or a lower limit moves it down. So no stream the budget lets in goes out uncompressed or in raw blocks, and memory caps the streams: an app with `Refuse` leaves `live.Options.MaxStreams` at its default as a backstop, and its pages open their streams with `retry: 'error'`.
- `prom`: `sse_budget_frames_bytes` and `sse_budget_reopened_total`; the limit and the reservation cover what the streams hold.
- `sse`: a brotli stream flushes its encoder every 16 KiB of a frame at quality 3 and every 32 KiB at 4. Without those flushes brotli extends a copy into the next input block without hashing it, so after a change in a large frame it lost the frame before and coded the rest in short pieces: a 230 KB board at quality 3 and 2^18 took 3,173 bytes a frame and now takes 259, a 230 KB data table that scrolls 5,756 and now 227 (quality 4: 2,382 and 140, 3,450 and 134). The encoder's heap no longer grows with the frame: at 230 KB, quality 3 at 2^18 held 1,554 KiB against the 1,312 its tier booked, and now holds 1,083; quality 4 at 2^18 2,370 against 2,192, now 1,603. Each flush costs a metablock header: a 230 KB list with 20 changes spread through it takes 676 bytes a frame instead of 363, and a 50 KB board at quality 4 50 instead of 34. The CPU per frame of a 230 KB frame changes by 13% less to 30% more.

## v0.1.1 (2026-10-14)

- `sse`: with `ZstdLowMemory` set and a memory budget (every `live` hub has one), a client that accepts brotli but not zstd, such as Safari, got its stream uncompressed: the low-memory filter dropped the brotli tiers too. It now narrows zstd's tiers only. Apps that set `ZstdLowMemory` should update.

## v0.1.0 (2026-10-13)

The first release: the runtime for Go, SQLite and Datastar apps, with a template for new apps, a skill for coding agents and the website at lith.zweiundeins.gmbh. It needs Go 1.27 and builds with `CGO_ENABLED=0`.

### Packages

- `sqlite`: one write connection and a `query_only` read pool; pragmas; migrations by `user_version` or a project's own runner; read-only opens; `View` and `ViewValue` in one snapshot, joining a transaction a ctx carries; `VACUUM INTO` backups. Each read connection keeps the 128 statements it ran last prepared.
- `writer`: the single writer. `Send`, `Do` and `DoValue` queue commands, which run in batches of one transaction with a savepoint each, on statements prepared once; `Once` and `OnceResult` for requests a client retries; `Flush`; `Exclusive` for work between batches; checkpoints and `PRAGMA optimize`.
- `live`: commits touch topics, topics mark views dirty, and each tick renders every dirty view once, in one read transaction per worker, for all the streams that watch it. Presence, `Every`, `Watch` and `Unwatch`, `WriteTick`, a stream cap, heartbeats, and `Spec.Reconnect`, which ends each stream with `sse.Reconnect` when the server stops, so that pages open their stream again on the next process.
- `sse`: Datastar's events, formatted once and written to many streams; zstd, brotli or gzip per stream, with a deadline on every write; a memory budget that shrinks the streams' windows as more open; idle streams that give their encoder back (`Hibernate`).
- `edge`: sessions, CSRF, the client's address behind a proxy, a CSP with a nonce, compression, precompressed static files, rate limiters, request logs, health checks.
- `tenant`: one open database per account, refcounted, evicted at a soft cap.
- `dbmetrics`: statement timing and a slow-query log through a wrapping driver.
- `loadtest`: the writer and the render loop under load, in process, without HTTP.
- `backup` (a module of its own): Litestream to S3, a restore when the file is missing, snapshots, a daily verify.
- `dbmetrics/prom` and `prom` (modules of their own): the statements', the writer's, the hub's and the stream budget's numbers in Prometheus.

### Defaults to know

- `synchronous=NORMAL`: a power loss or a kernel crash can undo the commits of the last 35 seconds or so; a crash of the process loses none. `sqlite.Options{Synchronous: "FULL"}` syncs every batch.
- The streams share a memory budget of half of GOMEMLIMIT, or of the cgroup's limit: a few streams compress with 512 KiB windows, many with 256 KiB, and the rest go out uncompressed, or not at all with `Refuse`.
- An idle stream keeps its encoder until `live.Options.Hibernate` is set.
- A page opens its stream with `retry: 'error'` from an element with an id, the one `Spec.Reconnect` names.

### Tools

- `cmd/lith-new` copies the template into a new module that requires the lith it ran from: a guestbook with tests, CI, a systemd unit, a Caddy block, a deploy script with rollback and a backup timer.
- `skills/lith`: the rules for coding agents, with review commands, Datastar notes, recipes and performance numbers.
- `bench`: the writer, render and stream benchmarks, with the commands to run them again (`bench/RESULTS.md`).

### Website

lith.zweiundeins.gmbh runs on lith, from `site/`: the landing page, the docs (guides that follow one app, Datastar notes, performance, the package reference from the doc comments, a review checklist and this changelog), and eight live demos: a guestbook, a writer race, a stream inspector, a pixel canvas, a shared board, a cursor party, a Game of Life and a million checkboxes.
