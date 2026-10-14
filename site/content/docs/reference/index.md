---
title: Package reference
nav: Packages
headline: Package reference: every exported name of lith
summary: Every exported name of lith's packages, with its signature and its doc comment.
description: Every exported name of lith's packages, from sqlite and writer to live, sse and edge, with its signature and doc comment, generated from the code.
order: 6
---

These pages are generated from the Go doc comments of lith's packages, at the commit this site was built from: `go generate ./content` in `site/` writes them, and `TestUpToDate` fails when they differ from the code. To change a page, change the doc comment and generate again.

| Package | What it does |
| --- | --- |
| [sqlite](/docs/reference/sqlite) | Opens one SQLite database file the way the projects use it: a single write connection, for migrations and the writer, and a pool of read-only connections for everything else. |
| [writer](/docs/reference/writer) | The single writer of a SQLite database. |
| [live](/docs/reference/live) | Renders views from the database and streams them to pages: view = f(state). |
| [sse](/docs/reference/sse) | Writes Datastar server-sent events through a compressor of its own per stream. |
| [edge](/docs/reference/edge) | What the apps do at the request edge, before a handler runs and after it has written. |
| [tenant](/docs/reference/tenant) | Keeps one open value per key, usually a SQLite database per account. |
| [dbmetrics](/docs/reference/dbmetrics) | Times the statements a database/sql driver runs and logs the slow ones. |
| [loadtest](/docs/reference/loadtest) | Measures a project's views and commands under load, in process: a writer and a live hub on one database, streams without a network, and producers that send commands on a fixed schedule. |
| [backup](/docs/reference/backup) | Replicates SQLite databases to S3-compatible storage with Litestream, embedded in the process. |
| [prom](/docs/reference/prom) | Exports a lith app's numbers to Prometheus: the writer's batches and commands, the live hub's ticks, renders and frames, and what the streams' encoders hold of the memory budget. |
| [dbmetrics/prom](/docs/reference/dbmetrics-prom) | Records dbmetrics observations in Prometheus. |

`backup`, `prom` and `dbmetrics/prom` are modules of their own, so that an app without them never pulls in their dependencies: Litestream and the AWS SDK for `backup`, the Prometheus client for the other two. The rest are packages of the module `github.com/zweiundeins/lith`, imported one by one.
