package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// HAR 1.2 structures. See http://www.softwareishard.com/blog/har-12-spec/

type HAR struct {
	Log HarLog `json:"log"`
}

type HarLog struct {
	Version string     `json:"version"`
	Creator HarCreator `json:"creator"`
	Pages   []HarPage  `json:"pages"`
	Entries []HarEntry `json:"entries"`
}

type HarCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type HarPage struct {
	StartedDateTime string         `json:"startedDateTime"`
	ID              string         `json:"id"`
	Title           string         `json:"title"`
	PageTimings     HarPageTimings `json:"pageTimings"`
}

type HarPageTimings struct {
	OnContentLoad float64 `json:"onContentLoad"`
	OnLoad        float64 `json:"onLoad"`
}

type HarEntry struct {
	Pageref         string      `json:"pageref"`
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         HarRequest  `json:"request"`
	Response        HarResponse `json:"response"`
	Cache           struct{}    `json:"cache"`
	Timings         HarTimings  `json:"timings"`
}

type HarTimings struct {
	Blocked float64 `json:"blocked"`
	DNS     float64 `json:"dns"`
	Connect float64 `json:"connect"`
	SSL     float64 `json:"ssl"`
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

// unmeasuredTimings returns a HarTimings with the phases we don't track set to
// -1 ("not applicable", per the HAR spec). WebKit's importer treats any value
// != -1 as a real phase and would otherwise compute NaN offsets.
func unmeasuredTimings(send, wait, receive float64) HarTimings {
	return HarTimings{
		Blocked: -1, DNS: -1, Connect: -1, SSL: -1,
		Send: send, Wait: wait, Receive: receive,
	}
}

type HarNV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HarRequest struct {
	Method      string       `json:"method"`
	URL         string       `json:"url"`
	HTTPVersion string       `json:"httpVersion"`
	Headers     []HarNV      `json:"headers"`
	QueryString []HarNV      `json:"queryString"`
	Cookies     []HarNV      `json:"cookies"`
	HeadersSize int          `json:"headersSize"`
	BodySize    int          `json:"bodySize"`
	PostData    *HarPostData `json:"postData,omitempty"`
}

type HarPostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

type HarResponse struct {
	Status      int        `json:"status"`
	StatusText  string     `json:"statusText"`
	HTTPVersion string     `json:"httpVersion"`
	Headers     []HarNV    `json:"headers"`
	Cookies     []HarNV    `json:"cookies"`
	Content     HarContent `json:"content"`
	RedirectURL string     `json:"redirectURL"`
	HeadersSize int        `json:"headersSize"`
	BodySize    int        `json:"bodySize"`
}

type HarContent struct {
	Size        int    `json:"size"`
	Compression int    `json:"compression,omitempty"`
	MimeType    string `json:"mimeType"`
	Text        string `json:"text,omitempty"`
	Encoding    string `json:"encoding,omitempty"`
}

const pageID = "page_1"

func newHarLog() *HarLog {
	return &HarLog{
		Version: "1.2",
		Creator: HarCreator{Name: creatorName, Version: version},
		Pages:   []HarPage{newPage()},
		Entries: []HarEntry{},
	}
}

func newPage() HarPage {
	return HarPage{
		StartedDateTime: nowISO(),
		ID:              pageID,
		Title:           creatorName,
		PageTimings:     HarPageTimings{OnContentLoad: -1, OnLoad: -1},
	}
}

// headerNVs flattens http.Header into sorted name/value pairs, redacting
// sensitive values when asked.
func headerNVs(h http.Header, redact bool) []HarNV {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]HarNV, 0, len(h))
	for _, k := range keys {
		for _, v := range h[k] {
			if redact && isSensitive(k) {
				v = "REDACTED"
			}
			out = append(out, HarNV{Name: k, Value: v})
		}
	}
	return out
}

func isSensitive(k string) bool {
	switch strings.ToLower(k) {
	case "x-api-key", "authorization":
		return true
	}
	return false
}

func queryNVs(q url.Values) []HarNV {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]HarNV, 0)
	for _, k := range keys {
		for _, v := range q[k] {
			out = append(out, HarNV{Name: k, Value: v})
		}
	}
	return out
}

// bodyContent stores a body as UTF-8 text, or base64 when it isn't valid UTF-8.
func bodyContent(b []byte, mimeType string) HarContent {
	c := HarContent{Size: len(b), MimeType: mimeType}
	if utf8.Valid(b) {
		c.Text = string(b)
	} else {
		c.Text = base64.StdEncoding.EncodeToString(b)
		c.Encoding = "base64"
	}
	return c
}

// isoMillis is RFC3339 with millisecond precision — the format Chrome emits and
// the one strict importers (.NET/Java/regex-based) reliably parse. Nanosecond
// fractions are valid RFC3339 but choke some consumers.
const isoMillis = "2006-01-02T15:04:05.000Z07:00"

func isoTime(t time.Time) string { return t.UTC().Format(isoMillis) }

func nowISO() string { return isoTime(time.Now()) }

func ms(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}
