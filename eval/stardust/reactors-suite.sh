#!/usr/bin/env bash
# Write throughput with and without live mutations, 1 and 200 producers,
# three interleaved runs each, pinned to the performance cores.
set -uo pipefail
cd "$(dirname "$0")"
ulimit -s 8192
out=runs/reactors-$(date -u +%Y%m%d-%H%M).jsonl
echo "{\"uptime_before\":\"$(uptime)\"}" >> "$out"
for run in 1 2 3; do
	for p in 1 200; do
		for r in none count done; do
			load=$(cut -d' ' -f1-3 /proc/loadavg)
			res=$(LD_PRELOAD=$PWD/trace/syncount.so taskset -c 0-11 timeout -s QUIT 300 ./bin/reactors -bench write -reactor $r -producers $p -warmup 1s -duration 5s 2>>runs/reactors-stderr.log | tail -1)
			echo "{\"run\":$run,\"load\":\"$load\",\"result\":${res:-null}}" >> "$out"
		done
	done
done
echo "{\"uptime_after\":\"$(uptime)\"}" >> "$out"
echo "$out"
