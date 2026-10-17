#!/usr/bin/env bash
# Many viewers without browsers (live's TestHybridLoad): 200 and 1,000
# streams, a tenth of them slow, acknowledgements straight to the hub,
# then through HTTP, three rounds each; then once with Game of Life
# frames, which compress worse. Under the lock, since it takes most of
# the machine.
set -euo pipefail
cd "$(dirname "$0")/../.."
run() {
	local name=$1
	shift
	{
		echo "# $(date -Is) $(git rev-parse --short HEAD) $(uptime)"
		echo "# $*"
		env "$@" go test ./live -run '^TestHybridLoad$' -v -count=1 -timeout 60m 2>&1 || true
		echo "# $(date -Is) $(uptime)"
	} >"bench/hybrid/$name.log"
}
flock /tmp/lith-bench.lock bash -c "$(declare -f run); run load-direct LIVE_HYBRID_LOAD=200,1000 LIVE_HYBRID_LOAD_ROUNDS=3"
flock /tmp/lith-bench.lock bash -c "$(declare -f run); run load-http LIVE_HYBRID_LOAD=200,1000 LIVE_HYBRID_LOAD_HTTP=1 LIVE_HYBRID_LOAD_ROUNDS=3"
flock /tmp/lith-bench.lock bash -c "$(declare -f run); run load-life-direct LIVE_HYBRID_LOAD=200,1000 LIVE_HYBRID_LOAD_FRAME=life"
flock /tmp/lith-bench.lock bash -c "$(declare -f run); run load-life-http LIVE_HYBRID_LOAD=200,1000 LIVE_HYBRID_LOAD_FRAME=life LIVE_HYBRID_LOAD_HTTP=1"
