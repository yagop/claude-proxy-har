# PLAN.md — `claude-har`: record Claude sessions as HAR files

A small CLI that runs a local HTTP proxy. It forwards every request to the
Anthropic API (or any compatible base URL) and records each request/response
pair into a HAR file, grouping requests into one file per session.

Point a Claude client (Claude Code, the SDK, `curl`) at the proxy instead of
`api.anthropic.com`, and you get a replayable HAR log of the whole session.

## Goal

- Transparent reverse proxy: forward untouched, respond untouched.
- Capture every request/response as a HAR 1.2 entry.
- One `.har` file per session, keyed by a request header
  (`X-Claude-Code-Session-Id` by default — Claude Code sends it on every request).
- Route to `ANTHROPIC_BASE_URL` (default `https://api.anthropic.com`), including
  non-Anthropic hosts such as `https://api.z.ai/api/anthropic`.

## Non-goals

- No request/response mutation, no caching, no auth of its own.
- No replay/serving from HAR (record only). Replay can come later.
- No TLS termination — the proxy listens on plain HTTP; the upstream call uses
  HTTPS via the default `http.Transport`.

## Tech stack

- **Go, standard library only.** `net/http` for the listener,
  `net/http/httputil.ReverseProxy` for forwarding + streaming, a custom
  `http.RoundTripper` as the capture point, `encoding/json` for HAR, `flag` +
  `os.Getenv` for config, `os/signal` for graceful shutdown. **No dependencies.**
- Single module, Go 1.22+ (`go.mod` only; nothing in `require`).

## CLI interface

```
claude-har [-port 8787] [-out ./sessions] [-session-header Session-Id]
           [-hide-auth] [-pretty] [-verbose]
```

| Flag / env | Default | Purpose |
|---|---|---|
| `-port` / `PORT` | `8787` | Port to listen on |
| `-out` / `HAR_OUT` | `./sessions` | Directory for `.har` files |
| `ANTHROPIC_BASE_URL` | `https://api.anthropic.com` | Upstream target |
| `-session-header` | `X-Claude-Code-Session-Id` | Header used to group entries into a file |
| `-hide-auth` | on | Redact the authentication header (`x-api-key` / `authorization`); `-hide-auth=false` to keep it |
| `-pretty` | off | Pretty-print the HAR JSON |
| `-verbose` | off | Print the full HAR entry (JSON) for each request to stderr |

Usage:

```
ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic go run . -port 8787
# then:
ANTHROPIC_BASE_URL=http://localhost:8787 claude   # point the client at the proxy
```

## Request flow

The server is an `httputil.ReverseProxy` with two customizations:

- **`Rewrite`** — retarget each request onto `ANTHROPIC_BASE_URL`:
  `r.SetURL(base)` then `r.Out.Host = base.Host`, preserving `pathname + query`.
  Works for any path (`/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`,
  …) and any host. Using `Rewrite` (not `Director`) avoids the default
  `X-Forwarded-For` injection so the request reaches upstream untouched.
- **`FlushInterval = -1`** — flush writes to the client immediately, so SSE
  (`text/event-stream`) responses stream in real time instead of buffering.

`ReverseProxy` already handles hop-by-hop header stripping and streaming copy to
the client. All capture happens in a custom `Transport` (`http.RoundTripper`)
set on the proxy — the one place that sees the full request and response:

1. **Buffer the request body** — read and replace `req.Body` with a fresh
   `io.NopCloser(bytes.NewReader(buf))` so the round trip still sends it. Keep
   `buf` for the HAR entry (Anthropic request bodies are JSON and small).
2. **Time it** — `t0` before `base.RoundTrip(req)`, `tFirstByte` when it returns
   (headers received).
3. **Tee the response body** — replace `resp.Body` with a reader that copies
   bytes into a capture buffer as `ReverseProxy` streams them to the client
   (`io.TeeReader` + a wrapper whose `Close()` finalizes the entry). No double
   read, streaming preserved.
4. **On body EOF/Close** — build the HAR entry (timings, headers, bodies),
   resolve the session key, and hand it to the store.

## HAR mapping

HAR 1.2 (`log.entries[]`). Each entry:

```
{
  startedDateTime,          // RFC3339 from t0
  time,                     // total ms
  request:  { method, url, httpVersion:"HTTP/1.1", headers[], queryString[],
              cookies[], headersSize:-1, bodySize, postData? },
  response: { status, statusText, httpVersion, headers[], cookies[],
              content:{ size, mimeType, text, encoding? }, redirectURL:"",
              headersSize:-1, bodySize },
  cache: {},
  timings: { send:0, wait, receive }   // wait = t0→tFirstByte, receive = →done
}
```

Details:

- Modeled as Go structs with `json` tags; `[]HarNameValue{Name, Value}` for
  headers and query string.
- **postData** — `{mimeType: <request content-type>, text: <utf8 body>}`.
- **response.content.text** — accumulated response bytes as UTF-8. Streaming
  (`text/event-stream`) responses are captured as the full concatenated SSE
  text, which is exactly what a replay needs. Non-UTF-8/binary bodies are
  base64 with `encoding: "base64"`.
- **headers** — the authentication header (`x-api-key` / `authorization`) is
  redacted to `"REDACTED"` by default (`-hide-auth`, on); `-hide-auth=false`
  stores it verbatim.
- `creator: {name: "claude-har", version}`.

## Session grouping & persistence (`store.go`)

- **Key** = value of the `-session-header` header on the request. Missing →
  fall back to `"unknown"` (or a value derived from `metadata.user_id` in the
  body — see Open questions). File path: `<out>/<sanitized-key>.har`.
- **In-memory logs**: `map[string]*HarLog` guarded by a `sync.Mutex`. On first
  use of a key, load an existing file if present so re-runs append to the same
  session.
- **Serialized writes**: the store mutex serializes read-modify-write, so
  concurrent requests can't corrupt a file. Each completed entry is appended to
  the in-memory log, then the whole file is rewritten atomically (write to
  `*.har.tmp`, then `os.Rename`). Full-file rewrite is fine for typical session
  sizes and can be optimized later.
- **Flush on exit**: `signal.Notify` on `SIGINT`/`SIGTERM` → `server.Shutdown`
  (drains in-flight requests), then flush any pending logs.

## Edge cases

- **SSE streaming** — `FlushInterval = -1` + tee; never buffer before the client.
- **Binary / non-UTF-8 bodies** — base64 with `encoding: "base64"`.
- **Upstream errors / non-2xx** — recorded like any other entry (status carried
  through). If `RoundTrip` fails, `ReverseProxy` returns `502`; record an entry
  with the error via `ErrorHandler`.
- **Client disconnect mid-stream** — the tee `Close` runs with a partial body;
  store what was captured and flag it.
- **Redirects** — upstream 3xx passes through as-is and is recorded (the proxy
  does not follow them).
- **Filename safety** — sanitize the session key (strip `/`, `..`, control chars)
  before building the path.

## Project layout

```
claude-proxy-har/
  go.mod                # module, Go 1.22+, no require block
  main.go               # flag/env parsing, signal handling, http.Server
  proxy.go              // ReverseProxy setup + capture RoundTripper
  har.go                // HAR structs, entry building, redaction, base64
  store.go              // per-session logs + serialized atomic file writes
  README.md
```

Flat package `main` — fewest files, no premature `internal/` split.

## Milestones

1. **Proxy passthrough** — `ReverseProxy` forwards to `ANTHROPIC_BASE_URL` with
   `FlushInterval = -1`. Verify Claude Code works through it end-to-end.
2. **HAR capture (non-streaming)** — capture Transport records
   `/v1/messages/count_tokens` and `/v1/models` round-trips to one file.
3. **Streaming capture** — tee a real `stream:true` `/v1/messages` response;
   confirm the client streams normally and the SSE body is captured intact.
4. **Session grouping** — split entries by header into per-session files with
   serialized atomic writes; append on restart.
5. **Polish** — `-hide-auth`, `-pretty`, `-verbose`, graceful shutdown flush,
   filename sanitization, base64 for binary.

## Verification

- `go vet ./...` and `go build` clean.
- Load a produced `.har` in Chrome DevTools (Import HAR) and an online HAR
  viewer — must parse as valid HAR 1.2.
- Diff captured request/response bytes against the real API for a fixed prompt.
- Confirm two interleaved sessions land in two separate files.

## Open questions

1. **Which header carries the session id?** *(resolved)* Claude Code sends
   `X-Claude-Code-Session-Id` on every request, so that is the default. The flag
   stays configurable; requests without the header fall back to `unknown.har`.
2. **One file per session vs. append across runs** — plan assumes append (load
   existing file on startup). Confirm this is desired over per-run files.
