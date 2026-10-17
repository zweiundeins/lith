#!/usr/bin/env bash
# The terminal at 15x and where it breaks (term 15x, termruns 50x),
# 150 s each: no fix, newest-frame.js with __reopen, ack pacing after
# paint. Then a trace of each at its break without a fix, and whether
# the plugin helps svg where it breaks.
set -euo pipefail
cd "$(dirname "$0")"
# A run whose log has its summary is not run again.
run() { if [[ -f $1.log ]] && grep -q "summary:" "$1.log"; then return; fi; ./run.sh "$@"; }
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
S="LIFE_DRIFT_STATS=off LIFE_DRIFT_FOR=150s"
for mr in term:15 termruns:15 termruns:50; do
	m=${mr%:*} r=${mr#*:}
	run fix-$m-${r}x-off LIFE_DRIFT=$r LIFE_DRIFT_RENDER=$m $S $C LIFE_DRIFT_PLUGIN=off
	run fix-$m-${r}x-reopen LIFE_DRIFT=$r LIFE_DRIFT_RENDER=$m $S $C LIFE_DRIFT_PLUGIN=reopen
	run fix-$m-${r}x-ack1paint LIFE_DRIFT=$r LIFE_DRIFT_RENDER=$m $S $C LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint
done
run fix-term-10x-off LIFE_DRIFT=10 LIFE_DRIFT_RENDER=term $S $C LIFE_DRIFT_PLUGIN=off
T="LIFE_DRIFT_TRACE=1 LIFE_DRIFT_TRACE_AT=30s LIFE_DRIFT_TRACE_FOR=20s LIFE_DRIFT_FOR=55s LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_STATS=off"
run trace-term-15x LIFE_DRIFT=15 LIFE_DRIFT_RENDER=term $T $C
run trace-termruns-50x LIFE_DRIFT=50 LIFE_DRIFT_RENDER=termruns $T $C
run plugin-svg-120x-noreopen LIFE_DRIFT=120 LIFE_DRIFT_FOR=60s LIFE_DRIFT_PLUGIN=noreopen LIFE_DRIFT_RENDER=svg $C
