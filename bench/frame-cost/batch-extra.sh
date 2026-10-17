#!/usr/bin/env bash
# Between the steps of batch-break.sh: the slowdowns that place each
# markup's break more closely, and where svg breaks above 50x. Then
# whether newest-frame.js helps the canvas where it falls behind, and a
# trace of svg at 50x for its paints.
set -euo pipefail
cd "$(dirname "$0")"
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
B="LIFE_DRIFT_FOR=60s LIFE_DRIFT_PLUGIN=off"
for r in 6 8; do ./run.sh break-cells-${r}x LIFE_DRIFT=$r $B LIFE_DRIFT_RENDER=cells $C; done
./run.sh break-lean-12x LIFE_DRIFT=12 $B LIFE_DRIFT_RENDER=lean $C
./run.sh break-canvas-40x LIFE_DRIFT=40 $B LIFE_DRIFT_RENDER=canvas $C
for r in 80 120; do ./run.sh break-svg-${r}x LIFE_DRIFT=$r $B LIFE_DRIFT_RENDER=svg $C; done
for p in noreopen reopen; do
	./run.sh plugin-canvas-50x-$p LIFE_DRIFT=50 LIFE_DRIFT_FOR=60s LIFE_DRIFT_PLUGIN=$p LIFE_DRIFT_RENDER=canvas $C
done
./run.sh trace-svg-50x LIFE_DRIFT=50 LIFE_DRIFT_RENDER=svg LIFE_DRIFT_TRACE=1 LIFE_DRIFT_TRACE_AT=30s LIFE_DRIFT_TRACE_FOR=20s LIFE_DRIFT_FOR=55s LIFE_DRIFT_PLUGIN=off $C
