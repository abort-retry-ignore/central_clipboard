#!/usr/bin/env bash
# Build the Clipboard shortcut and sign it on a Mac. iOS only imports signed
# shortcuts, and signing is macOS-only (it calls Apple's signing service, so the
# Mac needs network access and may need to be signed in to iCloud).
#
#   scripts/sign-shortcut.sh my-mac    # from Linux: signs on that Mac over ssh
#   scripts/sign-shortcut.sh             # on the Mac itself
#
# Result: shortcuts/Clipboard.shortcut. Rebuild the hub (docker compose up -d --build)
# and /join offers it in place of the two Unayung shortcuts.
set -euo pipefail
cd "$(dirname "$0")/.."

mac=${1:-}
unsigned=out/Clipboard.unsigned.shortcut
signed=out/Clipboard.shortcut
mkdir -p out

if command -v go >/dev/null 2>&1; then
  go run ./shortcuts/gen > "$unsigned"
elif [ ! -f "$unsigned" ]; then
  echo "Go is needed to generate the shortcut here. Generate $unsigned on a machine that has it and copy it over." >&2
  exit 1
fi

if [ -n "$mac" ]; then
  work=$(ssh "$mac" 'mktemp -d')
  trap 'ssh "$mac" "rm -rf \"$work\"" >/dev/null 2>&1 || true' EXIT
  scp -q "$unsigned" "$mac:$work/in.shortcut"
  ssh "$mac" "cd '$work' && plutil -convert binary1 in.shortcut -o bin.shortcut && shortcuts sign --mode anyone --input bin.shortcut --output Clipboard.shortcut"
  scp -q "$mac:$work/Clipboard.shortcut" "$signed"
else
  [ "$(uname -s)" = Darwin ] || { echo "not on a Mac: pass the Mac's ssh host as the argument" >&2; exit 1; }
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  plutil -convert binary1 "$unsigned" -o "$work/bin.shortcut"
  shortcuts sign --mode anyone --input "$work/bin.shortcut" --output "$signed"
fi

# signed shortcuts are Apple Encrypted Archives, which start with "AEA1"
if [ "$(head -c 4 "$signed")" != AEA1 ]; then
  echo "$signed does not look like a signed shortcut (no AEA1 header); not installing it" >&2
  exit 1
fi
cp "$signed" shortcuts/Clipboard.shortcut
echo "signed: shortcuts/Clipboard.shortcut ($(wc -c < shortcuts/Clipboard.shortcut) bytes)"
echo "next: docker compose up -d --build, then import it from http://<hub>:8766/join on the iPhone"
