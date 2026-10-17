# Stardust evaluation

Whether [Stardust](https://stardustdb.com), the Datastar team's embedded
temporal database, could serve as an optional store for lith. The findings,
the numbers and the issues reported upstream are in the Claude doc "Stardust as
an optional store for lith". This directory holds the code behind them: the
kanban workload on both stores, the probes, the upstream reproductions and the
raw results. It is a module of its own and not part of lith.

## Setup

- The Go binding ships in Stardust's release archive, not on a module proxy.
  Unpack `stardust-alpha-2610171653.zip` to `/tmp/stardust-release`,
  or point the `replace stardust` line in `go.mod` at the unpacked
  `abi/bindings/go`.
- Since alpha-2610171653 a nil `Options.Plugins` enables no plugins. The
  commands pass `sdx.DefaultPlugins()`, history and reactors, which is what
  the first alpha enabled and what the results in `runs/` were measured with.
- Put the licence token in `license.token` here (it is git-ignored). Every
  command reads it from there, or from its `-token` flag.
- Build with cgo: `go build -o bin/ ./cmd/...`.
- The suites preload `trace/syncount.so`, which counts fsync calls:
  `gcc -shared -fPIC -O2 -o trace/syncount.so trace/syncount.c -ldl`
  (`trace/interpose.c` logs the libc calls behind a blocked commit, built the
  same way).
- Run with `ulimit -s 8192`: an unlimited stack makes Stardust's first write
  block (`upstream/stackhang`).

## Contents

| Path | What it does |
| --- | --- |
| `internal/kanban` | the workload both stores run: one board of 200 cards |
| `internal/sdx` | the Stardust plumbing the commands share |
| `cmd/sdbench`, `cmd/sqlbench` | the kanban workload on Stardust, and through lith on SQLite |
| `cmd/reactors` | live mutations (the reactors plugin) under the same load |
| `cmd/subcheck`, `cmd/sharecheck` | which commits wake a subscription, and whether identical subscriptions share their work |
| `cmd/txcheck`, `cmd/sdgrow`, `cmd/probe` | how a query reaches a fact's transaction, how the file grows, the behaviour the benchmark relies on |
| `cmd/hang`, `cmd/pthreadcheck`, `cmd/minisig` | where a first commit blocks, whether the sandbox lets C create threads, the release signature |
| `upstream/stackhang`, `upstream/timetick` | reproductions of two of the issues reported upstream |
| `suite.sh`, `reactors-suite.sh`, `aggregate.py` | the benchmark runs and their summary |
| `runs/` | the raw results, one JSON line per run |
