package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Base           *url.URL
	SessionHeader  string
	AcceptEncoding string // if non-empty, overrides the outbound Accept-Encoding
	HideAuth       bool
	Verbose        bool
}

// newProxy builds a ReverseProxy that retargets onto cfg.Base, streams
// responses immediately (FlushInterval = -1, required for SSE), and captures
// every round trip via captureTransport.
func newProxy(cfg *Config, store *Store) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(cfg.Base)         // scheme + host + base-path join
			r.Out.Host = cfg.Base.Host // upstream Host header (not the inbound one)
			if cfg.AcceptEncoding != "" {
				r.Out.Header.Set("Accept-Encoding", cfg.AcceptEncoding)
			}
			// Not calling SetXForwarded(): forward the request untouched.
		},
		FlushInterval: -1,
		Transport:     &captureTransport{cfg: cfg, store: store, base: http.DefaultTransport},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("upstream error: %s %s: %v", r.Method, r.URL.Path, err)
			_ = store.Add(r.Header.Get(cfg.SessionHeader), errorEntry(r, err))
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

// captureTransport is the single place that sees the full request and response.
type captureTransport struct {
	cfg   *Config
	store *Store
	base  http.RoundTripper
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	// Buffer the request body so we can both forward it and record it.
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	session := req.Header.Get(t.cfg.SessionHeader)

	if t.cfg.Verbose {
		log.Printf("→ %s %s (upstream %s)", req.Method, req.URL.RequestURI(), req.URL.Host)
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	firstByte := time.Now()

	rec := &record{
		t:          t,
		start:      start,
		firstByte:  firstByte,
		session:    session,
		method:     req.Method,
		path:       req.URL.RequestURI(),
		url:        req.URL.String(),
		reqHeader:  req.Header.Clone(),
		reqBody:    reqBody,
		query:      req.URL.Query(),
		status:     resp.StatusCode,
		statusText: statusText(resp.Status, resp.StatusCode),
		proto:      resp.Proto,
		respHeader: resp.Header.Clone(),
		respMime:   resp.Header.Get("Content-Type"),
	}

	// Tee the response body: bytes are captured as ReverseProxy streams them to
	// the client. finalize() runs when the body is closed (copy complete).
	orig := resp.Body
	buf := &bytes.Buffer{}
	resp.Body = &captureBody{
		rc:  orig,
		tee: io.TeeReader(orig, buf),
		buf: buf,
		rec: rec,
	}
	return resp, nil
}

type captureBody struct {
	rc   io.ReadCloser
	tee  io.Reader
	buf  *bytes.Buffer
	rec  *record
	once sync.Once
}

func (c *captureBody) Read(p []byte) (int, error) { return c.tee.Read(p) }

func (c *captureBody) Close() error {
	err := c.rc.Close()
	c.once.Do(func() { c.rec.finalize(c.buf.Bytes()) })
	return err
}

// record holds a snapshot of everything needed to build one HAR entry.
type record struct {
	t          *captureTransport
	start      time.Time
	firstByte  time.Time
	session    string
	method     string
	path       string
	url        string
	reqHeader  http.Header
	reqBody    []byte
	query      url.Values
	status     int
	statusText string
	proto      string
	respHeader http.Header
	respMime   string
}

func (r *record) finalize(respBody []byte) {
	end := time.Now()
	cfg := r.t.cfg

	// Keep time == send + wait + receive exactly (strict validators check this).
	send := 0.0
	wait := ms(r.firstByte.Sub(r.start))
	receive := ms(end.Sub(r.firstByte))

	// HAR content.text must be the *decoded* body. respBody is the raw wire
	// bytes (possibly gzip/deflate); decompress so tools render it. The client
	// still received the original compressed stream via the tee.
	enc := r.respHeader.Get("Content-Encoding")
	content := bodyContent(respBody, r.respMime)
	if decoded, ok := decodeContentEncoding(respBody, enc); ok {
		content = bodyContent(decoded, r.respMime)
		content.Compression = len(decoded) - len(respBody)
	} else if content.Encoding == "base64" && enc != "" {
		log.Printf("note: %s response stored base64 (not decoded) — set -accept-encoding=gzip for a readable HAR", enc)
	}

	entry := HarEntry{
		Pageref:         pageID,
		StartedDateTime: isoTime(r.start),
		Time:            send + wait + receive,
		Request: HarRequest{
			Method: r.method,
			URL:    r.url,
			// Request and response share the connection, so the response's
			// negotiated protocol is the request's actual wire version too.
			HTTPVersion: r.proto,
			Headers:     headerNVs(r.reqHeader, cfg.HideAuth),
			QueryString: queryNVs(r.query),
			Cookies:     requestCookies(r.reqHeader),
			HeadersSize: -1,
			BodySize:    len(r.reqBody),
		},
		Response: HarResponse{
			Status:      r.status,
			StatusText:  r.statusText,
			HTTPVersion: r.proto,
			Headers:     headerNVs(r.respHeader, cfg.HideAuth),
			Cookies:     responseCookies(r.respHeader),
			Content:     content,
			RedirectURL: r.respHeader.Get("Location"),
			HeadersSize: -1,
			BodySize:    len(respBody), // bytes on the wire (compressed)
		},
		Timings: unmeasuredTimings(send, wait, receive),
	}
	if len(r.reqBody) > 0 {
		entry.Request.PostData = &HarPostData{
			MimeType: r.reqHeader.Get("Content-Type"),
			Text:     string(r.reqBody),
		}
	}

	log.Printf("%s %s → %d  session=%s  %.0fms  %s",
		r.method, r.path, r.status, sanitize(r.session), entry.Time, humanBytes(len(respBody)))
	if cfg.Verbose {
		if b, err := json.MarshalIndent(entry, "", "  "); err == nil {
			log.Printf("HAR entry:\n%s", b)
		}
	}

	if err := r.t.store.Add(r.session, entry); err != nil {
		log.Printf("store error [session=%s]: %v", sanitize(r.session), err)
	}
}

// errorEntry records a minimal entry when the upstream round trip fails.
func errorEntry(r *http.Request, cause error) HarEntry {
	return HarEntry{
		Pageref:         pageID,
		StartedDateTime: nowISO(),
		Time:            0,
		Request: HarRequest{
			Method:      r.Method,
			URL:         r.URL.String(),
			HTTPVersion: orDefaultProto(r.Proto),
			Headers:     []HarNV{},
			QueryString: queryNVs(r.URL.Query()),
			Cookies:     requestCookies(r.Header),
			HeadersSize: -1,
			BodySize:    0,
		},
		Response: HarResponse{
			Status:      http.StatusBadGateway,
			StatusText:  "Bad Gateway",
			HTTPVersion: "HTTP/1.1",
			Headers:     []HarNV{},
			Cookies:     []HarCookie{},
			Content:     bodyContent([]byte(cause.Error()), "text/plain"),
			RedirectURL: "",
			HeadersSize: -1,
			BodySize:    0,
		},
		Timings: unmeasuredTimings(0, 0, 0),
	}
}

// decodeContentEncoding decompresses body per the response Content-Encoding.
// Returns (decoded, true) when it handled the encoding, or (body, false) for
// identity/empty or an encoding we can't decode with the stdlib (br, zstd).
func decodeContentEncoding(body []byte, enc string) ([]byte, bool) {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body, false
		}
		defer zr.Close()
		out, err := io.ReadAll(zr)
		if err != nil {
			return body, false
		}
		return out, true
	case "deflate":
		// "deflate" is zlib-wrapped (RFC 1950) in practice, but some servers
		// send raw DEFLATE (RFC 1951) — try zlib first, then raw flate.
		if zr, err := zlib.NewReader(bytes.NewReader(body)); err == nil {
			out, err := io.ReadAll(zr)
			zr.Close()
			if err == nil {
				return out, true
			}
		}
		fr := flate.NewReader(bytes.NewReader(body))
		defer fr.Close()
		if out, err := io.ReadAll(fr); err == nil {
			return out, true
		}
		return body, false
	default:
		return body, false // "", "identity", "br", "zstd", …
	}
}

func orDefaultProto(p string) string {
	if p == "" {
		return "HTTP/1.1"
	}
	return p
}

// statusText extracts "OK" from a "200 OK" status line.
func statusText(status string, code int) string {
	if i := strings.IndexByte(status, ' '); i >= 0 {
		return status[i+1:]
	}
	return http.StatusText(code)
}
