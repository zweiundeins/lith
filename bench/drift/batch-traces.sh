#!/usr/bin/env bash
# The traces of TestLifeDrift: 15x CPU behind Caddy over HTTP/2 without
# the plugin (a), with it but without __reopen (b), with __reopen (c),
# and at full speed with __reopen (d) and without the plugin (d0). One
# after another, never in parallel.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
T="LIFE_DRIFT_TRACE=1 LIFE_DRIFT_TRACE_AT=30s LIFE_DRIFT_TRACE_FOR=20s LIFE_DRIFT_FOR=80s"
./run.sh trace-a-off LIFE_DRIFT=15 LIFE_DRIFT_PLUGIN=off $C $T
./run.sh trace-b-noreopen LIFE_DRIFT=15 LIFE_DRIFT_PLUGIN=noreopen $C $T
./run.sh trace-c-reopen LIFE_DRIFT=15 LIFE_DRIFT_PLUGIN=reopen $C $T
./run.sh trace-d-fullspeed LIFE_DRIFT=1 LIFE_DRIFT_PLUGIN=reopen $C $T
./run.sh trace-d0-fullspeed-off LIFE_DRIFT=1 LIFE_DRIFT_PLUGIN=off $C $T
