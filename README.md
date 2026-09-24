# Sys32.AI — Stage 8A Multi-Model Support

Stage 8A adds model selection per conversation while keeping the existing Stage 7C reliability and security behavior.

Included:
- Model registry for Qwen 3.8 27B and GPT-OSS 20B.
- Conversation-level model persistence in SQLite.
- Model-aware provider resolution for chat and streaming.
- Authenticated `GET /api/models` discovery endpoint.
- Model selector in the chat header.
- New conversations are created with the selected model.
- Existing conversations lock to their stored model.
- Assistant messages display the actual model used.
- Existing auth, CSRF, quota, rate limiting, stop/cancel, and message lifecycle behavior remain in place.

Local test:

```bash
export AI_PROVIDER=mock
export ALLOW_PAID_APIS=false
export MAX_OUTPUT_TOKENS=800
export MAX_DAILY_TOKENS=150000

go test ./...
go run .
```

The Go model registry and `/api/models` handler are consolidated into `main.go`; no separate `models.go` file is required.
