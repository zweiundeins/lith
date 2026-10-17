#!/usr/bin/env bash
# The terminal (LIFE_DRIFT_RENDER=term and termruns, frames without the
# counters): a picture at full speed, then where each falls behind
# without a fix, 60 s at each CPU slowdown.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
for m in term termruns; do
	./run.sh smoke/$m LIFE_DRIFT=1 LIFE_DRIFT_FOR=15s LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_RENDER=$m LIFE_DRIFT_STATS=off LIFE_DRIFT_SHOT=$PWD/smoke/$m.png $C
done
for m in term termruns; do
	for rate in ${RATES:-4 10 15 20 30}; do
		./run.sh break-$m-${rate}x LIFE_DRIFT=$rate LIFE_DRIFT_FOR=60s LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_RENDER=$m LIFE_DRIFT_STATS=off $C
	done
done
