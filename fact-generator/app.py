import json
import os
import random

from flask import Flask, request, jsonify
import litellm

app = Flask(__name__)

FALLBACK_FACTS = [
    "OpenTelemetry is the second most active CNCF project after Kubernetes.",
    "OpenTelemetry was formed by merging OpenTracing and OpenCensus in 2019.",
    "OTel supports three signal types: traces, metrics, and logs.",
    "The OTel Collector can receive, process, and export telemetry data in multiple formats.",
    "W3C Trace Context is the default propagation format in OpenTelemetry.",
    "OpenTelemetry provides auto-instrumentation for 11+ programming languages.",
    "A span represents a single operation within a trace and can have parent-child relationships.",
    "OTLP (OpenTelemetry Protocol) is the native protocol for transmitting telemetry data.",
    "Baggage in OpenTelemetry lets you propagate user-defined key-value pairs across service boundaries.",
    "OpenTelemetry semantic conventions standardize attribute names like http.request.method and db.system.",
]

MODEL = os.environ.get("LLM_MODEL", "gpt-4o-mini")


@app.route("/generate", methods=["POST"])
def generate():
    data = request.get_json(force=True)
    commits = data.get("commits", [])
    repo = data.get("repo", "unknown")
    docs_snippet = data.get("docs_snippet", "")

    commit_summary = "\n".join(
        f"- {c['message']} (by {c['author']}, {c['date'][:10]})" for c in commits[:5]
    )

    prompt = (
        "You are an OpenTelemetry expert. Generate one fun, surprising, and educational fact "
        "about OpenTelemetry. Base it on either the recent commit activity OR the documentation "
        "snippet below.\n\n"
        "Respond in JSON with these fields:\n"
        '- "title": short catchy headline (3-6 words)\n'
        '- "fact": the fun fact, 1-2 sentences\n'
        '- "source_type": either "commit" or "documentation"\n\n'
        f"Repository: {repo}\n\n"
        f"Recent commits:\n{commit_summary}\n\n"
        f"Documentation snippet:\n{docs_snippet[:2000]}\n"
    )

    response_format = {
        "type": "json_schema",
        "json_schema": {
            "name": "otel_fact",
            "schema": {
                "type": "object",
                "properties": {
                    "title": {"type": "string", "description": "Short catchy headline (3-6 words)"},
                    "fact": {"type": "string", "description": "The fun fact, 1-2 sentences"},
                    "source_type": {"type": "string", "enum": ["commit", "documentation"]},
                },
                "required": ["title", "fact", "source_type"],
            },
        },
    }

    try:
        response = litellm.completion(
            model=MODEL,
            messages=[{"role": "user", "content": prompt}],
            max_tokens=200,
            temperature=0.8,
            response_format=response_format,
        )
        result = json.loads(response.choices[0].message.content)
        return jsonify({
            "title": result["title"],
            "fact": result["fact"],
            "source_type": result["source_type"],
        })
    except Exception as e:
        app.logger.warning("LLM call failed (%s), returning fallback fact", e)
        return jsonify({"fact": random.choice(FALLBACK_FACTS)})


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=5000)
