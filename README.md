# otel-fact-generator

Demo app with 3 microservices that generate fun OpenTelemetry facts, now instrumented with OpenTelemetry tracing.

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
# optionally set OTEL_EXPORTER_OTLP_ENDPOINT in .env to export spans to a collector
docker compose up --build
```

Open http://localhost:4000 and click **Generate Fact**.

By default, spans are written to service logs with a console exporter. If `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set, all services export OTLP/HTTP traces instead.

## Services

| Service | Tech | Port | Role |
|---------|------|------|------|
| Frontend | Node.js / Express | 3000 (host: 4000) | Serves UI, proxies `/api/fact` |
| Context Fetcher | Go (stdlib) | 8080 | Fetches GitHub commits + OTel docs |
| Fact Generator | Python / Flask | 5000 | Calls LLM to generate a fact |
