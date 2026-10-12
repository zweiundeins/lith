// Package lith is the shared runtime of the Go, SQLite and Datastar
// projects: one writer for SQLite that batches commands, a read pool,
// a render loop that renders each changed view once per tick, and SSE
// streams that send the newest frame through their own compressor.
//
// The packages are used together but imported one by one:
//
//	sqlite    open one database file as a write connection and a read pool
//	writer    the single writer: Send, Do, batches with a savepoint per command
//	live      topics, the tick, render workers, shared views, stream mailboxes
//	sse       Datastar events and per-stream compression
//	edge      request edge: sessions, CSRF, client IP, CSP, compression, limits
//	loadtest  in-process load tests without HTTP
//	tenant    one open value per key (a database per account), evicted at a soft cap
//	dbmetrics statement timing and the slow-query log, reported to an Observer
//
// Two are modules of their own, so that projects without them never pull in
// their dependencies:
//
//	backup          Litestream replication to S3: restore, attach, verify, retention
//	dbmetrics/prom  the Prometheus Observer for dbmetrics
package lith
