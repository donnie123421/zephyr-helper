# CLAUDE.md

Guidance for Claude Code when working on Zephyr Helper — the Go service that runs on TrueNAS and bridges the Zephyr iOS app to a local Ollama instance.

## Build & Run
- Requires Go 1.22+
- Refresh deps: `go mod tidy`
- Quick validation: `go vet ./... && go build ./...`
- Local dev: `OLLAMA_URL=http://localhost:11434 OLLAMA_MODEL=llama3.1:8b go run ./cmd/server`
- Docker: `docker build -t zephyr-helper:dev .`
- After `go.mod` changes, always run `go mod tidy` before committing — CI runs `go vet` against whatever's checked in

## Architecture

### Layout
- `cmd/server/` — entry point, HTTP mux, graceful shutdown
- `internal/config/` — env-based config loading, auto-generates a pairing token on first boot if none supplied
- `internal/auth/` — constant-time bearer-token middleware
- `internal/chat/` — WebSocket handler, bridges client messages to Ollama
- `internal/ollama/` — streaming client for Ollama's `/api/chat` endpoint
- `internal/version/` — build-time version string (stamped via `-ldflags`)

### Wire protocol
JSON tagged unions over WebSocket. Client → server: `{"type":"user_message","content":"..."}`. Server → client: zero or more `{"type":"delta","content":"..."}` followed by `{"type":"done"}`, or `{"type":"error","message":"..."}` on failure. Keep the shape forward-compatible — iOS will switch on `type` and later phases add `tool_call` events.

### Transport
- Stdlib `net/http` with Go 1.22 `ServeMux` route patterns (`GET /health`, etc)
- `github.com/coder/websocket` for WebSocket handling
- No HTTP frameworks, no routers — keep deps minimal so the image stays small

## Conventions
- All new code goes in `internal/` — no public API surface outside the binary
- 12-factor config: every tunable is an env var, documented in `README.md`'s config table
- Streaming producers `defer close(out)` so consumers can `range` without explicit cancellation logic
- Bearer auth via `internal/auth.Require` on every endpoint that touches state or inference — `/health` and `/version` stay unauth
- JSON over HTTP for all interfaces — no protobuf/gRPC until there's a real need
- Pass dependencies explicitly; no global state beyond what `main.go` wires up

## Phase / roadmap
See `README.md`. Current phase: **A** — HTTP + WebSocket scaffolding, Ollama bridge, no TrueNAS tools yet. Phase B adds the tool palette.

## Release flow
- Merge to `main` → CI publishes `:main` + `:sha-<sha>` tags to `ghcr.io/donnie123421/zephyr-helper`
- `git tag vX.Y.Z && git push origin vX.Y.Z` → CI also publishes `:vX.Y.Z`, `:X.Y.Z`, `:X.Y`
- Never force-push main; never delete published tags

## Related
- iOS app repo (private): companion client, connects via WebSocket with bearer auth
- Image registry: `ghcr.io/donnie123421/zephyr-helper` (public)
- Ollama API reference: https://github.com/ollama/ollama/blob/main/docs/api.md
