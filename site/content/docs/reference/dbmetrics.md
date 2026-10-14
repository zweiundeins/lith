---
title: dbmetrics
headline: lith/dbmetrics: statement timings and slow queries
summary: Times the statements a database/sql driver runs and logs the slow ones.
description: Reference for lith/dbmetrics: wrap a database/sql driver to time every query and exec, and log the slow ones with their SQL but never their arguments.
order: 7
---

Import path `github.com/zweiundeins/lith/dbmetrics`. Generated from the doc comments in [dbmetrics/](https://github.com/zweiundeins/lith/tree/main/dbmetrics).

Wrap a connector (sql.OpenDB) or a driver (sql.Register): each query and exec goes to an Observer and, at or above the slow threshold, to the log with its SQL text, never its arguments. A query's time is the time spent in the driver, in the call and in every Next, not the caller's work between rows. The Prometheus Observer is the nested module dbmetrics/prom, so this package pulls in no metrics library.

## Constants

```go
const (
	OpQuery = "query"
	OpExec  = "exec"
)
```

The op an Observer receives: a statement that returns rows, or one that does not.

```go
const DefaultSlowThreshold = 100 * time.Millisecond
```

DefaultSlowThreshold is the slow threshold when Options leaves it zero.

## Functions

### func Register {#Register}

```go
func Register(name, base string, opts Options) error
```

Register registers under name the driver registered as base, wrapped. It fails if name is taken or base is unknown. sqlite.Options.Driver opens a lith database through it.

### func Wrap {#Wrap}

```go
func Wrap(d driver.Driver, opts Options) driver.Driver
```

Wrap returns a driver whose connections time their statements.

### func WrapConnector {#WrapConnector}

```go
func WrapConnector(c driver.Connector, opts Options) driver.Connector
```

WrapConnector returns a connector whose connections time their statements:

```text
c, err := sqlite.NewConnector(dsn)
...
db := sql.OpenDB(dbmetrics.WrapConnector(c, opts))
```

## Types

### type Observer {#Observer}

```go
type Observer interface {
	Observe(op string, elapsed time.Duration, slow bool)
}
```

Observer receives the duration of every statement. slow reports whether it reached the slow threshold. Observe must be safe for concurrent use.

### type Options {#Options}

```go
type Options struct {
	// Observer receives every statement; nil records nothing but the slow log.
	Observer Observer
	// SlowThreshold is the duration from which a statement is slow: logged at
	// Warn and flagged to the Observer. Zero means DefaultSlowThreshold, a
	// negative value turns the slow log off.
	SlowThreshold time.Duration
	// Logger receives the slow log; nil means slog.Default().
	Logger *slog.Logger
}
```

Options configure a wrapped driver or connector.
