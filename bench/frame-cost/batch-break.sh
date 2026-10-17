#!/usr/bin/env bash
# Where each markup falls behind: 60 s at each CPU slowdown, without the
# plugin, behind Caddy over HTTP/2. BREAK_STATS=off runs the frames
# without the counters (names end in -nostats).
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
S="" X=""
if [[ ${BREAK_STATS:-} == off ]]; then S=LIFE_DRIFT_STATS=off X=-nostats; fi
for m in ${MARKUPS:-cells lean svg canvas}; do
	for rate in ${RATES:-4 10 15 20 30 50}; do
		./run.sh break-$m$X-${rate}x LIFE_DRIFT=$rate LIFE_DRIFT_FOR=60s LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_RENDER=$m $S $C
	done
done
