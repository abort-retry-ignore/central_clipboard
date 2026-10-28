# central clipboard

Copy on one device, paste on another. One shared clipboard for everything on your Tailscale network: Linux machines running Wayland (built for [Omarchy](https://omarchy.org)) and an iPhone.

```
  iPhone ── Send / Get ──┐
                         ▼
  Linux machine ◀──▶   hub   ◀──▶ Linux machine
  (agent)         one small       (agent)
                  container
```

## What it shares

- **Text and pictures.** Photos from the phone are converted to PNG on the Linux side, so they paste anywhere.
- **Files and folders.** Copy in a file manager (Flea, Nautilus, Dolphin), paste in a file manager on another machine.
- **Last copy wins.** Nothing is echoed back, and clips marked secret by password managers are never sent.

## Set up

**1. The hub.** Run it as one Docker container on any machine on your tailnet.

```sh
cp .env.example .env
$EDITOR .env        # BIND_IP = this machine's tailscale address, CLIP_JOIN_PASSPHRASE = what a new device types to join
docker compose up -d --build
```

It listens only on the tailscale address, so nothing else can reach it. A ready-made image is at `ghcr.io/abort-retry-ignore/central_clipboard-hub`. It runs on amd64, arm64, arm/v7 and arm/v6, so it also fits a router; there is a `docker run` example in the [reference](docs/reference.md#on-an-openwrt-router-or-any-small-box).

**2. Each Linux machine.** Needs `curl` and `wl-clipboard` (plus ImageMagick for photos).

```sh
curl -fsSL http://<hub>:8766/install.sh | bash
```

It asks for the passphrase, installs the agent as a systemd user service and starts it. Run it again any time to upgrade.

**3. The iPhone.** Open `http://<hub>:8766/join` in Safari (the phone needs Tailscale on), enter a name and the passphrase, then follow the page to add the **Clipboard** shortcut.

## Use it

- **On Linux** there is nothing to do. Copy and paste as usual. For a file, copy it in the file manager and paste it into a file manager on the other machine; a terminal paste gets nothing.
- **On the iPhone** run **Clipboard** and choose **Send** to put the phone's clipboard on your other machines, or **Get** to bring theirs to the phone. You can put it on the Action Button or Back Tap for one-touch use.

## Good to know

- **Size limit.** The hub keeps the latest clip in memory, so a clip must fit in `CLIP_MAX_BYTES` (8 MiB by default). Bigger files are skipped with a note in the agent's log.
- **Manage devices.** `docker compose exec hub /hub devices` lists them and `docker compose exec hub /hub revoke <name>` cuts one off.
- **Security.** The passphrase and each device's token only protect you inside the tailnet, and traffic is plain HTTP that Tailscale encrypts. Keep the hub's port off the public internet.
- **Small.** The image is about 14 MB and the hub uses about 7.5 MiB of memory when idle.
- **Go and shell only.** There is no Python anywhere.

## More

- [docs/reference.md](docs/reference.md): configuration, protocol, files in detail, building the iPhone shortcut, security, footprint and what is not done yet.
- Development: `go test ./...`, `bash scripts/build.sh`, and `docker build --target test .` to run the checks CI runs.
- The signed shortcuts from the [Unayung project](https://github.com/Unayung/omarchy-iphone-clipboard) in `shortcuts/` are unchanged, under their MIT license, and speak the same protocol.
