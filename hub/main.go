// Command hub is the central clipboard server: one static binary, a few MB of RAM.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	log.SetFlags(0)
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		serve()
	case "healthcheck":
		healthcheck()
	case "devices":
		devices()
	case "revoke":
		revoke(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintln(os.Stderr, "usage: hub [serve | healthcheck | devices | revoke NAME | version]")
		os.Exit(2)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	n, err := strconv.ParseInt(os.Getenv(key), 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func openRegistry() *Registry {
	static, err := parseTokens(os.Getenv("CLIP_TOKENS"))
	if err != nil {
		log.Fatal(err)
	}
	return NewRegistry(static, filepath.Join(env("CLIP_DATA", "/data"), "tokens.json"))
}

func serve() {
	dataDir := env("CLIP_DATA", "/data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Printf("warning: %v (enrolled devices and the saved clip will not survive a restart)", err)
	}
	reg := openRegistry()
	pass := os.Getenv("CLIP_JOIN_PASSPHRASE")
	if pass != "" && len(pass) < minPassphraseLen {
		log.Fatalf("CLIP_JOIN_PASSPHRASE must be at least %d characters", minPassphraseLen)
	}
	if pass == "" && reg.count() == 0 {
		log.Fatal("nothing can authenticate: set CLIP_JOIN_PASSPHRASE (devices join with it) or CLIP_TOKENS")
	}
	storeDir := ""
	if env("CLIP_PERSIST_CLIP", "false") == "true" { // keeps flash-backed routers from being written on every copy
		storeDir = dataDir
	}
	maxBytes := envInt("CLIP_MAX_BYTES", 8<<20)
	if os.Getenv("GOMEMLIMIT") == "" {
		// The big costs are the clip we hold, the one arriving and the garbage in
		// between, so size the GC's soft limit from the largest clip we accept.
		debug.SetMemoryLimit(16<<20 + 3*maxBytes)
	}
	srv := &http.Server{
		Addr: net.JoinHostPort(os.Getenv("CLIP_HOST"), env("CLIP_PORT", "8766")),
		Handler: NewServer(config{
			maxBytes:   maxBytes,
			passphrase: pass,
			publicURL:  strings.TrimRight(os.Getenv("CLIP_PUBLIC_URL"), "/"),
		}, NewStore(storeDir), reg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("hub %s listening on %s; join %s, persist clip %t", version, srv.Addr,
		map[bool]string{true: "enabled", false: "disabled"}[pass != ""], storeDir != "")
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func healthcheck() {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + env("CLIP_PORT", "8766") + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}

func devices() {
	fmt.Printf("%-20s %-9s %s\n", "NAME", "KIND", "CREATED")
	for _, d := range openRegistry().list() {
		fmt.Printf("%-20s %-9s %s\n", d.Name, d.Kind, d.Created)
	}
}

func revoke(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hub revoke NAME")
		os.Exit(2)
	}
	ok, err := openRegistry().revoke(args[0])
	if err != nil {
		log.Fatal(err)
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "no enrolled device named %q (fixed tokens are removed from CLIP_TOKENS)\n", args[0])
		os.Exit(1)
	}
	fmt.Printf("revoked %s\n", args[0])
}
