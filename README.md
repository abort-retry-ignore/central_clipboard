# central clipboard

One shared clipboard for every machine on the tailnet: the Omarchy laptops and desktop (Wayland) and an iPhone.

```
 iPhone Shortcuts ──POST/GET──┐
                              ▼
 laptop agent ◀──SSE──▶   hub (docker, tailnet only)   ◀──SSE──▶ laptop agent
  wl-paste --watch         latest clip, persisted                 wl-copy
  wl-copy                  HEIC→PNG, RTF→text
```

- **hub** (`hub/`): small Python service in a container. Keeps the latest clip, accepts and serves it over HTTP, and streams changes to agents over Server-Sent Events. Same wire protocol as [Unayung/omarchy-iphone-clipboard](https://github.com/Unayung/omarchy-iphone-clipboard), so its iPhone Shortcuts work after changing the URL.
- **agent** (`agent/`): one per Omarchy machine, a systemd user service. Watches the clipboard with `wl-paste --watch`, pushes changes to the hub, and applies the hub's changes with `wl-copy`. Text and images; synced clips also show up in Walker's `Super+Ctrl+V` history.

## Run the hub

On a Docker host that is on your tailnet:

```sh
cp .env.example .env
# BIND_IP: this host's tailscale address (tailscale ip -4)
# CLIP_TOKENS: one name:token per device, tokens from `openssl rand -hex 24`
$EDITOR .env
docker compose up -d --build
```

The port is published only on `BIND_IP`, so the hub is reachable from the tailnet and nowhere else. To revoke a device, delete its entry from `CLIP_TOKENS` and run `docker compose up -d`. The clip persists in the `clip-data` volume.

## Add an Omarchy machine

Each machine gets its own token from `CLIP_TOKENS`:

```sh
./agent/install.sh http://<hub-tailscale-ip>:8766 <that-machine's-token>
journalctl --user -u central-clipboard-agent -f
```

If you run the Unayung plugin on that machine, turn off its service so only the agent writes the clipboard.

Behaviour worth knowing:
- On start the agent adopts the hub's clip; after that the last writer wins.
- A copy made while the hub is unreachable is pushed when the connection returns.
- Clips marked as secrets by password managers (`x-kde-passwordManagerHint`) are never sent.
- Text is preferred over images when both are on the clipboard. Files are not synced.

## iPhone

Open each of the two Unayung Shortcuts ("Send to Computer", "Get from Computer") and change the URL to
`http://<hub-tailscale-ip>:8766/?t=<phone token>`. Your Back Tap bindings stay as they are. The hub converts HEIC photos to PNG and RTF to plain text on arrival, as in the Unayung PR.

## Protocol

| Request | Effect |
|---|---|
| `POST /` (or `/clip`), raw body | Replace the clip. 204. Identical content is a no-op. |
| `GET /` (or `/clip`) | Latest clip with its `Content-Type` (`text/plain` or `image/png`). 204 if empty. |
| `GET /events` | SSE: `hello` (your device name and the current clip) on connect, `clip` on each change, `: ping` every 15 s. |
| `GET /healthz` | Unauthenticated liveness probe. |

Auth is `X-Token: …`, `Authorization: Bearer …`, or `?t=…`. Query tokens are never logged.

## Security

The token is the only protection inside the tailnet, and the clipboard holds whatever you copy, including passwords. Treat tokens like passwords, give each device its own, and consider a Tailscale ACL that limits who can reach the hub's port. Traffic is HTTP, but Tailscale encrypts it end to end. The container runs as a non-root user with a read-only filesystem and no capabilities.

## Tests

```sh
# hub: protocol, conversions, SSE, persistence (needs Pillow, so run it in the image)
docker build -t central-clipboard-hub:dev hub
docker run --rm -v "$PWD:/src:ro" -w /src --entrypoint python central-clipboard-hub:dev -B -m unittest discover -s tests

# end to end: hub + two agents on two headless Sway sessions + a fake phone
docker run --rm --cap-add SYS_NICE -v "$PWD:/src:ro" alpine:3 sh -c \
  'apk add -q bash curl python3 sway wl-clipboard >/dev/null; bash /src/tests/e2e-wayland.sh'
```

## Not yet

- Clipboard history (the hub only keeps the latest clip).
- Files and rich text.
- HTTPS with a Tailscale name (a `tailscale` sidecar container with `tailscale serve` would give the hub its own tailnet identity).
