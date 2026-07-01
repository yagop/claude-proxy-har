package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	creatorName = "claude-har"
	version     = "0.1.0"
)

func main() {
	port := flag.String("port", envOr("PORT", "8787"), "port to listen on")
	out := flag.String("out", envOr("HAR_OUT", "./sessions"), "directory for .har files")
	sessionHeader := flag.String("session-header", "X-Claude-Code-Session-Id", "request header used to group entries into a file")
	redact := flag.Bool("redact", false, "redact x-api-key / authorization in stored headers")
	pretty := flag.Bool("pretty", false, "pretty-print HAR JSON")
	verbose := flag.Bool("verbose", false, "log each proxied request")
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
		Base:          base,
		SessionHeader: *sessionHeader,
		Redact:        *redact,
		Verbose:       *verbose,
	}

	srv := &http.Server{Addr: ":" + *port, Handler: newProxy(cfg, store)}

	go func() {
		log.Printf("claude-har listening on :%s -> %s (out=%s, session-header=%s)",
			*port, base, *out, *sessionHeader)
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
