#!/usr/bin/env bash
# A fast phone on a far link: no throttling, Caddy over HTTP/2, the
# harness's delaying proxy, 90 s, one run each.
set -euo pipefail
cd "$(dirname "$0")"
L() { flock /tmp/lith-bench.lock ./run.sh "$@"; }
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
P="LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint"
for rtt in 100ms 300ms; do
	F="LIFE_DRIFT=1 LIFE_DRIFT_FOR=90s LIFE_DRIFT_RTT=$rtt"
	L rtt$rtt-hybrid-reopen $F $C $P LIFE_DRIFT_HYBRID=reopen LIFE_DRIFT_RELEASE=5s
	L rtt$rtt-hybrid-inplace $F $C $P LIFE_DRIFT_HYBRID=inplace LIFE_DRIFT_RELEASE=5s
	L rtt$rtt-plugin-reopen $F $C LIFE_DRIFT_PLUGIN=reopen
	L rtt$rtt-ack1paint $F $C LIFE_DRIFT_PLUGIN=off $P
done
