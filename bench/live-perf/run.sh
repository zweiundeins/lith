#!/bin/bash
# Alternates the base and new binaries, each run -count 1, ten times.
P=${P:?the folder with base/ and new/ test binaries}
R=$P/res
cd $P
hub='^Benchmark(Changed|ChangedBelow|Tick|TickDepends)$'
for i in $(seq 10); do
  for b in base new; do
    echo "# run $i $b load $(cut -d' ' -f1-3 /proc/loadavg)" >> $R/hub-$b.txt
    taskset -c 0-11 $P/$b/live.test -test.run '^$' -test.bench "$hub" -test.benchmem -test.count 1 >> $R/hub-$b.txt
  done
  echo "# run $i e9bebd0 load $(cut -d' ' -f1-3 /proc/loadavg)" >> $R/hub-e9bebd0.txt
  taskset -c 0-11 $P/base/live-e9bebd0.test -test.run '^$' -test.bench '^BenchmarkTickDepends$' -test.benchmem -test.count 1 >> $R/hub-e9bebd0.txt
done
for i in $(seq 10); do
  for b in base new; do
    echo "# run $i $b load $(cut -d' ' -f1-3 /proc/loadavg)" >> $R/sse-$b.txt
    taskset -c 0-11 $P/$b/sse.test -test.run '^$' -test.bench 'PatchElementsLarge|ElementsLarge|HeartbeatAsleep' -test.benchmem -test.count 1 >> $R/sse-$b.txt
  done
done
for i in $(seq 10); do
  for b in base new; do
    echo "# run $i $b load $(cut -d' ' -f1-3 /proc/loadavg)" >> $R/board-$b.txt
    taskset -c 0-11 $P/$b/loadtest.test -test.run '^$' -test.bench 'SharedBoard/FULL/zstd' -test.benchmem -test.count 1 >> $R/board-$b.txt
  done
done
echo done $(date -u) $(cut -d' ' -f1-3 /proc/loadavg) >> $R/done.txt
