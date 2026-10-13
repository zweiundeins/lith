#!/usr/bin/env bash
#
# One-time host setup for CI deploys (see README.md). Run as root on the
# server, from a copy of this folder, with the public half of the CI deploy
# key (a .pub file):
#
#   bash setup-host.sh lith-site-deploy-key.pub
#
# It installs /usr/local/bin/lith-site-deploy and creates the `lith-site-deploy`
# user. That user's only key is forced to `sudo -n /usr/local/bin/lith-site-deploy -`
# (no shell, no forwarding), and sudo allows exactly that command. Rerunning
# replaces the key.
set -euo pipefail
KEYFILE="${1:?usage: setup-host.sh <public key file>}"
[[ -f "$KEYFILE" ]] || { echo "!! no such file: $KEYFILE" >&2; exit 2; }
PUBKEY=$(tr -d '\r' < "$KEYFILE" | sed '/^$/d')
[[ "$PUBKEY" == ssh-* && "$PUBKEY" != *$'\n'* ]] || { echo "!! that does not look like one public key" >&2; exit 2; }
HERE=$(cd "$(dirname "$0")" && pwd)
DEPLOY=lith-site-deploy
HOMEDIR=/var/lib/$DEPLOY

install -m 0755 -o root -g root "$HERE/lith-site-deploy" /usr/local/bin/lith-site-deploy
id "$DEPLOY" >/dev/null 2>&1 ||
	useradd --system --create-home --home-dir "$HOMEDIR" --shell /bin/sh "$DEPLOY"
install -d -m 0700 -o "$DEPLOY" -g "$DEPLOY" "$HOMEDIR/.ssh"
printf 'restrict,command="sudo -n /usr/local/bin/lith-site-deploy -" %s\n' "$PUBKEY" > "$HOMEDIR/.ssh/authorized_keys"
chown "$DEPLOY": "$HOMEDIR/.ssh/authorized_keys"
chmod 0600 "$HOMEDIR/.ssh/authorized_keys"

echo "$DEPLOY ALL=(root) NOPASSWD: /usr/local/bin/lith-site-deploy -" > "/etc/sudoers.d/$DEPLOY.tmp"
chmod 0440 "/etc/sudoers.d/$DEPLOY.tmp"
visudo -cqf "/etc/sudoers.d/$DEPLOY.tmp"
mv -f "/etc/sudoers.d/$DEPLOY.tmp" "/etc/sudoers.d/$DEPLOY"
echo ">> ready: CI can deploy as $DEPLOY@$(hostname)"
