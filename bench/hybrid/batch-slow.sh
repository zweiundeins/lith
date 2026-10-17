#!/usr/bin/env bash
# A slow phone on a fast link: 15x CPU, Caddy over HTTP/2, 150 s, three
# rounds, the configurations in turn within a round. Each run takes
# /tmp/lith-bench.lock, so no other browser measurement runs beside it.
set -euo pipefail
cd "$(dirname "$0")"
L() { flock /tmp/lith-bench.lock ./run.sh "$@"; }
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
F="LIFE_DRIFT=15 LIFE_DRIFT_FOR=150s"
P="LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint"
for r in 1 2 3; do
	L slow-hybrid-reopen-$r $F $C $P LIFE_DRIFT_HYBRID=reopen LIFE_DRIFT_RELEASE=5s
	L slow-hybrid-inplace-$r $F $C $P LIFE_DRIFT_HYBRID=inplace LIFE_DRIFT_RELEASE=5s
	L slow-plugin-reopen-$r $F $C LIFE_DRIFT_PLUGIN=reopen
	L slow-plugin-noreopen-$r $F $C LIFE_DRIFT_PLUGIN=noreopen
	L slow-ack1paint-$r $F $C LIFE_DRIFT_PLUGIN=off $P
done
