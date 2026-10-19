#!/usr/bin/env bash
# batch.sh runs TestLifeDrift with the site as it ships, no switch set:
# newest-frame.js without __reopen and the hybrid in place (/pace, W=1,
# acknowledged after the paint, Release and ReleaseSlack at their
# defaults). Four cases, three rounds, each run under the lock and
# skipped when its log has a summary already.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
L() {
	if grep -q "summary:" "$1.log" 2>/dev/null; then return; fi
	touch /tmp/lith-bench.lock
	flock /tmp/lith-bench.lock ./run.sh "$@"
}
for r in 1 2 3; do
	L fast15x-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=150s $C
	L rtt300ms-$r LIFE_DRIFT=1 LIFE_DRIFT_FOR=90s LIFE_DRIFT_RTT=300ms $C
	L c300ms-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=150s LIFE_DRIFT_RTT=300ms $C
	L phase300ms-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=120s LIFE_DRIFT_THEN=1@60s LIFE_DRIFT_RTT=300ms $C
done
