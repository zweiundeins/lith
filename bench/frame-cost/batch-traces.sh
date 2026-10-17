#!/usr/bin/env bash
# One 20 s trace of each markup, with and without the counters in the
# frames, at full speed and at 15x CPU, without the plugin, behind Caddy
# over HTTP/2. One after another; run.sh takes the bench lock.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
T="LIFE_DRIFT_TRACE=1 LIFE_DRIFT_TRACE_AT=30s LIFE_DRIFT_TRACE_FOR=20s LIFE_DRIFT_FOR=55s LIFE_DRIFT_PLUGIN=off"
for rate in 1 15; do
	for m in cells lean svg canvas; do
		./run.sh trace-$m-${rate}x LIFE_DRIFT=$rate LIFE_DRIFT_RENDER=$m $C $T
		./run.sh trace-$m-nostats-${rate}x LIFE_DRIFT=$rate LIFE_DRIFT_RENDER=$m LIFE_DRIFT_STATS=off $C $T
	done
done
