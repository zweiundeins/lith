# Review checklist

Run these against a project before a release, and on any change that touches
the database, a stream or a handler. Each line names what is wrong when the
pattern matches.

## Writes

- `rg -n '\.(Exec|ExecContext)\(' --type go -g '!*_test.go'`: every hit must
  sit inside a writer command (`Apply(tx *writer.Tx)` or a `writer.Func`), in
  a `w.Exclusive` fn, in migrations, or in a test. A store method that writes
  on its own is a second writer.
- `rg -n 'sql\.Open\(' --type go` outside lith: a second connection that
  writes to the database file (one kept for a factory reset or an import,
  say) takes the write lock beside the writer, and a transaction
  on it past the busy timeout fails the writer's batches with `SQLITE_BUSY`.
  Pragma toggles, ATTACH, DETACH and VACUUM go in `w.Exclusive`, on the
  writer's own connection.
- `rg -n 'BeginTx\([^)]*nil\)|\.Begin\(\)' --type go`: a deferred
  transaction that reads and then writes can fail with `SQLITE_BUSY_SNAPSHOT`
  next to Litestream's connection; `busy_timeout` does not retry it.
- `rg -n 'SetMaxOpenConns\(1\)' --type go` outside lith: a pinned connection
  serialises writes but does not batch them, and reads queue behind it.
- A command that runs several autocommits (insert, then audit row, then
  publish) is not atomic. One command, one `Apply`, one savepoint.
- ``rg -n 'tx\.(Exec|Query|QueryRow)\((fmt\.Sprintf|"[^"]*" *\+|`[^`]*` *\+)' --type go``:
  SQL built at runtime in a command (an IN list with a mark per value) takes
  a place in the writer's statement cache for every text it makes. Run it
  through `tx.ExecUncached`, `tx.QueryUncached` or `tx.QueryRowUncached`, or
  with a ctx from `sqlite.Uncached`.
- `rg -n 'http\.(Get|Post)|smtp\.|\.Send\(' --type go` inside commands:
  network calls do not belong in the writer. Use `tx.AfterCommit`.
- `rg -n 'writer\.Once(Result)?\(' --type go`: `Once` and `OnceResult` keys
  are one namespace per database. Every key starts with who sent it
  (`sid + ":" + op`); a key from the payload alone lets a client that learns
  another user's op id turn that user's command into a no-op, or read its
  recorded result. `rg -n 'Once(Result)?\([a-z]+\.(Op|ID|Key)\b' --type go`
  finds the plainest case.

## Reads

- Render code queries through the transaction it is given. A render that
  opens its own transactions (one for the list, one per open dialog) mixes
  snapshots. (kanban's card dialogs did.)
- No `db.W` outside the writer and migrations; `db.R` opens the file
  read-only (`mode=ro`) and is `query_only`, so a write there fails loudly,
  even after a `PRAGMA query_only = 0` in the text. It can still `ATTACH`
  another file and write that one: no SQL text from outside, ever.
- ``rg -n 'Query(Row)?Context\(ctx, (fmt\.Sprintf|"[^"]*" *\+|`[^`]*` *\+)' --type go``:
  SQL built at runtime on the read side takes a place in a read
  connection's statement cache, or a render worker's, for every text it
  makes, and pushes out the statements that repeat. Run it with
  `sqlite.Uncached(ctx)`.
- `rg -n 'Unwatch\(' --type go`: a view swapped for another on navigation
  (a key that carries the route) can be one view keyed by the tab that
  declares its route's topics with `live.Depends`.

## Events

- `rg -n 'events\.(Load|Since)\(' --type go -g '!*_test.go'`: a render that
  loads a stream, or reads the log from its start, grows with the history.
  Render from a projection; a feed reads `Since` from near the end.
- `rg -n '(INSERT INTO|UPDATE|DELETE FROM) lith_(events|event_state|event_kinds|projections)' --type go`:
  only `lith/events` writes its tables (`Execute`, `Append`, `Forget`,
  `Prune`, `Compact`, the Runner). A write of the app's own skips the version
  check, the state row and the projectors.
- A projector with `Tables` writes only those tables: a rebuild runs its
  handlers over every event again, and a write anywhere else happens twice.

## Requests

- GET handlers never write: no session rows, no events, no counters per hit.
  Crawlers multiply them.
- `rg -n 'r\.Header\.Get\("X-(Real-IP|Forwarded-For)"\)' --type go`: client
  IPs come from `edge`, which trusts those headers only from the local proxy
  and takes the last X-Forwarded-For address before X-Real-IP. The proxy must
  set X-Forwarded-For (Caddy's `reverse_proxy` does by default, nginx with
  `$proxy_add_x_forwarded_for`). Caddy passes on an X-Real-IP the client sent
  unless the site block sets it, so a check that believes X-Real-IP first can
  be dodged.
- `rg -n '\.Allow\((edge\.SessionID|sid\b|v\.Session)' --type go`: a
  session id is no limiter key on its own. A client mints a new one with
  every request that sends no cookie, so each limit by session needs a limit
  by `edge.ClientKey(r)` before it on the same route.
- `rg -n 'Handle\("GET /static/"' --type go`: static files are mounted
  outside `edge.Sessions`, which mints a cookie for any request without one:
  a `Set-Cookie` on a public, immutable answer goes to everyone a shared
  cache hands it to.
- A page that renders a secret of the user (a token, an address) beside
  input the request reflects, compressed, leaks it to BREACH: send it with
  `Cache-Control: no-transform`, which `edge.Compress` honours. lith's
  sessions live in the cookie and its CSRF check in headers, so the template
  has nothing to leak.
- Every non-GET route passes the CSRF middleware; exemptions are listed.
- Every route that changes data checks authentication and membership.
- `rg -n 'edge\.NewWindow\(' --type go`: a limit promised to people, such as
  five messages an hour, is `edge.NewSliding(5, time.Hour)`. A fixed window
  lets up to twice as many through around the edge between two windows, and
  a bucket (`NewLimiter`) gives tokens back as time passes.

## Streams

- `rg -n 'datastar\.NewSSE|WithBrotli\(' --type go`: streams outside
  `lith/live` lack the tick, the frame dropping and the window settings;
  `WithBrotli` without `WithBrotliLGWin` keeps a 4 MiB window (9.9 MiB of heap
  per stream measured).
- Every stream counts against a cap, every network write has a deadline, and
  shutdown cancels stream contexts. A slot or lock held across a network
  write lets a few stalled clients stop every stream.
- `rg -n 'live\.Spec\{' --type go`: a public stream sets `Client:
  edge.ClientKey(r)`, or one address holds every place under `MaxStreams`
  with idle streams, and keeps the budget's compressed tiers while later
  pages go out uncompressed (`Options.MaxStreamsPerClient`, 16).
- An action that stores tab state checks that the tab's stream is open
  (`live.Hub.HasStream`, or the session that opened it) before the command:
  otherwise each made-up tab id is a row.
- `rg -n "retry: '(always|auto)'|@(get|post)\('[^']*(stream|sse|live)" -g '*.templ' -g '*.go'`:
  a page's stream opens with `retry: 'error'` from an element whose id is the
  stream's `Spec.Reconnect`. `'auto'` (the default) leaves pages stale after
  a deploy; `'always'` also reopens streams the server ended on purpose.
- `rg -n 'BaseContext' --type go`: the server's BaseContext must not end
  with the signal. Request contexts would end before the hub stops, the
  streams would end without sse.Reconnect, and Chrome takes a cut stream for
  a clean end, so even retry 'error' does not reopen it. Stop the hub first;
  give the server `context.WithoutCancel`.
- Tab state lives in SQLite, not in a map in memory: `rg -n
  'map\[string\]\*?[tT]ab' --type go`. A deploy loses the map.
- Does every view key contain everything its Render filters by? A Render
  that filters by the person, a role or a filter the page sent, under a
  key without it, leaks: views with equal keys are one view, rendered by
  the definition the first watcher gave, so everyone else on the route gets
  that person's data. In a shared database the key carries the person
  (`pages:<space>:person:<id>`), never the route alone.
- Every view names its topics, and every command touches the topics it
  changes. A command that touches nothing leaves pages stale; a view that
  listens to `all` re-renders on every commit, and so does one on a level
  above many topics, such as `board` above every `board:<id>`. An id from
  outside the app goes into a topic through `live.Topic`.

## Schema

- `rg -n 'uuid\.(New|NewString)|ulid\.Make' --type go` and `rg -n 'TEXT
  PRIMARY KEY' -g '*.sql'`: random keys scatter inserts over the B-tree
  (Anders measured UUIDv4 keys 14 to 16 times slower at 10 million rows).
  Prefer `INTEGER PRIMARY KEY`; time-ordered ids are acceptable; random
  tokens go on small tables, and a bulk insert sorts its batch by key first.
- Every table that grows has a retention rule and a sweeper that is actually
  started.

## Operations

- Backups: Litestream with a restore check, or `VACUUM INTO` on a timer. A `cp`
  of a WAL database is not a backup.
- One owner for checkpoints: Litestream when it is attached, else the writer.
- Migrations: numbered files, `user_version`, one transaction each; two trees
  must never reuse a number for different contents.
