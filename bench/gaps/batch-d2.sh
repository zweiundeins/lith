#!/usr/bin/env bash
# D, the edge: 6x and 8x, where no fix still applies every frame but
# paints fewer, two more runs each.
set -euo pipefail
cd "$(dirname "$0")"
. ./configs.sh
for r in 2 3; do
	for x in 6 8; do
		for c in off noreopen reopen ack1paint hybrid-inplace; do
			L d${x}x-$c-$r LIFE_DRIFT=$x LIFE_DRIFT_FOR=90s $C $(cfg $c)
		done
	done
done
