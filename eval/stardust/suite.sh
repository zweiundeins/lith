#!/usr/bin/env bash
# Runs every benchmark three times, interleaving the configurations, pinned
# to the performance cores. Output: one JSON line per run in runs/.
set -euo pipefail
cd "$(dirname "$0")"
ulimit -s 8192
out=runs/suite-$(date -u +%Y%m%d-%H%M).jsonl
pin="taskset -c 0-11"
sd="env LD_PRELOAD=$PWD/trace/syncount.so $pin ./bin/sdbench"
sq="$pin ./bin/sqlbench"
w="-warmup 1s -duration 5s"
echo "{\"uptime_before\":\"$(uptime)\"}" >> "$out"
one() {
	local run=$1; shift
	local load; load=$(cut -d' ' -f1-3 /proc/loadavg)
	local res; res=$(timeout -s QUIT 300 "$@" 2>>runs/stderr.log | tail -1) || res=null
	res=${res:-null}
	echo "{\"run\":$run,\"load\":\"$load\",\"cmd\":\"${*: -12}\",\"result\":$res}" >> "$out"
}
for run in 1 2 3; do
	for p in 1 50 200; do
		one $run $sd -bench write -mode alone -producers $p $w
		one $run $sq -bench write -sync FULL -producers $p $w
		one $run $sd -bench write -mode batched -producers $p $w
		one $run $sq -bench write -sync NORMAL -producers $p $w
		one $run $sd -bench write -mode batched -batchread board -producers $p $w
		one $run $sq -bench write -sync FULL -maxbatch 1 -producers $p $w
	done
	one $run $sd -bench read -iters 1000
	one $run $sq -bench read -iters 1000
	one $run $sd -bench sub -iters 100
	one $run $sq -bench sub -sync FULL -iters 100
	one $run $sq -bench sub -sync NORMAL -iters 100
done
one 1 $sd -bench subscale -subs 200
echo "{\"uptime_after\":\"$(uptime)\"}" >> "$out"
echo "$out"
