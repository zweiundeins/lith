#!/usr/bin/env bash
# Ack pacing with W=1 under DevTools' network emulation instead of the
# delaying link, to see whether its latency delays a stream's frames.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
for ms in 100 300; do
	./run.sh cdp${ms}ms-ack1 LIFE_DRIFT=1 LIFE_DRIFT_FOR=60s LIFE_DRIFT_CDP_LATENCY=$ms $C LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1
done
