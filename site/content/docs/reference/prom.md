---
title: prom
headline: lith/prom: Prometheus metrics for a lith app
summary: Exports a lith app's numbers to Prometheus: the writer's batches and commands, the live hub's ticks, renders and frames, and what the streams' encoders hold of the memory budget.
description: Reference for lith/prom: export the writer's batches and commands, the live hub's ticks, renders and frames, and the stream budget to Prometheus.
order: 10
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

New returns a collector of the sources' numbers, named under namespace: \<namespace>\_writer\_\*, \<namespace>\_live\_\* and \<namespace>\_sse\_budget\_\*. It reads the sources' Stats on every scrape.

## Types

### type Sources {#Sources}

```go
type Sources struct {
	Writer *writer.Writer
	Hub    *live.Hub
	// Budget defaults to the hub's (live.Hub.Budget).
	Budget *sse.Budget
}
```

Sources are what the collector reads; a nil source exports nothing.
