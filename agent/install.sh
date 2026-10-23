#!/usr/bin/env bash
# Install the central clipboard agent as a systemd user service.
#   ./install.sh http://100.x.y.z:8766 <this-device's-token>
set -euo pipefail

url=${1:?usage: install.sh http://100.x.y.z:8766 TOKEN}
token=${2:?usage: install.sh http://100.x.y.z:8766 TOKEN}
here=$(cd "$(dirname "$0")" && pwd)
conf=${XDG_CONFIG_HOME:-$HOME/.config}

command -v wl-paste >/dev/null || { echo "wl-clipboard is not installed" >&2; exit 1; }

install -Dm755 "$here/central-clipboard-agent" "$HOME/.local/bin/central-clipboard-agent"
install -Dm644 "$here/central-clipboard-agent.service" "$conf/systemd/user/central-clipboard-agent.service"

mkdir -p "$conf/central-clipboard"
(umask 077; printf 'CLIP_URL=%s\nCLIP_TOKEN=%s\n' "$url" "$token" > "$conf/central-clipboard/agent.env")

systemctl --user daemon-reload
systemctl --user enable central-clipboard-agent.service
systemctl --user restart central-clipboard-agent.service
echo "installed; logs: journalctl --user -u central-clipboard-agent -f"
