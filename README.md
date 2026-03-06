# otel-fact-generator

Demo app with 3 uninstrumented microservices that generate fun OpenTelemetry facts. Designed for adding OTel instrumentation as a follow-up exercise.

This setup now includes Grafana LGTM so all telemetry lands in one place.

## Architecture

```
                         ┌─────────────────────────────────────────────┐
                         │          Context Fetcher (Go :8080)         │
                         │                                             │
┌──────────┐   GET       │  ┌──────────────────────────────────────┐   │
│          │  /api/fact  │  │  GET github.com/.../commits          │   │
│ Browser  │────────────▶│  │  (recent OTel Collector commits)     │   │
│          │             │  └──────────────────────────────────────┘   │
└──────────┘             │                                             │
     ▲       Frontend    │  ┌──────────────────────────────────────┐   │
     │      (Node :3000) │  │  GET raw.githubusercontent.com/...   │   │
     │                   │  │  (random OTel docs concept page)     │   │
     │                   │  └──────────────────────────────────────┘   │
     │                   │                                             │
     │                   │         │ POST /generate                    │
     │                   │         │ {commits, repo, docs_snippet}     │
     │                   └─────────┼──────────────────────────────────-┘
     │                             │
     │                             ▼
     │                   ┌─────────────────────────────────────────────┐
     │                   │       Fact Generator (Python :5000)         │
     │      {"fact":...} │                                             │
     └───────────────────│  prompt ──▶ LLM (via LiteLLM)               │
                         │             or fallback hardcoded fact      │
                         └─────────────────────────────────────────────┘
```

## Quick Start

```bash
cp .env.example .env
# optionally set OPENAI_API_KEY in .env for real LLM facts
docker compose up --build
```

Open http://localhost:4000 and click **Generate Fact**.

Open Grafana at http://localhost:3000 (user: `admin`, password: `admin`) to inspect traces, metrics, and logs.

## Telemetry Destination

All three apps export OTLP over HTTP to `http://lgtm:4318` inside Docker Compose.

## Services

| Service | Tech | Port | Role |
|---------|------|------|------|
| Frontend | Node.js / Express | 3000 (host: 4000) | Serves UI, proxies `/api/fact` |
| Context Fetcher | Go (stdlib) | 8080 | Fetches GitHub commits + OTel docs |
| Fact Generator | Python / Flask | 5000 | Calls LLM to generate a fact |
