# Sys32.AI — Scalable AI Chatbot

> A lightweight, self-hosted AI chat application built with **Go, HTMX/vanilla JavaScript, SQLite, Docker, Nginx, DuckDNS, and Groq-hosted models**.

## Project Overview

**Sys32.AI** is a web-based AI chatbot designed to provide a fast, low-overhead chat experience while keeping the application architecture simple and deployable on a small cloud VM.

The project separates two important parts of development:

1. **Application development** — the chatbot application code, UI, backend logic, authentication, streaming logic, conversation handling, and related implementation were created with the help of AI.
2. **Deployment and infrastructure** — the containerization, deployment configuration, cloud setup, networking, domain configuration, and production operation were created and managed by me.

This distinction is intentional: the project demonstrates both **AI-assisted software development** and **hands-on DevOps/deployment work**.

---

## AI-Assisted Development

A major part of this project was developed through AI-assisted coding.

The following application areas were generated/implemented with AI assistance:

- Go application/backend code
- Web UI structure and interaction logic
- Authentication and session handling
- SQLite persistence layer
- Conversation management
- Streaming response handling
- Stop/cancellation behavior
- Message lifecycle management
- Usage and quota handling
- Error handling and validation
- Frontend JavaScript behavior
- Styling and UI refinement
- Tests and development utilities

The goal was not to hide the use of AI. Instead, Sys32.AI demonstrates how AI can be used as a software-development tool while the developer remains responsible for the infrastructure, deployment, configuration, and operational side of the system.

---

## My Contribution: Deployment & DevOps

The deployment layer was created and managed by me.

### Infrastructure

- Oracle Cloud VM
- Ubuntu Linux
- Docker
- Docker Compose
- Nginx reverse proxy
- DuckDNS domain
- HTTPS configuration
- Persistent application storage
- Environment/secrets configuration
- Containerized production deployment
- Server-side troubleshooting and monitoring

### Containerization

I created the Docker deployment configuration used to package and run the application, including:

- `Dockerfile`
- `compose.yml`
- persistent SQLite volume mapping
- runtime environment configuration
- container restart policy
- localhost-only application binding behind Nginx

The deployment intentionally keeps the application inaccessible directly on the public network. The Go server runs inside Docker and is exposed locally, while Nginx handles public HTTP/HTTPS traffic.

### Public Request Flow

```text
Internet
   ↓
DuckDNS domain
   ↓
Nginx :80 / :443
   ↓
127.0.0.1:8080
   ↓
Docker container
   ↓
Sys32.AI Go application
   ↓
Groq API
   ↓
AI model
```

---

## Why Sys32.AI Feels Fast

The responsiveness of Sys32.AI is **not primarily because a small or lightweight AI model is being used**.

The AI model is hosted remotely by Groq. The model still performs the actual inference remotely, and model generation speed depends on the selected model and provider.

The **application itself is lightweight and responsive because of its software architecture**.

### 1. Go backend

The backend is implemented in Go rather than a large Node.js application stack.

Go provides:

- fast startup
- low runtime overhead
- efficient concurrency
- simple deployment as a compiled binary
- low memory requirements
- good fit for long-running HTTP services

This makes the application practical on a small cloud VM.

### 2. HTMX / minimal JavaScript approach

The application does not depend on a large client-side JavaScript framework for the core chat experience.

HTMX and small amounts of vanilla JavaScript are used where appropriate, keeping the browser-side application relatively small.

This reduces:

- client-side JavaScript complexity
- frontend build overhead
- dependency overhead
- unnecessary browser-side processing

### 3. Server-Sent Events for streaming

AI responses are streamed to the browser instead of waiting for the entire response before displaying anything.

The flow is:

```text
Groq streaming response
        ↓
Go SSE handler
        ↓
Browser
        ↓
Token-by-token UI update
```

This makes the interface feel responsive even when the model is generating a longer answer.

### 4. SQLite instead of a separate database server

SQLite keeps the persistence layer simple and lightweight.

There is no separate PostgreSQL/MySQL server required for the current deployment.

The database stores application data such as:

- users
- sessions
- conversations
- messages
- AI usage information

### 5. Containerized deployment

Docker makes the runtime environment reproducible without requiring Go and all application dependencies to be manually installed on the Oracle VM.

---

## Technology Stack

| Layer | Technology |
|---|---|
| Backend | Go |
| Frontend interaction | HTMX + Vanilla JavaScript |
| Markup | HTML |
| Styling | CSS |
| Database | SQLite |
| AI API | Groq |
| AI models | Qwen / other supported Groq models |
| Streaming | Server-Sent Events (SSE) |
| Containerization | Docker |
| Orchestration | Docker Compose |
| Reverse Proxy | Nginx |
| DNS | DuckDNS |
| Cloud | Oracle Cloud |
| OS | Ubuntu Linux |

---

## Current AI Integration

The application uses a provider abstraction so the UI and core chat architecture are not tightly coupled to one model.

A current deployment configuration can use:

```env
AI_PROVIDER=groq
GROQ_MODEL=qwen/qwen3.8-27b
ALLOW_PAID_APIS=false
MAX_OUTPUT_TOKENS=800
MAX_DAILY_TOKENS=150000
```

The application is designed around a zero-paid-API objective for the current project configuration. Provider availability and free-tier limits can change, so the exact model used by a deployment should be configured through environment variables rather than hard-coded assumptions.

---

## Core Features

### Authentication

- User registration
- User login/logout
- Session-based authentication
- CSRF protection
- Password hashing
- Password change support
- Session invalidation after password change

### Conversations

- Persistent conversations
- Per-user conversation ownership
- Conversation titles
- Conversation history
- Message persistence
- Conversation loading
- Conversation deletion

### Streaming Chat

- AI response streaming
- Server-Sent Events
- Fast/Think modes
- Model/provider metadata
- Partial response handling
- Failed/cancelled response persistence
- Stop generation

### Reliability

- Request IDs
- Rate limiting
- Concurrent generation protection
- Daily token budget
- AI slot limiting
- Graceful stream cancellation
- Stale pending-message recovery
- Provider error handling
- SQLite persistence

---

## Message Lifecycle

Assistant messages use an explicit lifecycle instead of creating duplicate rows for every generation state.

```text
pending
   ↓
completed
```

or:

```text
pending
   ↓
failed
```

or:

```text
pending
   ↓
cancelled
```

This allows interrupted or cancelled generations to remain visible in conversation history instead of leaving the UI permanently stuck in a generating state.

---

## Security Approach

Current security controls include:

- Argon2id password hashing
- HttpOnly session cookies
- CSRF protection
- SameSite cookies
- Session expiration
- Session invalidation on password change
- User-owned conversations
- Rate limiting
- Request size limits
- AI concurrency limits
- Secure HTTP response headers
- Content Security Policy
- Secrets kept outside source code
- SQLite database kept outside the Git repository
- API key stored through environment configuration
- Go application bound to localhost behind Nginx in production

---

## Deployment

The application is intended to run as a Dockerized service on a small Linux VM.

### Local Docker deployment

```bash
docker compose up -d --build
```

The container exposes the application internally on:

```text
127.0.0.1:8080
```

### Production reverse proxy

Nginx provides the public entry point:

```text
https://sys32-ai.duckdns.org
```

and proxies requests to:

```text
http://127.0.0.1:8080
```

The public application therefore uses the standard web ports while the Go service remains private to the server.

---

## Environment Variables

Create a local `.env` file from the example configuration.

```env
APP_ADDR=:8080
AI_PROVIDER=groq
GROQ_MODEL=qwen/qwen3.8-27b
GROQ_API_KEY=your_groq_api_key
ALLOW_PAID_APIS=false
MAX_OUTPUT_TOKENS=800
MAX_DAILY_TOKENS=150000
DATABASE_PATH=data/sys32.db
COOKIE_SECURE=true
```

**Never commit `.env` or the production SQLite database to GitHub.**

---

## Project Structure

```text
Sys32.AI/
├── main.go
├── main_test.go
├── go.mod
├── go.sum
├── Dockerfile
├── compose.yml
├── .env.example
├── .gitignore
├── README.md
├── static/
│   ├── app.css
│   ├── app.js
│   ├── auth.css
│   └── auth.js
├── templates/
│   ├── index.html
│   ├── login.html
│   └── register.html
└── data/
    └── sys32.db
```

The `data/` directory is runtime storage and should not be committed to source control.

---

## Development Philosophy

Sys32.AI follows a simple principle:

> **Keep the application small, keep the deployment reproducible, and separate AI inference from application infrastructure.**

The project intentionally avoids building a heavyweight frontend stack when the chat experience can be delivered with Go, HTML, HTMX, small amounts of JavaScript, SSE, and SQLite.

This makes it suitable for:

- small cloud VMs
- educational demonstrations
- AI-assisted development experiments
- DevOps demonstrations
- low-overhead web applications

---

## Future Roadmap

Possible future development includes:

- multi-model UI selection
- improved conversation context management
- regeneration and editing of messages
- file uploads
- document analysis
- retrieval-augmented generation (RAG)
- developer tools
- Git/GitHub integration
- better observability and metrics
- automated backups
- additional provider adapters

These features are intentionally separate from the current deployment milestone.

---

## Credits & Transparency

### Application

**AI-assisted / AI-generated development:**
The application source code was created with AI assistance, including the backend, frontend, database logic, authentication, streaming behavior, and application features.

### Deployment

**Human-created and human-managed:**
The deployment and infrastructure work was created and managed by the project developer, including the Docker configuration, Compose YAML, Oracle Cloud environment, Nginx reverse proxy, DuckDNS domain, HTTPS setup, server configuration, runtime environment, and deployment troubleshooting.

This project is intentionally transparent about where AI was used and where hands-on infrastructure work was performed.

---

## Project Status

**Current milestone: Stage 7 — Deployable Demo**

The application currently supports authenticated chat, persistent conversations, streaming responses, cancellation/Stop behavior, quota controls, Docker deployment, and public deployment behind Nginx.

---

## License

Add the license required by your college or project policy. If this repository is intended to remain private, no public license is required.
