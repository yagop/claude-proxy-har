package main

import (
	"bytes"
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
	Base          *url.URL
	SessionHeader string
	HideAuth      bool
	Verbose       bool
}

// newProxy builds a ReverseProxy that retargets onto cfg.Base, streams
// responses immediately (FlushInterval = -1, required for SSE), and captures
// every round trip via captureTransport.
func newProxy(cfg *Config, store *Store) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(cfg.Base)         // scheme + host + base-path join
			r.Out.Host = cfg.Base.Host // upstream Host header (not the inbound one)
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

	entry := HarEntry{
		Pageref:         pageID,
		StartedDateTime: isoTime(r.start),
		Time:            send + wait + receive,
		Request: HarRequest{
			Method:      r.method,
			URL:         r.url,
			HTTPVersion: "HTTP/1.1",
			Headers:     headerNVs(r.reqHeader, cfg.HideAuth),
			QueryString: queryNVs(r.query),
			Cookies:     []HarNV{},
			HeadersSize: -1,
			BodySize:    len(r.reqBody),
		},
		Response: HarResponse{
			Status:      r.status,
			StatusText:  r.statusText,
			HTTPVersion: r.proto,
			Headers:     headerNVs(r.respHeader, cfg.HideAuth),
			Cookies:     []HarNV{},
			Content:     bodyContent(respBody, r.respMime),
			RedirectURL: r.respHeader.Get("Location"),
			HeadersSize: -1,
			BodySize:    len(respBody),
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
			HTTPVersion: "HTTP/1.1",
			Headers:     []HarNV{},
			QueryString: queryNVs(r.URL.Query()),
			Cookies:     []HarNV{},
			HeadersSize: -1,
			BodySize:    0,
		},
		Response: HarResponse{
			Status:      http.StatusBadGateway,
			StatusText:  "Bad Gateway",
			HTTPVersion: "HTTP/1.1",
			Headers:     []HarNV{},
			Cookies:     []HarNV{},
			Content:     bodyContent([]byte(cause.Error()), "text/plain"),
			RedirectURL: "",
			HeadersSize: -1,
			BodySize:    0,
		},
		Timings: unmeasuredTimings(0, 0, 0),
	}
}

// statusText extracts "OK" from a "200 OK" status line.
func statusText(status string, code int) string {
	if i := strings.IndexByte(status, ' '); i >= 0 {
		return status[i+1:]
	}
	return http.StatusText(code)
}
