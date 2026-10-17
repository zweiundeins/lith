#!/usr/bin/env bash
# C: a slow phone on a far link, 15x and 100 or 300 ms, 150 s, three
# rounds; then the release with the round trip the page reports
# (LIFE_DRIFT_RELEASE_SLACK): at 15x on the far links and on the fast
# one, where it must not release, and on a page slow for 60 s, then
# fast, where it should, beside the release as it was.
set -euo pipefail
cd "$(dirname "$0")"
. ./configs.sh
for r in 1 2 3; do
	for rtt in 100ms 300ms; do
		for c in off reopen noreopen ack1paint ack2 hybrid-inplace hybrid-slack; do
			L c$rtt-$c-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=150s LIFE_DRIFT_RTT=$rtt $C $(cfg $c)
		done
	done
done
for r in 1 2 3; do
	for rtt in 100ms 300ms; do
		for c in hybrid-inplace hybrid-slack; do
			L phase$rtt-$c-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=120s LIFE_DRIFT_THEN=1@60s LIFE_DRIFT_RTT=$rtt $C $(cfg $c)
		done
	done
	L slow-hybrid-slack-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=150s $C $(cfg hybrid-slack)
	L phase-hybrid-slack-$r LIFE_DRIFT=15 LIFE_DRIFT_FOR=120s LIFE_DRIFT_THEN=1@60s $C $(cfg hybrid-slack)
done
