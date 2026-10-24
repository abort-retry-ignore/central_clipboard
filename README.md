# central clipboard

One shared clipboard for every machine on the tailnet: Wayland Linux machines (built for Omarchy) and an iPhone.

```
 iPhone Shortcuts ──POST/GET──┐
                              ▼
 laptop agent ◀──SSE──▶   hub (one static binary)   ◀──SSE──▶ laptop agent
  wl-paste --watch         latest clip                          wl-copy
  wl-copy                  serves the installer, agents, join page
```

- **hub** (`hub/`): a single static Go binary, small enough for a router (see [Footprint](#footprint)). Keeps the latest clip, streams changes to agents, and hands new devices everything they need to join.
- **agent** (`agent/`): a second small Go binary for each Wayland machine, run as a systemd user service. It watches the clipboard with `wl-paste --watch`, pushes changes to the hub, and applies the hub's changes with `wl-copy`. Text, images, and files and folders copied in a file manager (see [Files](#files)); synced clips also show up in Walker's `Super+Ctrl+V` history. HEIC photos and RTF text from the phone are converted on this side (ImageMagick for photos).
- Everything is Go plus POSIX shell. There is no Python anywhere.

## Run the hub

On a Docker host that is on your tailnet:

```sh
cp .env.example .env
# BIND_IP: this host's tailscale address (tailscale ip -4)
# CLIP_JOIN_PASSPHRASE: what a new device types to join
$EDITOR .env
docker compose up -d --build
```

The port is published only on `BIND_IP`, so the hub is reachable from the tailnet and nowhere else.

### On an OpenWrt router (or any small box)

```sh
docker run -d --name clipboard --restart unless-stopped \
  -p <tailscale-ip>:8766:8766 \
  -e CLIP_JOIN_PASSPHRASE='…' -e CLIP_MAX_BYTES=2097152 \
  -v clip-data:/data --read-only --memory 48m \
  ghcr.io/abort-retry-ignore/central_clipboard-hub:latest
```

The clip is held in RAM and never written to flash unless `CLIP_PERSIST_CLIP=true`, so copying does not wear the storage. Only enrolled devices' hashed tokens are stored (rarely written). `CLIP_MAX_BYTES` is the main memory knob.

## Add a device

Joining needs only the passphrase, and everything comes from the hub.

**Linux (Wayland):**

```sh
curl -fsSL http://<hub>:8766/install.sh | bash
```

It asks for the passphrase, downloads the agent from the hub, gets this machine its own token, and starts a systemd user service. Running it again upgrades the agent and keeps the token. Unattended: `curl -fsSL http://<hub>:8766/install.sh | CLIP_PASSPHRASE='…' CLIP_NAME=laptop2 bash`. Needs `curl` and `wl-clipboard`.

**iPhone:** open `http://<hub>:8766/join` in Safari, enter a name and the passphrase, then follow the page: copy the URL, add the two shortcuts, paste the URL when asked. Use the hub's address on the tailnet (the phone needs Tailscale on).

**Anything else:** `curl -s -X POST http://<hub>:8766/enroll -d name=my-device -d passphrase='…'` returns `{"name","token","url"}`. Send the token as `X-Token` or `?t=`.

Joining the same name again replaces that device's token.

### Manage devices

```sh
docker compose exec hub /hub devices        # fixed and enrolled devices
docker compose exec hub /hub revoke laptop2 # cuts it off; open streams close within 15 s
```

Changing the passphrase (`.env`, then `docker compose up -d`) only affects devices that join afterwards.

### The iPhone shortcut

Until `shortcuts/Clipboard.shortcut` exists, `/join` offers the two Unayung shortcuts. Once it does, `/join` offers that one instead: a single "Clipboard" shortcut with a Send / Get menu, where the URL is asked for once at import.

iOS only imports signed shortcuts, and signing needs a Mac with network access (it calls Apple's service). To produce it:

```sh
scripts/sign-shortcut.sh <mac-ssh-host>   # from Linux; run with no argument on the Mac itself
docker compose up -d --build              # the hub embeds the signed file
```

`shortcuts/gen` generates the unsigned shortcut (plain Go, covered by tests); the script copies it to the Mac, converts it with `plutil`, signs it with `shortcuts sign --mode anyone`, checks for the signed header, and installs it. Without ssh access, run `go run ./shortcuts/gen > Clipboard.unsigned.shortcut` here, then on the Mac:

```sh
plutil -convert binary1 Clipboard.unsigned.shortcut -o bin.shortcut
shortcuts sign --mode anyone --input bin.shortcut --output Clipboard.shortcut
```

and put the result in `shortcuts/`. The shortcut holds no hub address or token, so one signed file serves every hub. If the Mac rejects the generated file or the phone will not import it, build the menu shortcut once by hand in the Shortcuts app, sign that export instead, and commit it as `shortcuts/Clipboard.shortcut`.

## How it behaves

- When an agent starts it adopts the hub's clip; after that the last copy wins.
- A copy made while the hub is unreachable is pushed when the connection returns.
- Clips marked as secrets by password managers (`x-kde-passwordManagerHint`) are never sent.
- Text is preferred over images when both are on the clipboard. Files copied in a file manager come before both (see below).
- Identical content is never re-sent, so machines do not echo clips back and forth.

### Files

Copy a file or folder in your file manager (Nautilus, Flea, Dolphin, and anything else that offers `x-special/gnome-copied-files` or `text/uri-list`) and paste it into a file manager on another machine, as you would on one machine.

- **How:** the clipboard only holds paths, so the agent sends the file itself. One file goes as it is, with its name; several files or a folder go as a zip. The receiving agent saves it under `~/.cache/central-clipboard/files/` (or `CLIP_FILES_DIR`) and puts it on the clipboard as a copy of those files, so Ctrl+V in the file manager copies it into the folder you are looking at.
- **From the phone:** a file copied on the iPhone (an iCloud Drive file, a voice memo) arrives the same way. The shortcut does not send a name yet, so it is called `clipboard-<time>.<type>`, with the type worked out from the contents (m4a, mp4, mov, pdf, zip and a few more).
- **To the phone:** the Get shortcut fetches the file with its name. Several files arrive as one zip.
- **Limits:** the whole clip is held in the hub's memory, so a file must fit in `CLIP_MAX_BYTES` (8 MiB by default). The hub tells agents its limit, and a file over it is skipped with a line in the agent's log instead of being uploaded. Raise the limit on a hub with RAM to spare; a router should keep it small.
- **Not carried:** `cut` is treated as `copy`, nothing is deleted at the source. Symlinks inside a folder, timestamps and permissions are not carried. Received files are never executable.
- **Pasting into a terminal** gives nothing for a received file, because the clipboard then holds only the file copy (`wl-copy` can offer a single type). Pasting into a file manager is what it is for.
- **Disk use:** only the newest clip is kept in the staging directory; older ones are deleted when a new one arrives. Point `CLIP_FILES_DIR` at a directory of its own: the agent only removes the 16-character clip folders it created there.

## Configuration

| Variable | Default | |
|---|---|---|
| `CLIP_JOIN_PASSPHRASE` | | Enables joining. At least 10 characters. |
| `CLIP_TOKENS` | | Optional fixed tokens, `name:token,name:token` (tokens ≥ 16 chars). |
| `CLIP_PORT` / `CLIP_HOST` | `8766` / all | Listen address inside the container. |
| `CLIP_DATA` | `/data` | Where enrolled tokens (and the clip, if persisted) live. |
| `CLIP_PERSIST_CLIP` | `false` | Keep the clip across restarts. |
| `CLIP_MAX_BYTES` | `8388608` | Largest clip accepted. |
| `CLIP_PUBLIC_URL` | from `Host` | Address baked into scripts and pages, if the hub sits behind a proxy. |

## Protocol

| Request | Effect |
|---|---|
| `POST /` (or `/clip`), raw body | Replace the clip. 204. Identical content is a no-op. Optional `X-Filename` (percent-encoded) makes it a file; `X-Clip-Bundle: 1` says it is a zip of several files or a folder. |
| `GET /` (or `/clip`) | Latest clip with its `Content-Type`. 204 if empty. A file also has `X-Clip-Name`, `Content-Disposition` and, for a bundle, `X-Clip-Bundle: 1`. |
| `GET /events` | SSE: `hello` (your device name, the clip and `max`, the largest clip the hub accepts) on connect, `clip` on each change, `: ping` every 15 s. |
| `POST /enroll` | Passphrase in, token out. |
| `GET /join`, `/install.sh`, `/agent/linux-{amd64,arm64}.gz`, `/agent.service`, `/shortcuts/*.shortcut` | Unauthenticated; nothing secret in them. |
| `GET /healthz` | Liveness probe. |

Auth is `X-Token: …`, `Authorization: Bearer …`, or `?t=…`. Query tokens are never logged. This is the same protocol the [Unayung iPhone shortcuts](https://github.com/Unayung/omarchy-iphone-clipboard) use, which is why they work unchanged.

## Security

- The passphrase is the key to the cluster and the token is the key to a device. Both only protect you inside the tailnet, so keep the hub's port off the public internet. Consider a Tailscale ACL limiting who can reach it.
- Tokens are stored hashed. Wrong passphrases are limited to 5 per 5 minutes per client address.
- Traffic is plain HTTP, which Tailscale encrypts. `curl | bash` is only as trustworthy as the hub it comes from.
- The container runs as a non-root user, read-only, with no capabilities and no shell.
- Files from the network are written only inside the agent's staging directory: names are reduced to a plain file name, archive entries that would leave it are refused, links and special files are skipped, nothing is created executable, and an archive may expand to at most 512 MiB. Anyone holding a device token can put files there, so treat tokens accordingly.
- `shortcuts/` holds the signed shortcuts from the Unayung project, unchanged, under their MIT license.

## Footprint

Measured on an x86-64 build (resident memory, while sending ten max-size clips and eight concurrent reads of one):

| | |
|---|---|
| Image | 14 MB (the two embedded agents are ~5 MB of that) |
| Idle | ~7.5 MiB |
| 5 open event streams | ~9.5 MiB |
| `CLIP_MAX_BYTES` 2 MiB | peak ~14.5 MiB |
| `CLIP_MAX_BYTES` 8 MiB (default) | peak ~27 MiB, ~20 MiB afterwards |
| `CLIP_MAX_BYTES` 32 MiB | peak ~75 MiB, ~44 MiB afterwards |

Ordinary text clips stay near the idle figure. The hub sets its own garbage-collector limit from `CLIP_MAX_BYTES` (override with `GOMEMLIMIT`) and hands memory back after large clips.

Images are published for `linux/amd64`, `arm64`, `arm/v7`, `arm/v6` and `mipsle`. CI starts the first four under emulation; mipsle is built but has never been run.

## Development

```sh
go test ./...                      # hub and agent (agent builds must exist: scripts/build.sh)
bash scripts/build.sh              # agents into agentbin/, hub into out/hub
docker build --target test .       # gofmt, vet and tests, as CI runs them

# end to end: hub + two agents joining through /install.sh on two headless Sway sessions
docker build --target bin --output type=local,dest=out .
docker run --rm --cap-add SYS_NICE -v "$PWD:/src:ro" alpine:3 sh -c \
  'apk add -q bash curl sway wl-clipboard >/dev/null && bash /src/tests/e2e-wayland.sh'
```

## Not yet

- Clipboard history.
- Files larger than `CLIP_MAX_BYTES` (they would have to stream through the hub to disk instead of RAM), and filenames from the iPhone shortcut.
- Rich text (RTF is flattened to plain text).
- Offering a file copy as `text/uri-list` as well, for file managers that do not read `x-special/gnome-copied-files`; `wl-copy` can only offer one type.
- Agents for architectures other than amd64 and arm64 (add them in the `agents` stage of the Dockerfile and `agent/join.sh`).
