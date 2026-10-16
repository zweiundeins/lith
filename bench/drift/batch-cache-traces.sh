#!/usr/bin/env bash
# Traces of the stream's Cache-Control variants from before the page
# loads: a renderer that is busy when a trace starts mid-run may never
# join it, which it did not with no-cache and without the header. 15x
# CPU, no plugin, 60 s of trace, HTTP/2 behind Caddy and HTTP/1.1.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
T="LIFE_DRIFT_TRACE=1 LIFE_DRIFT_TRACE_AT=0s LIFE_DRIFT_TRACE_FOR=60s LIFE_DRIFT_FOR=65s"
for cc in no-store no-cache none; do
	./run.sh cachetrace-h2-$cc LIFE_DRIFT=15 LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_CACHE=$cc $C $T
	./run.sh cachetrace-h1-$cc LIFE_DRIFT=15 LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_CACHE=$cc $T
done
