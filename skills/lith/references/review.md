# Review checklist

Run these against a project before a release, and on any change that touches
the database, a stream or a handler. Each line names what is wrong when the
pattern matches, and what the review of October 2026 found where.

## Writes

- `rg -n '\.(Exec|ExecContext)\(' --type go -g '!*_test.go'`: every hit must
  sit inside a writer command (`Apply(tx *writer.Tx)` or a `writer.Func`), in
  migrations, or in a test. A store method that writes on its own is a second
  writer. (libretto had 398 such sites, swisscyberguard 89.)
- `rg -n 'BeginTx\([^)]*nil\)|\.Begin\(\)' --type go`: a deferred
  transaction that reads and then writes can fail with `SQLITE_BUSY_SNAPSHOT`
  next to Litestream's connection; `busy_timeout` does not retry it.
- `rg -n 'SetMaxOpenConns\(1\)' --type go` outside lith: a pinned connection
  serialises writes but does not batch them, and reads queue behind it.
- A command that runs several autocommits (insert, then audit row, then
  publish) is not atomic. One command, one `Apply`, one savepoint.
- `rg -n 'http\.(Get|Post)|smtp\.|\.Send\(' --type go` inside commands:
  network calls do not belong in the writer. Use `tx.AfterCommit`.

## Reads

- Render code queries through the transaction it is given. A render that
  opens its own transactions (one for the list, one per open dialog) mixes
  snapshots. (kanban's card dialogs, libretto's carlist.)
- No `db.W` outside the writer and migrations; `db.R` is `query_only` and
  refuses writes, so a write there fails loudly.
- A render that replays an event stream grows with the history: render from
  the projection. (pendlerpreis.)

## Requests

- GET handlers never write: no session rows, no events, no counters per hit.
  Crawlers multiply them. (pendlerpreis created a session per `GET /`.)
- `rg -n 'r\.Header\.Get\("X-(Real-IP|Forwarded-For)"\)' --type go`: client
  IPs come from `edge`, which trusts those headers only from the local proxy.
- Every non-GET route passes the CSRF middleware; exemptions are listed.
- Every route that changes data checks authentication and membership.
  (tickit's `/api/*` routes had none.)

## Streams

- `rg -n 'datastar\.NewSSE|WithBrotli\(' --type go`: streams outside
  `lith/live` lack the tick, the frame dropping and the window settings;
  `WithBrotli` without `WithBrotliLGWin` keeps a 4 MiB window (9.9 MiB of heap
  per stream measured).
- Every stream counts against a cap, every network write has a deadline, and
  shutdown cancels stream contexts. A slot or lock held across a network
  write lets a few stalled clients stop every stream. (kanban, 2und1.)
- Tab state lives in SQLite, not in a map in memory: `rg -n
  'map\[string\]\*?[tT]ab' --type go`. A deploy loses the map.
- Every view names its topics, and every command touches the topics it
  changes. A command that touches nothing leaves pages stale; a view that
  listens to `all` re-renders on every commit.

## Schema

- `rg -n 'uuid\.(New|NewString)|ulid\.Make' --type go` and `rg -n 'TEXT
  PRIMARY KEY' -g '*.sql'`: random keys scatter inserts over the B-tree
  (Anders measured UUIDv4 keys 14 to 16 times slower at 10 million rows).
  Prefer `INTEGER PRIMARY KEY`; time-ordered ids are acceptable; random
  tokens go on small tables, and a bulk insert sorts its batch by key first.
- Every table that grows has a retention rule and a sweeper that is actually
  started. (swisscyberguard's three sweepers had no caller.)

## Operations

- Backups: Litestream with a restore check, or `VACUUM INTO` on a timer. A `cp`
  of a WAL database is not a backup.
- One owner for checkpoints: Litestream when it is attached, else the writer.
- Migrations: numbered files, `user_version`, one transaction each; two trees
  must never reuse a number for different contents (libretto and car-tco
  both have a different migration 85).
