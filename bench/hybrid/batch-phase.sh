#!/usr/bin/env bash
# A page slow only for a while: 15x for 60 s, then 1x for 60 s (and in
# the -relapse runs 15x again from 120 s to 180 s), the slowdown changed
# through CDP mid-run. Then the edge of keeping up, where a released
# stream could flap: 2x to 6x for 150 s.
set -euo pipefail
cd "$(dirname "$0")"
L() { flock /tmp/lith-bench.lock ./run.sh "$@"; }
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
P="LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint LIFE_DRIFT_RELEASE=5s"
F="LIFE_DRIFT=15 LIFE_DRIFT_FOR=120s LIFE_DRIFT_THEN=1@60s"
L phase-hybrid-reopen $F $C $P LIFE_DRIFT_HYBRID=reopen
L phase-hybrid-inplace $F $C $P LIFE_DRIFT_HYBRID=inplace
L phase-plugin-reopen $F $C LIFE_DRIFT_PLUGIN=reopen
F="LIFE_DRIFT=15 LIFE_DRIFT_FOR=180s LIFE_DRIFT_THEN=1@60s,15@120s"
L relapse-hybrid-reopen $F $C $P LIFE_DRIFT_HYBRID=reopen
L relapse-hybrid-inplace $F $C $P LIFE_DRIFT_HYBRID=inplace
L relapse-plugin-reopen $F $C LIFE_DRIFT_PLUGIN=reopen
for x in 2 3 4 5 6; do
	L edge${x}x-hybrid-inplace LIFE_DRIFT=$x LIFE_DRIFT_FOR=150s $C $P LIFE_DRIFT_HYBRID=inplace
done
