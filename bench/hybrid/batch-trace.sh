#!/usr/bin/env bash
# One trace around the switch: the page starts at full speed, is slowed
# 15 times at 12 s, falls behind, and asks for pacing. The trace covers
# 5 s to 25 s.
set -euo pipefail
cd "$(dirname "$0")"
V=${1:-inplace}
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
P="LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint LIFE_DRIFT_RELEASE=5s"
flock /tmp/lith-bench.lock ./run.sh trace-hybrid-$V LIFE_DRIFT=1 LIFE_DRIFT_THEN=15@12s LIFE_DRIFT_FOR=40s $C $P LIFE_DRIFT_HYBRID=$V \
	LIFE_DRIFT_TRACE=1 LIFE_DRIFT_TRACE_AT=5s LIFE_DRIFT_TRACE_FOR=20s
