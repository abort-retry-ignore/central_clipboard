#!/usr/bin/env bash
# Build for this machine: the gzipped agents (embedded in the hub) into agentbin/,
# then the hub into out/hub. The Dockerfile does the same for the container image.
set -euo pipefail
cd "$(dirname "$0")/.."

export CGO_ENABLED=0
flags=(-trimpath -ldflags="-s -w")
for arch in amd64 arm64; do
  GOOS=linux GOARCH=$arch go build "${flags[@]}" -o "agentbin/linux-$arch" ./agent
  gzip -9nf "agentbin/linux-$arch"
done
go build "${flags[@]}" -o out/hub ./hub
ls -l agentbin/*.gz out/hub
