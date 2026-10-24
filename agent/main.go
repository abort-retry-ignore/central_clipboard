// Command agent mirrors the Wayland clipboard to and from the central clipboard hub.
//
// Needs wl-clipboard. Configure with CLIP_URL (http://100.x.y.z:8766) and CLIP_TOKEN.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

var version = "dev"

func main() {
	log.SetFlags(0)
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	url := strings.TrimRight(os.Getenv("CLIP_URL"), "/")
	token := os.Getenv("CLIP_TOKEN")
	if url == "" || token == "" {
		log.Fatal("set CLIP_URL and CLIP_TOKEN")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	NewAgent(url, token, wlClipboard{}).Run(ctx)
}
