# Benchmark results

Numbers measured on this project. Each one carries its date, the machine, the settings and the exact command that produced it, so a later run can be compared with it. Measure again before quoting a number for another machine.

## Machine

Date, CPU and cores, memory, disk and filesystem, OS, Go version, modernc.org/sqlite version.

## Writer

Commands per second and commands per transaction with 1, 50 and 200 producers, with `synchronous` FULL and NORMAL.

## Render loop

Tick duration (p50, p95, p99, max), renders per tick, overruns.

## Streams

Bytes per frame on the wire, heap per open stream, CPU per frame.

## End to end

Time from an action to its frame in the browser, with the number of tabs open.

## Measuring

`lith/loadtest` runs the writer and the render loop in process, without HTTP: N streams watch the views while M producers send commands, and it reports tick percentiles, renders per tick, bytes per frame and heap per stream. Run it before and after a change and record both runs here; `go doc github.com/zweiundeins/lith/loadtest` has its API. In production the writer's `Stats()` and the hub's overrun log (n, p50, p95, p99 and max of the ticks over 20 seconds) show the same quantities. For the whole path, drive the binary from `go tool task build` with a browser or a load tool on other cores (`taskset`) or another machine. The lith skill's `references/performance.md` has the numbers measured on the other projects and the settings they led to.
