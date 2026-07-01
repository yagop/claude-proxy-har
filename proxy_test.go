package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeUpstream serves a JSON endpoint and a flushed SSE endpoint, standing in
// for the Anthropic API.
func fakeUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":[{"id":"claude-opus-4-8"}]}`)
		case "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			for _, chunk := range []string{
				"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			} {
				io.WriteString(w, chunk)
				if fl != nil {
					fl.Flush()
				}
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func TestProxyCapturesSessions(t *testing.T) {
	upstream := fakeUpstream()
	defer upstream.Close()

	base, _ := url.Parse(upstream.URL)
	dir := t.TempDir()
	store, err := NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Base: base, SessionHeader: "Session-Id", Redact: true}

	proxy := httptest.NewServer(newProxy(cfg, store))
	defer proxy.Close()

	// Two requests on session s1, one on s2.
	doReq(t, proxy.URL+"/v1/models", "GET", "", "s1")
	sse := doReq(t, proxy.URL+"/v1/messages", "POST", `{"model":"claude-opus-4-8","stream":true}`, "s1")
	if !strings.Contains(sse, "content_block_delta") || !strings.Contains(sse, "message_stop") {
		t.Fatalf("client did not receive full SSE stream: %q", sse)
	}
	doReq(t, proxy.URL+"/v1/models", "GET", "", "s2")

	// s1 has both entries; the SSE body and request body are captured.
	h1 := readHARWait(t, filepath.Join(dir, "s1.har"), 2)
	if got := len(h1.Log.Entries); got != 2 {
		t.Fatalf("s1: want 2 entries, got %d", got)
	}
	msg := h1.Log.Entries[1]
	if !strings.Contains(msg.Response.Content.Text, "message_stop") {
		t.Fatalf("SSE response body not captured: %q", msg.Response.Content.Text)
	}
	if msg.Response.Content.MimeType != "text/event-stream" {
		t.Fatalf("SSE mime not recorded: %q", msg.Response.Content.MimeType)
	}
	if msg.Request.PostData == nil || !strings.Contains(msg.Request.PostData.Text, "stream") {
		t.Fatal("request body not captured")
	}

	// Redaction: the x-api-key header value must be scrubbed.
	if !hasHeader(msg.Request.Headers, "X-Api-Key", "REDACTED") {
		t.Fatalf("x-api-key not redacted: %+v", msg.Request.Headers)
	}

	// s2 is a separate file with one entry.
	h2 := readHARWait(t, filepath.Join(dir, "s2.har"), 1)
	if got := len(h2.Log.Entries); got != 1 {
		t.Fatalf("s2: want 1 entry, got %d", got)
	}
}

func TestMissingSessionHeaderGoesToUnknown(t *testing.T) {
	upstream := fakeUpstream()
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	dir := t.TempDir()
	store, _ := NewStore(dir, false)
	proxy := httptest.NewServer(newProxy(&Config{Base: base, SessionHeader: "Session-Id"}, store))
	defer proxy.Close()

	doReq(t, proxy.URL+"/v1/models", "GET", "", "") // no Session-Id
	readHARWait(t, filepath.Join(dir, "unknown.har"), 1)
}

func doReq(t *testing.T, target, method, body, session string) string {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if session != "" {
		req.Header.Set("Session-Id", session)
	}
	req.Header.Set("X-Api-Key", "sk-secret")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// readHARWait polls until the file has at least want entries (the capture
// finalizes just after the client sees EOF).
func readHARWait(t *testing.T, path string, want int) HAR {
	t.Helper()
	for i := 0; i < 200; i++ {
		if data, err := os.ReadFile(path); err == nil {
			var h HAR
			if json.Unmarshal(data, &h) == nil && len(h.Log.Entries) >= want {
				return h
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d entries in %s", want, path)
	return HAR{}
}

func hasHeader(hs []HarNV, name, value string) bool {
	for _, h := range hs {
		if h.Name == name && h.Value == value {
			return true
		}
	}
	return false
}
