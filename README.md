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

## Local Trace Visualization

`docker compose up --build` now also starts an OpenTelemetry Collector and Jaeger.

- Generate traffic via http://localhost:4000
- Open Jaeger UI: http://localhost:16686
- Select service (`frontend`, `context-fetcher`, or `fact-generator`) and click **Find Traces**

By default in Docker Compose, services export traces to `http://otel-collector:4318`. You can override with `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`.

## Services

| Service | Tech | Port | Role |
|---------|------|------|------|
| Frontend | Node.js / Express | 3000 (host: 4000) | Serves UI, proxies `/api/fact` |
| Context Fetcher | Go (stdlib) | 8080 | Fetches GitHub commits + OTel docs |
| Fact Generator | Python / Flask | 5000 | Calls LLM to generate a fact |
