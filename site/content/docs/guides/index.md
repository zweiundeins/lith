---
title: Guides
headline: lith guides: one task per page, with real code
summary: One task per page, each with code that compiles against the lith of this site's commit and runs in its tests.
description: One task per page, from commands and reads to streams, backups and deploys, each with code that compiles against this site's lith and runs in its tests.
order: 3
---

The guides follow one app: boards of lanes that hold cards, the domain of lith's own benchmarks. Its code is in [`site/internal/docs/example`](https://github.com/zweiundeins/lith/tree/main/site/internal/docs/example) in lith's repository, compiled and tested with the site: `board` holds the commands, the queries and the schema, `web` the handlers, the views and the page, `tenants` a pool of databases, and `backup` the Litestream wiring, in a module of its own. A listing with a file name above it is a piece of that code, of lith's template or of this site, and the docs' tests fail when the file no longer holds it.

| Guide | What it covers |
| --- | --- |
| [Commands and the writer](/docs/guides/commands) | `Send`, `Do`, `DoValue`, validation, topics, refusals, op ids with `Once` and `OnceResult`, `AfterCommit`, the statement cache |
| [Event sourcing](/docs/guides/events) | streams and event types, deciders with `Execute`, expected versions, inline and catch-up projectors, rebuilds, upcasting, retention, `eventstest` |
| [Migrations, jobs and imports](/docs/guides/migrations-and-jobs) | the schema's files, `Exclusive`, bulk imports, background jobs, sweeps, checkpoints |
| [Reads](/docs/guides/reads) | `View` and `ViewValue`, query code shared through `sqlite.Querier`, `WithTx`, the read pool |
| [Views and the hub](/docs/guides/views) | keys and topics, `Presence`, `Every`, `Watch` and `Unwatch`, `Spec.WriteTick`, the heartbeat signal, `MaxStreams` |
| [Pages and Datastar](/docs/guides/pages) | the document, the fat morph, tab state in SQLite, op ids and pending, reconnecting after a deploy |
| [Streams and compression](/docs/guides/streams) | zstd and brotli, windows, the memory budget, hibernation, `NoCompression`, slow links |
| [The request edge](/docs/guides/edge) | sessions, CSRF, the CSP and its nonce, client addresses, slow links, rate limits, compression, static files, health |
| [One database per account](/docs/guides/tenants) | `tenant.Pool`, a writer per account, views across accounts, erasure |
| [Backups](/docs/guides/backups) | `VACUUM INTO` copies, Litestream with the `backup` module, restores |
| [Metrics and monitoring](/docs/guides/monitoring) | `lith/prom`, `dbmetrics`, the Grafana dashboard, alerts, logs, probes, the files each app installs on a shared host |
| [Deploying](/docs/guides/deploying) | the systemd unit, Caddy, the deploy script with rollback, memory settings, the host's folders |
| [Load testing](/docs/guides/load-testing) | `loadtest.Run` with an app's own views and commands, and how to read its report |

[The first command and view](/docs/getting-started/first-view) builds a smaller feature from scratch, and the [package reference](/docs/reference) has every exported name.
