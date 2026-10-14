#!/usr/bin/env bash
#
# Host setup (see README.md). Run as root on the server, from a copy of this
# folder:
#
#   bash setup-host.sh [app-deploy-key.pub]
#
# The drop-ins in observability/ go into the folders the host's Prometheus and
# Alloy load, each only where that service is installed. With the public half
# of the CI deploy key (a .pub file), it also installs /usr/local/bin/app-deploy
# and creates the `app-deploy` user. That user's only key is forced to
# `sudo -n /usr/local/bin/app-deploy -` (no shell, no forwarding), and sudo
# allows exactly that command. Rerunning replaces the drop-ins and the key.
set -euo pipefail
NAME=app
HERE=$(cd "$(dirname "$0")" && pwd)
OBS=$HERE/observability
# Alloy's HTTP server, for its reload endpoint.
ALLOY_URL=${ALLOY_URL:-http://127.0.0.1:12345}

installed() { [[ "$(systemctl show -p LoadState --value "$1" 2>/dev/null)" == loaded ]]; }

deploy_user() {
	local keyfile=$1 pubkey deploy=$NAME-deploy
	local homedir=/var/lib/$deploy
	[[ -f "$keyfile" ]] || { echo "!! no such file: $keyfile" >&2; exit 2; }
	pubkey=$(tr -d '\r' < "$keyfile" | sed '/^$/d')
	[[ "$pubkey" == ssh-* && "$pubkey" != *$'\n'* ]] || { echo "!! that does not look like one public key" >&2; exit 2; }

	install -m 0755 -o root -g root "$HERE/$deploy" "/usr/local/bin/$deploy"
	id "$deploy" >/dev/null 2>&1 ||
		useradd --system --create-home --home-dir "$homedir" --shell /bin/sh "$deploy"
	install -d -m 0700 -o "$deploy" -g "$deploy" "$homedir/.ssh"
	printf 'restrict,command="sudo -n /usr/local/bin/%s -" %s\n' "$deploy" "$pubkey" > "$homedir/.ssh/authorized_keys"
	chown "$deploy": "$homedir/.ssh/authorized_keys"
	chmod 0600 "$homedir/.ssh/authorized_keys"

	echo "$deploy ALL=(root) NOPASSWD: /usr/local/bin/$deploy -" > "/etc/sudoers.d/$deploy.tmp"
	chmod 0440 "/etc/sudoers.d/$deploy.tmp"
	visudo -cqf "/etc/sudoers.d/$deploy.tmp"
	mv -f "/etc/sudoers.d/$deploy.tmp" "/etc/sudoers.d/$deploy"
	echo ">> ready: CI can deploy as $deploy@$(hostname)"
}

# Prometheus watches both folders (file_sd), so it needs no reload.
prometheus_dropins() {
	if ! installed prometheus; then
		echo ">> no Prometheus here: no scrape target, no probes"
		return
	fi
	if [[ -f "$OBS/$NAME.metrics.yml" ]]; then
		install -D -m 0644 "$OBS/$NAME.metrics.yml" "/etc/prometheus/apps/$NAME.yml"
		echo ">> Prometheus scrapes $NAME: /etc/prometheus/apps/$NAME.yml"
	fi
	if [[ -f "$OBS/$NAME.targets.yml" ]]; then
		if grep -q 'example\.com' "$OBS/$NAME.targets.yml"; then
			echo "!! $NAME.targets.yml still probes example.com: put in the real domain" >&2
			exit 2
		fi
		install -D -m 0644 "$OBS/$NAME.targets.yml" "/etc/prometheus/targets/$NAME.yml"
		echo ">> the blackbox exporter probes $NAME: /etc/prometheus/targets/$NAME.yml"
	fi
}

# Alloy loads every .alloy file in /etc/alloy as one config, and a file it
# refuses would stop the logs of all apps at its next start: the old one goes back.
alloy_dropin() {
	local src=$OBS/$NAME.alloy dst=/etc/alloy/$NAME.alloy old out
	[[ -f "$src" ]] || return 0
	if ! installed alloy; then
		echo ">> no Alloy here: no logs to Loki"
		return
	fi
	old=$(mktemp)
	cp "$dst" "$old" 2>/dev/null || rm -f "$old"
	install -D -m 0644 "$src" "$dst"
	if ! systemctl is-active --quiet alloy; then
		echo ">> Alloy loads $dst when it starts"
	elif out=$(curl -sS --fail-with-body --max-time 30 "$ALLOY_URL/-/reload" 2>&1); then
		echo ">> Alloy ships $NAME's logs: $dst"
	else
		if [[ -f "$old" ]]; then install -m 0644 "$old" "$dst"; else rm -f "$dst"; fi
		rm -f "$old"
		curl -sS --max-time 30 "$ALLOY_URL/-/reload" >/dev/null 2>&1 || true
		echo "!! Alloy refused the new $NAME.alloy and keeps what it had: $out" >&2
		exit 1
	fi
	rm -f "$old"
}

if [[ -n "${1:-}" ]]; then
	deploy_user "$1"
fi
prometheus_dropins
alloy_dropin
