---
title: prom
headline: lith/prom: Prometheus metrics for a lith app
summary: Exports a lith app's numbers to Prometheus: the writer's batches and commands, the live hub's ticks, renders and frames, what the streams hold of the memory budget, and the event store's appends and projectors.
description: Reference for lith/prom: export the writer's commands, the live hub's ticks, renders and frames, the stream budget and the event store to Prometheus.
order: 12
---

Import path `github.com/zweiundeins/lith/prom`, a module of its own: `go get github.com/zweiundeins/lith/prom`. Generated from the doc comments in [prom/](https://github.com/zweiundeins/lith/tree/main/prom).

It is a module of its own, so that projects without Prometheus never pull it in. Statement timings come from dbmetrics/prom.

```go
reg := prometheus.NewRegistry()
reg.MustRegister(prom.New("myapp", prom.Sources{Writer: w, Hub: hub}))
```

## Functions

### func New {#New}

```go
func New(namespace string, s Sources) prometheus.Collector
```

New returns a collector of the sources' numbers, named under namespace: \<namespace>\_writer\_\*, \<namespace>\_live\_\*, \<namespace>\_sse\_budget\_\* and \<namespace>\_events\_\*. It reads the sources' Stats on every scrape.

## Types

### type Sources {#Sources}

```go
type Sources struct {
	Writer *writer.Writer
	Hub    *live.Hub
	// Budget defaults to the hub's (live.Hub.Budget).
	Budget *sse.Budget
	// Events is the runner of the app's projectors in its database, and
	// Store the event store, whose counters cover every database it
	// serves.
	Events *events.Runner
	Store  *events.Store
}
```

Sources are what the collector reads; a nil source exports nothing.
