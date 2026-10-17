# configs.sh, sourced by the batch-*.sh scripts: L runs one TestLifeDrift
# under the lock, unless its log has a summary already, and cfg NAME
# prints the switches of a configuration.
L() {
	if grep -q "summary:" "$1.log" 2>/dev/null; then return; fi
	flock /tmp/lith-bench.lock ./run.sh "$@"
}
C=CADDY=${CADDY:-$HOME/.cache/lith-drift/caddy}
P="LIFE_DRIFT_ACK_AFTER=paint LIFE_DRIFT_RELEASE=5s"
cfg() {
	case $1 in
	off) echo "LIFE_DRIFT_PLUGIN=off" ;;
	noreopen) echo "LIFE_DRIFT_PLUGIN=noreopen" ;;
	reopen) echo "LIFE_DRIFT_PLUGIN=reopen" ;;
	ack1) echo "LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1" ;;
	ack2) echo "LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=2" ;;
	ack1paint) echo "LIFE_DRIFT_PLUGIN=off LIFE_DRIFT_ACK=1 LIFE_DRIFT_ACK_AFTER=paint" ;;
	hybrid-reopen) echo "LIFE_DRIFT_ACK=1 $P LIFE_DRIFT_HYBRID=reopen" ;;
	hybrid-inplace) echo "LIFE_DRIFT_ACK=1 $P LIFE_DRIFT_HYBRID=inplace" ;;
	hybrid-slack) echo "LIFE_DRIFT_ACK=1 $P LIFE_DRIFT_HYBRID=inplace LIFE_DRIFT_RELEASE_SLACK=100ms" ;;
	*) echo "unknown configuration $1" >&2; exit 1 ;;
	esac
}
