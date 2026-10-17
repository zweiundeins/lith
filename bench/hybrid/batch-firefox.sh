#!/usr/bin/env bash
# Firefox, which cannot slow its CPU: each board frame costs the page
# 150 ms more (LIFE_DRIFT_COST), Caddy over HTTP/2, 150 s, one run each.
set -euo pipefail
cd "$(dirname "$0")"
L() { flock /tmp/lith-bench.lock ./run.sh "$@"; }
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
F="LITH_BROWSER=firefox LIFE_DRIFT=1 LIFE_DRIFT_COST=150 LIFE_DRIFT_FOR=150s"
P="LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint LIFE_DRIFT_RELEASE=5s"
L ff-hybrid-inplace $F $C $P LIFE_DRIFT_HYBRID=inplace
L ff-hybrid-reopen $F $C $P LIFE_DRIFT_HYBRID=reopen
L ff-plugin-reopen $F $C LIFE_DRIFT_PLUGIN=reopen
