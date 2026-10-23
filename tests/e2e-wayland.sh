#!/usr/bin/env bash
# End-to-end: hub + two agents, each on its own headless Sway (two "laptops"), plus a fake phone.
# Needs sway, wl-clipboard, python3, curl. Run it in a throwaway container:
#   docker run --rm -v "$PWD:/src:ro" alpine:3 sh -c \
#     'apk add -q bash curl python3 sway wl-clipboard && bash /src/tests/e2e-wayland.sh'
set -uo pipefail

SRC=${SRC:-/src}
PORT=18766
URL=http://127.0.0.1:$PORT
TOKEN_A=$(printf 'a%.0s' {1..32}); TOKEN_B=$(printf 'b%.0s' {1..32}); TOKEN_PHONE=$(printf 'p%.0s' {1..32})
WORK=$(mktemp -d); export HOME=$WORK
fails=0

ok()   { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }

# run a command inside laptop $1's session
on() { local rt=$WORK/rt$1; shift; XDG_RUNTIME_DIR=$rt WAYLAND_DISPLAY=wayland-1 "$@"; }
paste() { on "$1" wl-paste -n -t text 2>/dev/null; }

# poll until laptop $1's clipboard text equals $2 (5 s)
expect_text() {
  for _ in $(seq 50); do [ "$(paste "$1")" = "$2" ] && { ok "laptop $1 has '$2'"; return; }; sleep 0.1; done
  fail "laptop $1 expected '$2', has '$(paste "$1")'"
}

start_hub() {
  CLIP_TOKENS="a:$TOKEN_A,b:$TOKEN_B,phone:$TOKEN_PHONE" CLIP_HOST=127.0.0.1 CLIP_PORT=$PORT \
    CLIP_DATA=$WORK/data python3 "$SRC/hub/server.py" 2>>"$WORK/hub.log" &
  HUB=$!
  for _ in $(seq 50); do curl -sf "$URL/healthz" >/dev/null && return; sleep 0.1; done
  echo "hub did not start"; cat "$WORK/hub.log"; exit 1
}

start_sway() {
  local rt=$WORK/rt$1; mkdir -p "$rt"; chmod 700 "$rt"
  : > "$WORK/sway.conf"
  XDG_RUNTIME_DIR=$rt WLR_BACKENDS=headless WLR_LIBINPUT_NO_DEVICES=1 WLR_RENDERER=pixman \
    sway -c "$WORK/sway.conf" >"$WORK/sway$1.log" 2>&1 &
  for _ in $(seq 100); do [ -S "$rt/wayland-1" ] && return; sleep 0.1; done
  echo "sway $1 did not start"; cat "$WORK/sway$1.log"; exit 1
}

start_agent() {  # laptop, token
  XDG_RUNTIME_DIR=$WORK/rt$1 WAYLAND_DISPLAY=wayland-1 CLIP_URL=$URL CLIP_TOKEN=$2 \
    python3 "$SRC/agent/central-clipboard-agent" >"$WORK/agent$1.log" 2>&1 &
  eval "AGENT_$1=$!"
}

cleanup() { kill $(jobs -p) 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

start_hub
start_sway A; start_sway B
start_agent A "$TOKEN_A"; start_agent B "$TOKEN_B"
sleep 1.5

echo "-- laptop to laptop"
on A sh -c 'printf "hello from A" | wl-copy'
expect_text B "hello from A"
on B sh -c 'printf "reply from B" | wl-copy'
expect_text A "reply from B"

echo "-- no echo loop"
seq1=$(curl -si -H "X-Token: $TOKEN_A" "$URL/clip" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-clip-seq"{print $2}')
sleep 2
seq2=$(curl -si -H "X-Token: $TOKEN_A" "$URL/clip" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-clip-seq"{print $2}')
[ "$seq1" = "$seq2" ] && ok "hub seq stable at $seq1" || fail "hub seq moved $seq1 -> $seq2"
pushes=$(grep -c '^pushed' "$WORK/agentA.log" "$WORK/agentB.log" | awk -F: '{s+=$2} END{print s}')
[ "$pushes" = 2 ] && ok "exactly 2 pushes in total" || fail "expected 2 pushes, saw $pushes"

echo "-- phone to laptops, laptops to phone"
printf 'text from phone' | curl -sf -X POST -H "X-Token: $TOKEN_PHONE" --data-binary @- "$URL/"
expect_text A "text from phone"; expect_text B "text from phone"
on A sh -c 'printf "for the phone" | wl-copy'
for _ in $(seq 50); do [ "$(curl -s "$URL/?t=$TOKEN_PHONE")" = "for the phone" ] && break; sleep 0.1; done
[ "$(curl -s "$URL/?t=$TOKEN_PHONE")" = "for the phone" ] && ok "phone GET sees laptop copy" || fail "phone GET stale"

echo "-- multi-line text and unicode survive"
text=$'line one\n  indented “quoted” — dash\nline three'
on A sh -c 'printf "%s" "$1" | wl-copy' _ "$text"
expect_text B "$text"

echo "-- images"
python3 - "$WORK/pic.png" <<'EOF'
import struct, sys, zlib
w, h = 16, 16
raw = b"".join(b"\x00" + bytes([255, 0, 0] * w) for _ in range(h))
def chunk(t, d): return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d))
open(sys.argv[1], "wb").write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
                              + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))
EOF
on A sh -c "wl-copy --type image/png < $WORK/pic.png"
for _ in $(seq 50); do on B wl-paste -l 2>/dev/null | grep -q '^image/png$' && break; sleep 0.1; done
on B wl-paste -t image/png > "$WORK/pic-b.png" 2>/dev/null
cmp -s "$WORK/pic.png" "$WORK/pic-b.png" && ok "PNG arrives byte-identical" || fail "PNG differs or missing"

echo "-- hub outage: copy while offline, push on reconnect"
kill $HUB; wait $HUB 2>/dev/null
on A sh -c 'printf "copied offline" | wl-copy'
sleep 1.5
start_hub
expect_text B "copied offline"

echo "-- agent restart adopts the hub clip"
kill $AGENT_B; wait $AGENT_B 2>/dev/null
on B sh -c 'printf "stale local" | wl-copy'
start_agent B "$TOKEN_B"
expect_text B "copied offline"

echo
[ $fails = 0 ] && echo "all passed" || { echo "$fails failed"; for f in hub agentA agentB; do echo "== $f.log"; tail -15 "$WORK/$f.log"; done; }
exit $fails
