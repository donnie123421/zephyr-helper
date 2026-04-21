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

- [x] **Phase A — scaffolding (current).** HTTP + WebSocket, Ollama bridge, bearer auth, health/version endpoints. No TrueNAS tools yet.
- [ ] **Phase B — tool palette.** `get_pool_status`, `get_alerts`, `get_disks`, `get_recent_jobs`, `get_reporting`, `get_datasets`, `get_system_info`.
- [ ] **Phase C — iOS chat UI.** Streaming responses with tool-call indicator.
- [ ] **Phase D — in-app install + auto-pairing.**
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
OLLAMA_MODEL=llama3.1:8b go run ./cmd/server
```

On first boot with no `PAIRING_TOKEN` env set, the server mints one and prints it to stderr so you can pair by hand.

### Configuration

| Variable | Default | Purpose |
|---|---|---|
| `ZEPHYR_ADDR` | `:8080` | Listen address |
| `PAIRING_TOKEN` | auto-generated on boot | Bearer token; paired client sends `Authorization: Bearer <token>` |
| `OLLAMA_URL` | `http://ollama:11434` | Ollama HTTP endpoint |
| `OLLAMA_MODEL` | `llama3.1:8b` | Default model |
| `TRUENAS_URL` | — | TrueNAS host (used from Phase B onward) |
| `TRUENAS_API_KEY` | — | TrueNAS API key (used from Phase B onward) |

### Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/health` | no | Liveness |
| GET | `/version` | no | Reports running version, for update detection |
| POST | `/auth/verify` | yes | Token-check round-trip |
| GET (WS) | `/chat` | yes | Streaming chat |

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
{"type": "done"}
```

Errors terminate the stream:
```json
{"type": "error", "message": "ollama: 500 Internal Server Error"}
```

### Docker

```bash
docker build -t zephyr-helper:dev .
docker run --rm -p 8080:8080 \
  -e OLLAMA_URL=http://host.docker.internal:11434 \
  -e OLLAMA_MODEL=llama3.1:8b \
  zephyr-helper:dev
```

## License

MIT — see [LICENSE](LICENSE).
