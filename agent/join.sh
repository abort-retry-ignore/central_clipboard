#!/usr/bin/env bash
# Join this machine to the central clipboard hub at @HUB@.
#
#   curl -fsSL @HUB@/install.sh | bash
#   curl -fsSL @HUB@/install.sh | CLIP_PASSPHRASE='...' CLIP_NAME=laptop2 bash   # unattended
#
# Downloads the agent from the hub, trades the passphrase for this machine's own
# token, and runs the agent as a systemd user service. Re-running upgrades the
# agent and keeps the token. Needs curl, gunzip and wl-clipboard.
set -euo pipefail

HUB='@HUB@'

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1 ($2)" >&2; exit 1; }; }
need curl "talks to the hub"
need gunzip "unpacks the agent"
need wl-paste "from the wl-clipboard package"
need wl-copy "from the wl-clipboard package"

[ "$(uname -s)" = Linux ] || { echo "Linux only" >&2; exit 1; }
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "no agent build for $(uname -m)" >&2; exit 1 ;;
esac

conf=${XDG_CONFIG_HOME:-$HOME/.config}
envfile=$conf/central-clipboard/agent.env
bin=$HOME/.local/bin/central-clipboard-agent
unit=$conf/systemd/user/central-clipboard-agent.service
name=$(printf %s "${CLIP_NAME:-$(hostname -s 2>/dev/null || hostname)}" | tr -c 'A-Za-z0-9_-' '-' | cut -c1-32)

tmp=
cleanup() { [ -z "$tmp" ] || rm -f "$tmp" "$tmp.pass"; }
trap cleanup EXIT

token_works() {
  [ -n "$1" ] || return 1
  case $(curl -s -o /dev/null -w '%{http_code}' -H "X-Token: $1" "$HUB/clip") in 200|204) return 0 ;; esac
  return 1
}

token=
if [ -f "$envfile" ] && [ "$(sed -n 's/^CLIP_URL=//p' "$envfile")" = "$HUB" ]; then
  old=$(sed -n 's/^CLIP_TOKEN=//p' "$envfile")
  if token_works "$old"; then
    token=$old
    echo "keeping this machine's existing token"
  fi
fi

mkdir -p "$(dirname "$bin")" "$(dirname "$envfile")" "$(dirname "$unit")"
tmp=$(mktemp "$bin.XXXXXX")

if [ -z "$token" ]; then
  pass=${CLIP_PASSPHRASE:-}
  if [ -z "$pass" ]; then
    [ -r /dev/tty ] || { echo "no terminal to ask on: set CLIP_PASSPHRASE" >&2; exit 1; }
    read -rsp "Passphrase for $HUB: " pass </dev/tty
    echo >&2
  fi
  (umask 077; printf %s "$pass" > "$tmp.pass")
  reply=$(curl -sS --data-urlencode "name=$name" --data-urlencode "passphrase@$tmp.pass" \
            -w '\n%{http_code}' "$HUB/enroll") || { echo "could not reach $HUB" >&2; exit 1; }
  code=${reply##*$'\n'}
  body=${reply%$'\n'*}
  case $code in
    200) token=$(printf %s "$body" | sed -n 's/.*"token":"\([0-9a-f]*\)".*/\1/p') ;;
    403) echo "wrong passphrase" >&2; exit 1 ;;
    409) echo "the name '$name' is taken; choose another with CLIP_NAME" >&2; exit 1 ;;
    429) echo "too many wrong passphrases; wait a few minutes" >&2; exit 1 ;;
    *) echo "joining failed (HTTP $code): $body" >&2; exit 1 ;;
  esac
  [ -n "$token" ] || { echo "unexpected reply from the hub: $body" >&2; exit 1; }
fi

curl -fsS "$HUB/agent/linux-$arch.gz" | gunzip > "$tmp"
chmod 755 "$tmp"
mv -f "$tmp" "$bin"
tmp=
curl -fsS "$HUB/agent.service" -o "$unit"
(umask 077; printf 'CLIP_URL=%s\nCLIP_TOKEN=%s\n' "$HUB" "$token" > "$envfile")

if systemctl --user show-environment >/dev/null 2>&1; then
  systemctl --user daemon-reload
  systemctl --user enable central-clipboard-agent.service >/dev/null 2>&1
  systemctl --user restart central-clipboard-agent.service
  echo "joined as '$name' and running. Logs: journalctl --user -u central-clipboard-agent -f"
else
  echo "joined as '$name', but there is no systemd user session here. Run the agent with:"
  echo "  set -a; . '$envfile'; set +a; '$bin'"
fi
command -v magick >/dev/null 2>&1 || command -v convert >/dev/null 2>&1 ||
  echo "note: install ImageMagick to paste photos sent from the phone"
