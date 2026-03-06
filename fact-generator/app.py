import json
import logging
import os
import random
import time

from flask import Flask, request, jsonify
import litellm
from opentelemetry import metrics, trace
from opentelemetry._logs import set_logger_provider
from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace import Status, StatusCode

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


def init_otel():
    service_name = os.environ.get("OTEL_SERVICE_NAME", "otel-fact-generator")
    base_endpoint = os.environ.get(
        "OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318"
    ).rstrip("/")
    trace_endpoint = os.environ.get(
        "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", f"{base_endpoint}/v1/traces"
    )
    metric_endpoint = os.environ.get(
        "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", f"{base_endpoint}/v1/metrics"
    )
    log_endpoint = os.environ.get(
        "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", f"{base_endpoint}/v1/logs"
    )

    resource = Resource.create({"service.name": service_name})

    tracer_provider = TracerProvider(resource=resource)
    tracer_provider.add_span_processor(
        BatchSpanProcessor(OTLPSpanExporter(endpoint=trace_endpoint))
    )
    trace.set_tracer_provider(tracer_provider)

    metric_reader = PeriodicExportingMetricReader(
        OTLPMetricExporter(endpoint=metric_endpoint)
    )
    meter_provider = MeterProvider(resource=resource, metric_readers=[metric_reader])
    metrics.set_meter_provider(meter_provider)

    logger_provider = LoggerProvider(resource=resource)
    logger_provider.add_log_record_processor(
        BatchLogRecordProcessor(OTLPLogExporter(endpoint=log_endpoint))
    )
    set_logger_provider(logger_provider)

    otel_handler = LoggingHandler(level=logging.INFO, logger_provider=logger_provider)
    otel_logger = logging.getLogger("otel.fact_generator")
    otel_logger.setLevel(logging.INFO)
    if not any(isinstance(handler, LoggingHandler) for handler in otel_logger.handlers):
        otel_logger.addHandler(otel_handler)
    otel_logger.propagate = False

    tracer = trace.get_tracer("otel.fact_generator")
    meter = metrics.get_meter("otel.fact_generator")
    return tracer, meter, otel_logger


TRACER, METER, OTEL_LOGGER = init_otel()
REQUEST_COUNTER = METER.create_counter(
    name="fact_generator.requests.total",
    unit="1",
    description="Total /generate requests",
)
REQUEST_DURATION = METER.create_histogram(
    name="fact_generator.request.duration",
    unit="ms",
    description="/generate request duration",
)


@app.route("/generate", methods=["POST"])
def generate():
    start = time.perf_counter()
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
                    "title": {
                        "type": "string",
                        "description": "Short catchy headline (3-6 words)",
                    },
                    "fact": {
                        "type": "string",
                        "description": "The fun fact, 1-2 sentences",
                    },
                    "source_type": {
                        "type": "string",
                        "enum": ["commit", "documentation"],
                    },
                },
                "required": ["title", "fact", "source_type"],
            },
        },
    }

    request_attributes = {
        "http.method": "POST",
        "http.route": "/generate",
        "repo.name": repo,
        "repo.commit_count": len(commits),
    }

    with TRACER.start_as_current_span(
        "generate_fact", attributes=request_attributes
    ) as span:
        try:
            with TRACER.start_as_current_span(
                "llm.completion", attributes={"llm.model": MODEL}
            ):
                response = litellm.completion(
                    model=MODEL,
                    messages=[{"role": "user", "content": prompt}],
                    max_tokens=200,
                    temperature=0.8,
                    response_format=response_format,
                )

            result = json.loads(response.choices[0].message.content)
            span.set_attribute("fact.source_type", result["source_type"])
            OTEL_LOGGER.info(
                "fact generated",
                extra={
                    "repo.name": repo,
                    "repo.commit_count": len(commits),
                    "fact.source_type": result["source_type"],
                },
            )
            REQUEST_COUNTER.add(1, {"outcome": "success"})
            return jsonify(
                {
                    "title": result["title"],
                    "fact": result["fact"],
                    "source_type": result["source_type"],
                }
            )
        except Exception as e:
            span.record_exception(e)
            span.set_status(Status(StatusCode.ERROR, str(e)))
            OTEL_LOGGER.exception(
                "fact generation failed",
                extra={
                    "repo.name": repo,
                    "repo.commit_count": len(commits),
                },
            )
            REQUEST_COUNTER.add(1, {"outcome": "fallback"})
            app.logger.warning("LLM call failed (%s), returning fallback fact", e)
            return jsonify({"fact": random.choice(FALLBACK_FACTS)})
        finally:
            REQUEST_DURATION.record(
                (time.perf_counter() - start) * 1000,
                {"http.method": "POST", "http.route": "/generate"},
            )


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=5000)
