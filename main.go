package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	creatorName = "claude-proxy-har"
	version     = "0.1.0"
)

func main() {
	host := flag.String("host", envOr("HOST", "127.0.0.1"), "interface to bind (127.0.0.1 = loopback only; 0.0.0.0 = all interfaces)")
	port := flag.String("port", envOr("PORT", "8787"), "port to listen on")
	out := flag.String("out", envOr("HAR_OUT", "./sessions"), "directory for .har files")
	sessionHeader := flag.String("session-header", "X-Claude-Code-Session-Id", "request header used to group entries into a file")
	acceptEncoding := flag.String("accept-encoding", "", "override the outbound Accept-Encoding header (e.g. \"identity\" or \"gzip\"); empty = pass the client's through unchanged")
	hideAuth := flag.Bool("hide-auth", true, "redact the authentication header (x-api-key / authorization) in stored HARs; pass -hide-auth=false to keep it")
	pretty := flag.Bool("pretty", false, "pretty-print HAR JSON")
	verbose := flag.Bool("verbose", false, "print the full HAR entry (JSON) for each request to stderr")
	flag.Parse()

	baseRaw := envOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
	base, err := url.Parse(baseRaw)
	if err != nil || base.Scheme == "" || base.Host == "" {
		log.Fatalf("invalid ANTHROPIC_BASE_URL %q: %v", baseRaw, err)
	}
	base.Path = strings.TrimRight(base.Path, "/")

	store, err := NewStore(*out, *pretty)
	if err != nil {
		log.Fatalf("cannot open out dir %q: %v", *out, err)
	}

	cfg := &Config{
		Base:           base,
		SessionHeader:  *sessionHeader,
		AcceptEncoding: *acceptEncoding,
		HideAuth:       *hideAuth,
		Verbose:        *verbose,
	}

	addr := net.JoinHostPort(*host, *port)
	srv := &http.Server{Addr: addr, Handler: newProxy(cfg, store)}

	go func() {
		log.Printf("claude-proxy-har listening on %s", addr)
		log.Printf("  upstream:       %s", base)
		log.Printf("  out dir:        %s", *out)
		log.Printf("  session header: %s", *sessionHeader)
		log.Printf("  accept-enc:     %s", orDefault(*acceptEncoding, "(passthrough)"))
		log.Printf("  hide auth:      %v   pretty: %v   verbose: %v", *hideAuth, *pretty, *verbose)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
