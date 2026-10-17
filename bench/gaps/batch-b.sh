#!/usr/bin/env bash
# B: how fast the page answers input at 15x on a fast link, 150 s, three
# rounds: Shift every 500 ms (LIFE_DRIFT_INPUT).
set -euo pipefail
cd "$(dirname "$0")"
. ./configs.sh
for r in 1 2 3; do
	for c in off noreopen reopen ack1paint hybrid-inplace; do
		L input-$c-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=150s LIFE_DRIFT_INPUT=1 $C $(cfg $c)
	done
done
