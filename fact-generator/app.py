import json
import logging
import os
import random
import time
from typing import Any, cast

from flask import Flask, g, jsonify, request
import litellm
from opentelemetry import metrics, propagate, trace
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
from opentelemetry.trace import SpanKind, Status, StatusCode

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


def setup_telemetry():
    resource = Resource.create(
        {
            "service.name": os.environ.get("OTEL_SERVICE_NAME", "otel-fact-generator"),
            "service.version": os.environ.get("SERVICE_VERSION", "0.1.0"),
            "deployment.environment": os.environ.get(
                "DEPLOYMENT_ENVIRONMENT", "development"
            ),
        }
    )

    tracer_provider = TracerProvider(resource=resource)
    tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(tracer_provider)
    tracer = trace.get_tracer("fact_generator")

    metric_reader = PeriodicExportingMetricReader(OTLPMetricExporter())
    meter_provider = MeterProvider(resource=resource, metric_readers=[metric_reader])
    metrics.set_meter_provider(meter_provider)
    meter = metrics.get_meter("fact_generator")

    logger_provider = LoggerProvider(resource=resource)
    logger_provider.add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter()))
    set_logger_provider(logger_provider)

    app_logger = logging.getLogger("fact_generator")
    app_logger.setLevel(logging.INFO)
    if not any(isinstance(handler, LoggingHandler) for handler in app_logger.handlers):
        app_logger.addHandler(
            LoggingHandler(level=logging.INFO, logger_provider=logger_provider)
        )

    return tracer, meter, app_logger


TRACER, METER, OTEL_LOGGER = setup_telemetry()

REQUEST_COUNTER = METER.create_counter(
    "fact_generator.requests",
    description="Total incoming HTTP requests",
    unit="1",
)

REQUEST_DURATION = METER.create_histogram(
    "fact_generator.request.duration",
    description="HTTP request duration",
    unit="ms",
)

LLM_COUNTER = METER.create_counter(
    "fact_generator.llm.calls",
    description="Total LLM calls",
    unit="1",
)

LLM_DURATION = METER.create_histogram(
    "fact_generator.llm.duration",
    description="LLM completion duration",
    unit="ms",
)


def finish_request_span(response_status_code=None, error=None):
    span = getattr(g, "request_span", None)
    scope = getattr(g, "request_scope", None)
    if span is None or scope is None or getattr(g, "request_span_closed", False):
        return

    method = request.method
    route = getattr(g, "http_route", request.path)
    status_code = response_status_code if response_status_code is not None else 500
    duration_ms = (time.perf_counter() - g.request_started_at) * 1000

    span.set_attribute("http.response.status_code", status_code)
    if error is not None:
        span.record_exception(error)
        span.set_status(Status(StatusCode.ERROR, str(error)))
    elif status_code >= 500:
        span.set_status(Status(StatusCode.ERROR))

    attributes = {
        "http.request.method": method,
        "http.route": route,
        "http.response.status_code": status_code,
    }
    REQUEST_COUNTER.add(1, attributes=attributes)
    REQUEST_DURATION.record(duration_ms, attributes=attributes)

    scope.__exit__(None, None, None)
    span.end()
    g.request_span_closed = True


@app.before_request
def start_request_span():
    route = request.url_rule.rule if request.url_rule is not None else request.path
    attributes = {
        "http.request.method": request.method,
        "http.route": route,
        "url.path": request.path,
        "url.scheme": request.scheme,
    }
    if request.query_string:
        attributes["url.query"] = request.query_string.decode("utf-8", "replace")

    carrier = dict(request.headers)
    context = propagate.extract(carrier)
    span_name = f"{request.method} {route}"
    span = TRACER.start_span(
        name=span_name, context=context, kind=SpanKind.SERVER, attributes=attributes
    )
    scope = trace.use_span(span, end_on_exit=False)
    scope.__enter__()

    g.request_span = span
    g.request_scope = scope
    g.request_span_closed = False
    g.request_started_at = time.perf_counter()
    g.http_route = route


@app.after_request
def close_request_span(response):
    finish_request_span(response_status_code=response.status_code)
    return response


@app.teardown_request
def close_request_span_on_error(error):
    finish_request_span(error=error)


@app.route("/generate", methods=["POST"])
def generate():
    OTEL_LOGGER.info("generate request received")
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

    try:
        llm_start = time.perf_counter()
        with TRACER.start_as_current_span(
            "llm.completion", kind=SpanKind.CLIENT
        ) as llm_span:
            llm_span.set_attribute("gen_ai.request.model", MODEL)
            response = cast(
                Any,
                litellm.completion(
                    model=MODEL,
                    messages=[{"role": "user", "content": prompt}],
                    max_tokens=200,
                    temperature=0.8,
                    response_format=response_format,
                ),
            )

        llm_duration_ms = (time.perf_counter() - llm_start) * 1000
        llm_attributes = {
            "gen_ai.request.model": MODEL,
            "gen_ai.request.outcome": "success",
        }
        LLM_COUNTER.add(1, attributes=llm_attributes)
        LLM_DURATION.record(llm_duration_ms, attributes=llm_attributes)

        response_content = response.choices[0].message.content or "{}"
        result = json.loads(response_content)
        return jsonify(
            {
                "title": result["title"],
                "fact": result["fact"],
                "source_type": result["source_type"],
            }
        )
    except Exception as e:
        llm_attributes = {
            "gen_ai.request.model": MODEL,
            "gen_ai.request.outcome": "error",
        }
        LLM_COUNTER.add(1, attributes=llm_attributes)
        OTEL_LOGGER.exception("LLM call failed")
        app.logger.warning("LLM call failed (%s), returning fallback fact", e)
        return jsonify({"fact": random.choice(FALLBACK_FACTS)})


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=5000)
