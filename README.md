# Zephyr Helper

Small container that runs on your TrueNAS Scale box and extends the Zephyr iOS app with on-NAS capabilities:

- **AI Assistant** — chat with your NAS, grounded in live data
- More coming — smart alerts, automation rules, config time machine, scripted actions, log search

Everything stays on your hardware. No cloud, no telemetry.

## How it works

Zephyr Helper is a tiny Go HTTP service. It bridges chat messages from the Zephyr iOS app to a local LLM running in [Ollama](https://ollama.com), and (from Phase B onward) exposes a curated set of tools that read your NAS state through the TrueNAS REST API.

```
┌──────────────┐   HTTPS (LAN)    ┌──────────────────┐
│  Zephyr iOS  │ ←──────────────→ │  zephyr-helper   │ ──┬── Ollama
└──────────────┘   bearer token   │  Go, port 8080   │  │
                                  └──────────────────┘  └── TrueNAS REST (Phase B+)
```

## Installation

The normal path is installing from inside the Zephyr iOS app — it handles catalog install, Ollama dependency, and pairing automatically.

Manual install docs land alongside the v1.0 release (Phase E).

## Roadmap

- [x] **Phase A — scaffolding.** HTTP + WebSocket, Ollama bridge, bearer auth, health/version endpoints.
- [x] **Phase B — tool palette (current).** Read-only tools: `list_pools`, `pool_status`, `list_apps`, `list_disks`, `list_alerts`, `system_info`. Multi-turn Ollama tool-calling loop. Requires `TRUENAS_URL` + `TRUENAS_API_KEY`.
- [x] **Phase C — iOS chat UI.** Streaming responses with status indicator during tool calls.
- [x] **Phase D — in-app install + auto-pairing.** Zephyr iOS app mints a dedicated TrueNAS API key and deploys the helper automatically.
- [ ] **Phase E — Demo Mode, docs, tagged ghcr.io releases.**
- [ ] **v1.1 — Smart alerts + event archive.**
- [ ] **v1.2 — Automation rules.**
- [ ] **v1.3 — Config time machine.**
- [ ] **v1.4 — Scripted quick actions + log search.**

## Development

Requires Go 1.22+. For chat to work locally you also need an [Ollama](https://ollama.com) install with a model pulled.

```bash
git clone https://github.com/donnie123421/zephyr-helper
cd zephyr-helper
go mod tidy
OLLAMA_MODEL=qwen2.5:7b-instruct go run ./cmd/server
```

On first boot with no `PAIRING_TOKEN` env set, the server mints one and prints it to stderr so you can pair by hand.

### Configuration

| Variable | Default | Purpose |
|---|---|---|
| `ZEPHYR_ADDR` | `:8080` | Listen address |
| `PAIRING_TOKEN` | auto-generated on boot | Bearer token; paired client sends `Authorization: Bearer <token>` |
| `OLLAMA_URL` | `http://ollama:11434` | Ollama HTTP endpoint |
| `OLLAMA_MODEL` | `qwen2.5:7b-instruct` | Default model |
| `TRUENAS_URL` | — | TrueNAS host (used from Phase B onward) |
| `TRUENAS_API_KEY` | — | TrueNAS API key (used from Phase B onward) |
| `DATA_DIR` | `/data` | Directory for persistent state (events DB, push prefs, pool capacity history, TLS certificate, model settings). The iOS installer mounts a named volume here; falls back to `/tmp` if unwritable. |
| `EVENTS_DB_PATH` | `{DATA_DIR}/events.db` | SQLite path for the event store. Override for tests/manual layouts. |
| `EVENTS_RETENTION_DAYS` | `90` | Daily pruner deletes events older than this. `0` disables pruning. |
| `PUSH_RELAY_URL` | — | Base URL of the zephyr-push-relay Worker. Set by the iOS installer. Empty = push disabled. |
| `PUSH_SUBSCRIPTION_ID` | — | OneSignal subscription id for the paired iPhone. Set by the iOS installer. |
| `PUSH_INSTALL_TOKEN` | — | Per-device secret the relay's allowlist validates. Set by the iOS installer. |

### Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/health` | no | Liveness |
| GET | `/version` | no | Reports running version + commit SHA, for update detection |
| GET | `/tls?nonce=<hex>` | no | Certificate fingerprint plus proof of the pairing token (see [HTTPS](#https)) |
| POST | `/auth/verify` | yes | Token-check round-trip |
| GET (WS) | `/chat` | yes | Streaming chat |
| GET | `/model` | yes | Current Ollama model |
| POST | `/model` | yes | Swap the active model |
| GET | `/events` | yes | Event feed (v1.1 scaffold — pollers land next) |
| GET | `/events/{id}` | yes | Single event detail |
| POST | `/events/{id}/ack` | yes | Acknowledge an event |
| POST | `/events/ack_all` | yes | Acknowledge every un-acked event; returns the count |
| GET (WS) | `/events/stream` | yes | Live event stream |
| GET | `/push/prefs` | yes | Muted push categories (iOS-synced) |
| POST | `/push/prefs` | yes | Replace muted categories: `{"muted_categories": ["ALERT_WARNING"]}` |
| GET | `/llm/config` | yes | Model provider, model, and whether a key is saved (last four characters only) |
| POST | `/llm/config` | yes | Choose the provider and model, save or remove a key (see [Models](#models)) |
| POST | `/llm/test` | yes | Check an Anthropic key by listing its models; `{"ok": false, "error": …}` when it's rejected |
| GET | `/llm/models` | yes | Models the saved Anthropic key can use |

### HTTPS

The helper answers plain HTTP and HTTPS on the same port, telling them apart by each connection's first byte, so existing installs need no compose change and older app versions keep working over HTTP.

The certificate is self-signed and generated on first start (`tls-cert.pem` and `tls-key.pem` in `DATA_DIR`). Clients pin its SHA-256 fingerprint (of the DER bytes, lowercase hex) rather than trusting a CA. To pin it without sending the pairing token over an unverified connection, a client connects over HTTPS, sends `GET /tls?nonce=<32–128 hex chars>`, and checks the reply

```json
{"fingerprint_sha256": "…", "proof": "…"}
```

where `proof` is `hex(HMAC-SHA256(key: pairing token, message: "zephyr-helper-tls-v1\n" + fingerprint + "\n" + nonce))`, computed with the fingerprint the client actually saw in the handshake. A proxy in the middle can't produce a valid proof for its own certificate without the token.

When the helper is on a tailnet (`TS_AUTHKEY` set), it also serves the same API on its own tailnet address, port 8080.

### Models

Chats run on Ollama unless the app saves another provider. Today that's Claude (`anthropic`) with the user's own API key, through Anthropic's official Go SDK; the default model is `claude-opus-5`. The choice and the key live in `llm.json` in `DATA_DIR`, readable only by the helper. Keys go in through `POST /llm/config` and never come back out: responses carry only whether a key is saved and its last four characters. The helper only accepts a key over HTTPS.

```json
{"provider": "anthropic", "anthropic": {"api_key": "sk-ant-…", "model": "claude-opus-5"}}
{"provider": "ollama", "anthropic": {"remove_key": true}}
```

A chat keeps the model it started with; a change applies from the next chat.

### Wire format

Client → server:
```json
{"type": "user_message", "content": "What is 2+2?"}
```

Server → client (streamed):
```json
{"type": "delta", "content": "2"}
{"type": "delta", "content": " plus "}
{"type": "delta", "content": "2"}
{"type": "delta", "content": " = 4."}
{"type": "usage", "model": "claude-opus-5", "input_tokens": 1250, "output_tokens": 180, "cache_read_tokens": 900, "cache_write_tokens": 0}
{"type": "done"}
```

`usage` comes before `done` for models that count tokens (not Ollama), even when the turn fails partway. Clients that don't know a frame type skip it.

An error ends the turn, not the chat — the client can send the next message on the same connection:
```json
{"type": "error", "message": "Anthropic rejected the API key. Check it in the assistant's settings."}
```

### Docker

```bash
docker build -t zephyr-helper:dev .
docker run --rm -p 8080:8080 \
  -e OLLAMA_URL=http://host.docker.internal:11434 \
  -e OLLAMA_MODEL=qwen2.5:7b-instruct \
  zephyr-helper:dev
```

## License

MIT — see [LICENSE](LICENSE).
