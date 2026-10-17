#!/usr/bin/env bash
# Every markup, with and without the counters in the frames, at 15x CPU
# behind Caddy over HTTP/2 without the plugin, 150 s, three rounds, the
# configurations in turn within a round.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
F="LIFE_DRIFT=15 LIFE_DRIFT_FOR=150s LIFE_DRIFT_PLUGIN=off"
for r in 1 2 3; do
	for m in cells lean svg canvas; do
		./run.sh cmp-$m-$r $F $C LIFE_DRIFT_RENDER=$m
		./run.sh cmp-$m-nostats-$r $F $C LIFE_DRIFT_RENDER=$m LIFE_DRIFT_STATS=off
	done
done
