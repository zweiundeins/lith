#!/usr/bin/env bash
# Task 2: every configuration at 15x CPU behind Caddy over HTTP/2 for
# 150 s, three rounds, the configurations in turn within a round, one run
# at a time. Then a trace of the ack-paced page.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
F="LIFE_DRIFT=15 LIFE_DRIFT_FOR=150s"
for r in 1 2 3; do
	./run.sh cmp-off-$r $F $C LIFE_DRIFT_PLUGIN=off
	./run.sh cmp-noreopen-$r $F $C LIFE_DRIFT_PLUGIN=noreopen
	./run.sh cmp-reopen-$r $F $C LIFE_DRIFT_PLUGIN=reopen
	./run.sh cmp-ack1-$r $F $C LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1
	./run.sh cmp-ack2-$r $F $C LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=2
	./run.sh cmp-ack1paint-$r $F $C LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint
done
T="LIFE_DRIFT_TRACE=1 LIFE_DRIFT_TRACE_AT=30s LIFE_DRIFT_TRACE_FOR=20s LIFE_DRIFT_FOR=80s"
./run.sh trace-e-ack1 LIFE_DRIFT=15 $C $T LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1
./run.sh trace-e-ack1paint LIFE_DRIFT=15 $C $T LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint
./run.sh trace-e-ack1-fullspeed LIFE_DRIFT=1 $C $T LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1
