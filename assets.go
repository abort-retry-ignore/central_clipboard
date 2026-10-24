// Package assets embeds the files the hub hands out to new devices.
package assets

import "embed"

// agentbin holds the gzipped agent builds, produced by the Dockerfile or
// scripts/build.sh; it is empty in a bare checkout.
//
//go:embed agent/central-clipboard-agent.service agent/join.sh shortcuts/*.shortcut all:agentbin
var FS embed.FS
