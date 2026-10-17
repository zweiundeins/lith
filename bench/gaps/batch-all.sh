#!/usr/bin/env bash
# Everything in order: A, B, C, D.
set -uo pipefail
cd "$(dirname "$0")"
for b in a b c d; do
	echo "$(date -Is) batch $b"
	./batch-$b.sh
done
echo "$(date -Is) done"
