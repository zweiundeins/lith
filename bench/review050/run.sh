#!/bin/bash
# Alternates the base (5ddf4b9), r5 (the frame and counter commit alone)
# and new test binaries in $P, each run -count 1, ten times; run it under
# flock /tmp/lith-bench.lock.
P=${P:?the folder with the test binaries}
R=${R:-$(dirname "$0")}
cd "$P"
hub='^Benchmark(Changed|ChangedBelow|Tick|TickDepends)$'
for i in $(seq 10); do
  for b in base r5 new; do
    echo "# run $i $b load $(cut -d' ' -f1-3 /proc/loadavg)" >> "$R/hub-$b.txt"
    taskset -c 0-11 "$P/live.$b" -test.run '^$' -test.bench "$hub" -test.benchmem -test.count 1 >> "$R/hub-$b.txt"
  done
done
for i in $(seq 10); do
  for b in base new; do
    echo "# run $i $b load $(cut -d' ' -f1-3 /proc/loadavg)" >> "$R/sse-$b.txt"
    taskset -c 0-11 "$P/sse.$b" -test.run '^$' -test.bench 'HeartbeatAsleep' -test.benchmem -test.count 1 >> "$R/sse-$b.txt"
  done
done
for b in base new; do
  taskset -c 0-11 "$P/live.$b" -test.run '^$' -test.bench '^BenchmarkTick$/^10000$' -test.benchtime 5s -test.cpuprofile "$P/tick10000-$b.pprof" > /dev/null
done
