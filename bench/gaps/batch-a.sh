#!/usr/bin/env bash
# A: the matrix gaps, and three runs where there was one. A far link
# (no throttling, 90 s, every configuration at 100 and 300 ms), a page
# slow for 60 s and then fast (the page without a fix 180 s, to see its
# backlog drain), the load test with the slow stream, Firefox.
set -euo pipefail
cd "$(dirname "$0")"
. ./configs.sh
for r in 1 2 3; do
	for rtt in 100ms 300ms; do
		for c in off noreopen reopen ack1 ack2 ack1paint hybrid-reopen hybrid-inplace; do
			L rtt$rtt-$c-$r LIFE_DRIFT=1 LIFE_DRIFT_FOR=90s LIFE_DRIFT_RTT=$rtt $C $(cfg $c)
		done
	done
done
for r in 1 2 3; do
	for c in noreopen off ack1paint hybrid-reopen hybrid-inplace reopen; do
		span=120s
		[[ $c == off ]] && span=180s
		L phase-$c-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=$span LIFE_DRIFT_THEN=1@60s $C $(cfg $c)
	done
done
./batch-load.sh
F="LITH_BROWSER=firefox FIREFOX=${FIREFOX:-$HOME/.cache/lith-drift/firefox/firefox} LIFE_DRIFT=1 LIFE_DRIFT_COST=150 LIFE_DRIFT_FOR=150s"
for r in 1 2 3; do
	for c in hybrid-inplace reopen off; do
		L ff-$c-$r $F $C $(cfg $c)
	done
done
