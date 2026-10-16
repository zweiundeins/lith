#!/usr/bin/env bash
# The stream's Cache-Control: no-store (what lith sends), no-cache and
# none, without the plugin. Chromium at 15x CPU behind Caddy (HTTP/2) and
# without it (HTTP/1.1), two rounds, each run with a 15 s trace; then
# Firefox, which has no CPU throttling, with each board frame costing
# 150 ms more, and Chromium the same way for comparison. 120 s per run,
# one at a time.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
T="LIFE_DRIFT_TRACE=1 LIFE_DRIFT_TRACE_AT=60s LIFE_DRIFT_TRACE_FOR=15s"
FF="LITH_BROWSER=firefox FIREFOX=${FIREFOX:-$HOME/.cache/lith-drift/firefox/firefox}"
if [[ ${1:-} != firefox ]]; then
	for r in 1 2; do
		for cc in no-store no-cache none; do
			./run.sh cache-h2-$cc-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=120s LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_CACHE=$cc $C $T
			./run.sh cache-h1-$cc-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=120s LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_CACHE=$cc $T
		done
	done
fi
if [[ ${1:-} != chromium ]]; then
	for cc in no-store no-cache none; do
		./run.sh cache-ff-h2-$cc $FF LIFE_DRIFT=1 LIFE_DRIFT_COST=150 LIFE_DRIFT_FOR=120s LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_CACHE=$cc $C
		./run.sh cache-ff-h1-$cc $FF LIFE_DRIFT=1 LIFE_DRIFT_COST=150 LIFE_DRIFT_FOR=120s LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_CACHE=$cc
	done
	./run.sh cache-cost-h2-no-store LIFE_DRIFT=1 LIFE_DRIFT_COST=150 LIFE_DRIFT_FOR=120s LIFE_DRIFT_PLUGIN=off $C
fi
