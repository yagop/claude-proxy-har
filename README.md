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
| `-accept-encoding` | passthrough | Override the outbound `Accept-Encoding` (e.g. `identity` to disable compression). Empty forwards the client's header unchanged |
| `-hide-auth` | **on** | Redact the authentication header (`x-api-key` / `authorization`) in stored HARs. Pass `-hide-auth=false` to keep it |
| `-pretty` | off | Pretty-print the HAR JSON |
| `-verbose` | off | Print the full HAR entry (JSON) for each request to stderr |

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
- Compressed responses (`gzip`/`deflate`) are stored **decompressed** in
  `content.text` so viewers render the JSON (the client still receives the
  original compressed stream). `br`/`zstd` aren't decoded (stdlib-only) — force
  a decodable encoding with `-accept-encoding=gzip` or `-accept-encoding=identity`
  if your upstream negotiates those.

Load a produced `.har` in Chrome DevTools (Network tab → Import HAR) to inspect.

## Extracting system prompts

Because the full request body is captured, you can pull the exact system prompt
Claude Code sends for each model. Run it once per model through the proxy, then
extract with `jq`.

```sh
# 1. Proxy running on :8787, writing to ./sessions
./claude-har -port 8787 -out ./sessions &

# 2. One capture per model (--no-session-persistence → each run gets its own .har)
mkdir -p system-prompts
for m in claude-opus-4-8 claude-sonnet-5 claude-haiku-4-5; do
  ANTHROPIC_BASE_URL=http://localhost:8787 bun x @anthropic-ai/claude-code \
    --no-session-persistence --safe-mode -p "hi" --model="$m" \
    --exclude-dynamic-system-prompt-sections
done
```

`--exclude-dynamic-system-prompt-sections` drops volatile bits (date, cwd, env)
so the prompt is stable across runs.

Then extract each model's main-agent prompt from the captured HARs — the request
body is JSON stored as a string (`fromjson`), and `system` is an array of blocks.
Select the longest entry whose `.model` matches and join its blocks:

```sh
for m in claude-opus-4-8 claude-sonnet-5 claude-haiku-4-5; do
  jq -rs --arg m "$m" '
    [ .[].log.entries[]
      | (.request.postData.text? // empty) | fromjson?
      | select(.model == $m and (.system | type == "array"))
      | (.system | map(.text) | join("\n\n")) ]
    | max_by(length) // empty
  ' sessions/*.har > "system-prompts/$m.md"
done
```

Or grab one model's prompt straight from a single session file:

```sh
jq -r '.log.entries[0].request.postData.text | fromjson | .system | map(.text) | join("\n\n")' \
  sessions/<session-id>.har
```

> `system[0]` is Claude Code's SDK/billing marker
> (`x-anthropic-billing-header: …`); the instruction prose follows in the next
> blocks. Newer models get a shorter prompt (e.g. Opus 4.8 is far leaner than
> Sonnet 4.6).
