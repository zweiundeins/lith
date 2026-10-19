#!/usr/bin/env bash
# run.sh NAME [VAR=value ...] runs TestLifeDrift once with the variables
# given and keeps what it measured in this directory: NAME.log (the
# test output, with the load average before and after), NAME.csv and
# NAME.lag.csv (LIFE_DRIFT_OUT), and NAME.trace.json when the variables
# ask for a trace, gzipped when it is larger than 20 MB.
set -euo pipefail
dir=$(cd "$(dirname "$0")" && pwd)
name=$1
shift
cd "$dir/../../site"
env_args=("$@")
trace=""
for a in "${env_args[@]}"; do
	[[ $a == LIFE_DRIFT_TRACE=1 ]] && trace="$dir/$name.trace.json"
done
if [[ -n $trace ]]; then
	env_args=("${env_args[@]/LIFE_DRIFT_TRACE=1/LIFE_DRIFT_TRACE=$trace}")
fi
{
	echo "# $(date -Is) $(git rev-parse --short HEAD) $(uptime)"
	echo "# $*"
	env LITH_CHROME_NO_SANDBOX=1 LIFE_DRIFT_OUT="$dir/$name.csv" "${env_args[@]}" \
		go test ./internal/app -run "^TestLifeDrift\$" -v -count=1 -timeout 60m 2>&1 || true
	echo "# $(date -Is) $(uptime)"
} >"$dir/$name.log"
if [[ -n $trace && -f $trace ]] && (($(stat -c %s "$trace") > 20000000)); then
	gzip -f "$trace"
fi
