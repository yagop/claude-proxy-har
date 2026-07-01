# claude-har

A tiny reverse proxy that records Claude API sessions as [HAR](http://www.softwareishard.com/blog/har-12-spec/) files.

Point a Claude client at the proxy instead of `api.anthropic.com`, and every
request/response is captured into a `.har` file — one file per session.

## Build & run

```sh
go build -o claude-har .
./claude-har -port 8787 -out ./sessions
```

Then point the client at the proxy:

```sh
ANTHROPIC_BASE_URL=http://localhost:8787 claude
```

Route to a non-Anthropic upstream by setting `ANTHROPIC_BASE_URL` on the proxy:

```sh
ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic ./claude-har -port 8787
```

## Flags & env

| Flag / env | Default | Purpose |
|---|---|---|
| `-port` / `PORT` | `8787` | Port to listen on |
| `-out` / `HAR_OUT` | `./sessions` | Directory for `.har` files |
| `ANTHROPIC_BASE_URL` | `https://api.anthropic.com` | Upstream target |
| `-session-header` | `X-Claude-Code-Session-Id` | Request header used to group entries into a file |
| `-redact` | off | Redact `x-api-key` / `authorization` in stored headers |
| `-pretty` | off | Pretty-print the HAR JSON |
| `-verbose` | off | Log each proxied request to stderr |

## How it works

- `httputil.ReverseProxy` retargets each request onto `ANTHROPIC_BASE_URL`
  (`FlushInterval = -1` so SSE streams in real time).
- A custom `http.RoundTripper` buffers the request body and tees the response
  body, building a HAR entry once the response has fully streamed to the client.
- Entries are grouped by the `-session-header` value (default
  `X-Claude-Code-Session-Id`, which Claude Code sends on every request) and
  written to `<out>/<session>.har`. Missing header → `unknown.har`. Re-runs
  append to the existing file.
- Output is valid HAR 1.2 and imports into Safari Web Inspector (Network →
  Import), Chrome DevTools, and Firefox.

Load a produced `.har` in Chrome DevTools (Network tab → Import HAR) to inspect.
