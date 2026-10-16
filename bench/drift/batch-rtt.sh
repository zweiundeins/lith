#!/usr/bin/env bash
# Task 2: a fast CPU on a link far away. No CPU throttling, Caddy over
# HTTP/2, a TCP link that holds each chunk for half the round trip time
# (LIFE_DRIFT_RTT), 90 s per run, one at a time.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
for rtt in 100ms 300ms; do
	F="LIFE_DRIFT=1 LIFE_DRIFT_FOR=90s LIFE_DRIFT_RTT=$rtt"
	./run.sh rtt$rtt-off $F $C LIFE_DRIFT_PLUGIN=off
	./run.sh rtt$rtt-reopen $F $C LIFE_DRIFT_PLUGIN=reopen
	./run.sh rtt$rtt-ack1 $F $C LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1
	./run.sh rtt$rtt-ack2 $F $C LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=2
	./run.sh rtt$rtt-ack1paint $F $C LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint
done
