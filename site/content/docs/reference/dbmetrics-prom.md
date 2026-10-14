---
title: dbmetrics/prom
headline: lith/dbmetrics/prom: statement timings in Prometheus
summary: Records dbmetrics observations in Prometheus.
description: Reference for lith/dbmetrics/prom: a dbmetrics Observer that records query and exec durations and slow statements as Prometheus histograms and counters.
order: 11
---

Import path `github.com/zweiundeins/lith/dbmetrics/prom`, a module of its own: `go get github.com/zweiundeins/lith/dbmetrics/prom`. Generated from the doc comments in [dbmetrics/prom/](https://github.com/zweiundeins/lith/tree/main/dbmetrics/prom).

It is a module of its own so that projects without Prometheus never pull it in.

```go
m := prom.New("myapp")
prometheus.MustRegister(m)
db := sql.OpenDB(dbmetrics.WrapConnector(c, dbmetrics.Options{Observer: m}))
```

## Variables

```go
var DefaultBuckets = []float64{.00005, .0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}
```

DefaultBuckets span SQLite's range, from 50µs to 5s.

## Types

### type Metrics {#Metrics}

```go
type Metrics struct {
	// contains filtered or unexported fields
}
```

Metrics is a dbmetrics.Observer and a prometheus.Collector of \<namespace>\_db\_query\_duration\_seconds{op} and \<namespace>\_db\_slow\_queries\_total{op}.

#### func New {#New}

```go
func New(namespace string, buckets ...float64) *Metrics
```

New returns Metrics named under namespace, with histogram buckets in seconds (DefaultBuckets if none are given).

#### func (*Metrics) Collect {#Metrics.Collect}

```go
func (m *Metrics) Collect(ch chan<- prometheus.Metric)
```

Collect implements prometheus.Collector.

#### func (*Metrics) Describe {#Metrics.Describe}

```go
func (m *Metrics) Describe(ch chan<- *prometheus.Desc)
```

Describe implements prometheus.Collector.

#### func (*Metrics) Observe {#Metrics.Observe}

```go
func (m *Metrics) Observe(op string, elapsed time.Duration, slow bool)
```

Observe implements dbmetrics.Observer.
