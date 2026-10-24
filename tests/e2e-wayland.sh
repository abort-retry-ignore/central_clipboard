#!/usr/bin/env bash
# End to end: the hub, two laptops that join through the hub's own /install.sh (each on
# its own headless Sway, so real wl-clipboard), and a fake phone.
#
#   docker build --target bin --output type=local,dest=out .
#   docker run --rm --cap-add SYS_NICE -v "$PWD:/src:ro" alpine:3 sh -c \
#     'apk add -q bash curl sway wl-clipboard >/dev/null && bash /src/tests/e2e-wayland.sh'
set -uo pipefail

SRC=${SRC:-/src}
HUB_BIN=${HUB_BIN:-$SRC/out/hub}
PORT=18766
URL=http://127.0.0.1:$PORT
PASS='correct horse battery'
TOKEN_PHONE=$(printf 'p%.0s' {1..32})
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

phone_get() { curl -s "$URL/?t=$TOKEN_PHONE"; }
phone_post() { curl -sf -X POST -H "X-Token: $TOKEN_PHONE" --data-binary @- "$URL/"; }
clip_seq() { curl -si -H "X-Token: $TOKEN_PHONE" "$URL/clip" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-clip-seq"{print $2}'; }

start_hub() {
  CLIP_TOKENS="phone:$TOKEN_PHONE" CLIP_JOIN_PASSPHRASE="$PASS" CLIP_DATA=$WORK/data \
    CLIP_PORT=$PORT CLIP_HOST=127.0.0.1 CLIP_PERSIST_CLIP=true "$HUB_BIN" serve 2>>"$WORK/hub.log" &
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

# join the way a person would: the script comes from the hub, the passphrase is all it needs
join() {  # laptop, passphrase
  local home=$WORK/home$1; mkdir -p "$home"
  curl -fsS "$URL/install.sh" |
    HOME=$home XDG_CONFIG_HOME=$home/.config CLIP_PASSPHRASE="$2" CLIP_NAME=laptop$1 bash >"$WORK/join$1.log" 2>&1
}

start_agent() {  # laptop
  local home=$WORK/home$1
  ( set -a; . "$home/.config/central-clipboard/agent.env"; set +a
    CLIP_FILES_DIR="$home/files" XDG_RUNTIME_DIR=$WORK/rt$1 WAYLAND_DISPLAY=wayland-1 exec "$home/.local/bin/central-clipboard-agent"
  ) >"$WORK/agent$1.log" 2>&1 &
  eval "AGENT_$1=$!"
}

cleanup() { kill $(jobs -p) 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

start_hub
start_sway A; start_sway B

echo "-- joining with only the passphrase"
join Z wrong && fail "joined with a wrong passphrase" || { grep -q 'wrong passphrase' "$WORK/joinZ.log" && ok "wrong passphrase refused"; }
join A "$PASS" && join B "$PASS" && ok "both laptops joined" || { fail "join failed"; cat "$WORK/joinA.log" "$WORK/joinB.log"; exit 1; }
[ -x "$WORK/homeA/.local/bin/central-clipboard-agent" ] && ok "agent binary came from the hub" || fail "no agent binary"
[ "$(stat -c %a "$WORK/homeA/.config/central-clipboard/agent.env")" = 600 ] && ok "token file is private (600)" || fail "token file perms"
before=$(cat "$WORK/homeA/.config/central-clipboard/agent.env")
join A "$PASS" && [ "$(cat "$WORK/homeA/.config/central-clipboard/agent.env")" = "$before" ] \
  && grep -q 'keeping this machine' "$WORK/joinA.log" && ok "re-running the installer keeps the token" || fail "re-join changed the token"
start_agent A; start_agent B
sleep 1.5

echo "-- laptop to laptop"
on A sh -c 'printf "hello from A" | wl-copy'
expect_text B "hello from A"
on B sh -c 'printf "reply from B" | wl-copy'
expect_text A "reply from B"

echo "-- no echo loop"
seq1=$(clip_seq); sleep 2; seq2=$(clip_seq)
[ "$seq1" = "$seq2" ] && ok "hub seq stable at $seq1" || fail "hub seq moved $seq1 -> $seq2"
pushes=$(cat "$WORK/agentA.log" "$WORK/agentB.log" | grep -c '^pushed')
[ "$pushes" = 2 ] && ok "exactly 2 pushes in total" || fail "expected 2 pushes, saw $pushes"

echo "-- phone to laptops, laptops to phone"
printf 'text from phone' | phone_post
expect_text A "text from phone"; expect_text B "text from phone"
on A sh -c 'printf "for the phone" | wl-copy'
for _ in $(seq 50); do [ "$(phone_get)" = "for the phone" ] && break; sleep 0.1; done
[ "$(phone_get)" = "for the phone" ] && ok "phone GET sees laptop copy" || fail "phone GET stale"

echo "-- multi-line text and unicode survive"
text=$'line one\n  indented “quoted” — dash\nline three'
on A sh -c 'printf "%s" "$1" | wl-copy' _ "$text"
expect_text B "$text"

echo "-- rich text from the phone is pasted as plain text"
printf '%s' '{\rtf1\ansi{\fonttbl{\f0 Helvetica;}}\f0 Hello from RTF\par}' | phone_post
expect_text A "Hello from RTF"; expect_text B "Hello from RTF"

echo "-- images"
base64 -d > "$WORK/pic.png" <<'EOF'
iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==
EOF
on A sh -c "wl-copy --type image/png < $WORK/pic.png"
for _ in $(seq 50); do on B wl-paste -l 2>/dev/null | grep -q '^image/png$' && break; sleep 0.1; done
on B wl-paste -t image/png > "$WORK/pic-b.png" 2>/dev/null
cmp -s "$WORK/pic.png" "$WORK/pic-b.png" && ok "PNG arrives byte-identical" || fail "PNG differs or missing"

echo "-- files: what a file manager's copy puts on the clipboard"
# copy_files LAPTOP PATH...: offer files the way Nautilus and Flea do (x-special/gnome-copied-files)
copy_files() {
  local l=$1 payload=copy p; shift
  for p in "$@"; do payload+=$'\n'"file://${p// /%20}"; done
  on "$l" sh -c 'printf "%s" "$1" | wl-copy --type x-special/gnome-copied-files' _ "$payload"
}
# staged LAPTOP: the paths on its clipboard as a file copy, one per line
staged() { on "$1" wl-paste -n -t x-special/gnome-copied-files 2>/dev/null | tail -n +2 | sed 's|^file://||; s|%20| |g'; }
wait_staged() {  # laptop, how many paths to expect
  for _ in $(seq 60); do [ "$(staged "$1" | grep -c .)" = "$2" ] && return 0; sleep 0.1; done; return 1
}
push_count() { grep -c "^pushed file" "$WORK/agent$1.log"; }
mkdir -p "$WORK/src/Album/sub"
head -c 300000 /dev/urandom > "$WORK/src/Quarterly report.bin"
printf 'one\n' > "$WORK/src/Album/one.txt"; head -c 5000 /dev/urandom > "$WORK/src/Album/sub/two.bin"
printf 'three\n' > "$WORK/src/three.txt"

copy_files A "$WORK/src/Quarterly report.bin"
if wait_staged B 1; then
  got=$(staged B)
  case "$got" in
    "$WORK/homeB/files/"*"/Quarterly report.bin") ok "B was offered the file, staged under its own cache";;
    *) fail "B's clipboard names '$got'";;
  esac
  cmp -s "$got" "$WORK/src/Quarterly report.bin" && ok "300 KB binary file arrives byte-identical" || fail "file contents differ"
else fail "the file never reached B (agent B: $(tail -2 "$WORK/agentB.log" | tr '\n' '|'))"; fi
[ "$(on B wl-paste -l 2>/dev/null | grep -c '^text/plain')" = 0 ] && ok "B's clipboard is a file copy, not the path as text" || fail "B also offers the path as text"

copy_files A "$WORK/src/Album" "$WORK/src/three.txt"
if wait_staged B 2; then
  mapfile -t got < <(staged B)
  [ "$(basename "${got[0]}")" = Album ] && [ "$(basename "${got[1]}")" = three.txt ] && ok "folder and file arrive as two items" || fail "items: ${got[*]}"
  diff -r "$WORK/src/Album" "${got[0]}" >/dev/null && cmp -s "$WORK/src/three.txt" "${got[1]}" && ok "folder contents identical, subfolder included" || fail "folder differs"
else fail "the folder never reached B"; fi

seqs=$(clip_seq); sleep 2
[ "$(clip_seq)" = "$seqs" ] && [ "$(push_count B)" = 0 ] && [ "$(push_count A)" = 2 ] && ok "no echo: B sent nothing back, A sent each copy once" \
  || fail "echo: seq $seqs -> $(clip_seq), A pushed $(push_count A), B pushed $(push_count B)"

echo "-- files from the phone (no name) and to the phone"
{ printf '\0\0\0\034ftypM4A \0\0\2\0'; head -c 5000 /dev/urandom; } > "$WORK/memo.m4a"
curl -sf -X POST -H "X-Token: $TOKEN_PHONE" --data-binary @"$WORK/memo.m4a" "$URL/"
if wait_staged A 1 && wait_staged B 1 && [[ "$(staged B)" == *.m4a ]]; then
  cmp -s "$(staged B)" "$WORK/memo.m4a" && cmp -s "$(staged A)" "$WORK/memo.m4a" && ok "audio from the phone is a named .m4a file on both laptops" || fail "m4a contents differ"
else fail "phone audio did not arrive as a file ($(staged B))"; fi
curl -s -D "$WORK/hdr" -o "$WORK/memo-back.m4a" -H "X-Token: $TOKEN_PHONE" "$URL/clip"
grep -qi '^content-disposition: attachment; filename="clipboard-.*\.m4a"' "$WORK/hdr" && cmp -s "$WORK/memo.m4a" "$WORK/memo-back.m4a" \
  && ok "the phone can fetch it back, with a filename" || fail "phone GET: $(tr -d '\r' < "$WORK/hdr" | tr '\n' '|')"

curl -sf -X POST -H "X-Token: $TOKEN_PHONE" -H 'X-Filename: my%20notes.txt' --data-binary 'hello file' "$URL/"
for _ in $(seq 60); do [ "$(basename "$(staged B)")" = "my notes.txt" ] && break; sleep 0.1; done
[ "$(basename "$(staged B)")" = "my notes.txt" ] && [ "$(cat "$(staged B)")" = "hello file" ] \
  && ok "a filename from the phone is kept (a text file stays a file)" || fail "named text: $(staged B)"

copy_files A "$WORK/src/Quarterly report.bin"
for _ in $(seq 50); do curl -si -H "X-Token: $TOKEN_PHONE" "$URL/clip" | grep -qi '^x-clip-name: Quarterly%20report.bin' && break; sleep 0.1; done
curl -s -D "$WORK/hdr" -o "$WORK/back.bin" -H "X-Token: $TOKEN_PHONE" "$URL/clip"
cmp -s "$WORK/back.bin" "$WORK/src/Quarterly report.bin" && grep -qi '^x-clip-name: Quarterly%20report.bin' "$WORK/hdr" \
  && ok "a laptop's file is fetchable by the phone, name and all" || fail "laptop file to phone"

echo "-- a file over the hub's limit is not sent"
head -c 9000000 /dev/urandom > "$WORK/src/huge.bin"
seqs=$(clip_seq)
copy_files A "$WORK/src/huge.bin"
for _ in $(seq 30); do grep -q "huge.bin is 9000000 bytes, over the hub's limit" "$WORK/agentA.log" && break; sleep 0.1; done
grep -q "huge.bin is 9000000 bytes, over the hub's limit" "$WORK/agentA.log" && [ "$(clip_seq)" = "$seqs" ] \
  && ok "refused on the laptop, nothing uploaded" || fail "huge file: seq $seqs -> $(clip_seq)"

echo "-- hub outage: copy while offline, push on reconnect"
kill $HUB; wait $HUB 2>/dev/null
on A sh -c 'printf "copied offline" | wl-copy'
sleep 1.5
start_hub
expect_text B "copied offline"

echo "-- agent restart adopts the hub clip"
kill $AGENT_B; wait $AGENT_B 2>/dev/null
on B sh -c 'printf "stale local" | wl-copy'
start_agent B
expect_text B "copied offline"

echo "-- revoking a device cuts it off"
CLIP_DATA=$WORK/data "$HUB_BIN" revoke laptopB >/dev/null && ok "revoked laptopB"
for _ in $(seq 100); do grep -q 'refused the event stream: HTTP 403' "$WORK/agentB.log" && break; sleep 0.25; done
grep -q 'refused the event stream: HTTP 403' "$WORK/agentB.log" && ok "B's open stream was dropped and refused" || fail "B was not cut off"
on A sh -c 'printf "after revoke" | wl-copy'
expect_text A "after revoke"
sleep 1.5
[ "$(paste B)" != "after revoke" ] && ok "B no longer receives clips" || fail "B still received a clip"
on B sh -c 'printf "from revoked B" | wl-copy'; sleep 1.5
[ "$(phone_get)" = "after revoke" ] && ok "B's copies no longer reach the hub" || fail "hub took a clip from revoked B"

echo
[ $fails = 0 ] && echo "all passed" || { echo "$fails failed"; for f in hub agentA agentB joinA joinB; do echo "== $f.log"; tail -12 "$WORK/$f.log"; done; }
exit $fails
