# Sys32.AI — Stage 7C

> **Live Website:** https://sys32-ai.duckdns.org
>
> **Try the live application and share your feedback.** Bug reports, UI feedback, performance observations, and suggestions are welcome.

Stage 7C builds on Stage 7B with persistent assistant-message lifecycle state and recovery after browser refresh, network interruption, or an unexpected server restart.

## Included

- Persists an assistant message as `pending` before streaming begins.
- Updates that same assistant row to `completed`, `cancelled`, or `failed` instead of inserting duplicate assistant rows.
- Preserves partial streamed output when a generation is cancelled or fails.
- Reconciles stale pending assistant messages older than two minutes into a visible failed state when conversation history is opened.
- Loads message status into the UI so interrupted/failed generations survive refresh instead of disappearing.
- Keeps Stage 7B per-conversation generation locking and all earlier authentication, user isolation, quota, CSRF, and password-management behavior.

## Live Demo & Feedback

The current deployed version is available here:

**https://sys32-ai.duckdns.org**

Please try the application and share feedback about:

- Chat responsiveness and streaming behavior
- Stop/cancellation behavior
- Login, registration, and session handling
- Conversation persistence and recovery
- UI/UX issues
- Errors or unexpected behavior

## Local test

```bash
export AI_PROVIDER=mock
export ALLOW_PAID_APIS=false
export MAX_OUTPUT_TOKENS=800
export MAX_DAILY_TOKENS=150000

go test ./...
go run .
```

Use the mock provider during local development so no external AI quota is consumed.
