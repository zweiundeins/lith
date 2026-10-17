#!/usr/bin/env bash
# D: moderate slowdowns, 4x to 15x on a fast link, 90 s, one run each.
set -euo pipefail
cd "$(dirname "$0")"
. ./configs.sh
for x in 4 6 8 10 15; do
	for c in off noreopen reopen ack1paint hybrid-inplace; do
		L d${x}x-$c-1 LIFE_DRIFT=$x LIFE_DRIFT_FOR=90s $C $(cfg $c)
	done
done
