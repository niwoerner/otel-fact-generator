const { context, metrics, propagation, trace } = require("@opentelemetry/api");
const { logs, SeverityNumber } = require("@opentelemetry/api-logs");
const { OTLPLogExporter } = require("@opentelemetry/exporter-logs-otlp-http");
const { OTLPMetricExporter } = require("@opentelemetry/exporter-metrics-otlp-http");
const { OTLPTraceExporter } = require("@opentelemetry/exporter-trace-otlp-http");
const { Resource } = require("@opentelemetry/resources");
const { LoggerProvider, BatchLogRecordProcessor } = require("@opentelemetry/sdk-logs");
const { MeterProvider, PeriodicExportingMetricReader } = require("@opentelemetry/sdk-metrics");
const { NodeTracerProvider } = require("@opentelemetry/sdk-trace-node");
const { BatchSpanProcessor } = require("@opentelemetry/sdk-trace-base");
const { SemanticResourceAttributes } = require("@opentelemetry/semantic-conventions");

const serviceName = process.env.OTEL_SERVICE_NAME || "otel-fact-frontend";
const serviceVersion = process.env.npm_package_version || "1.0.0";
const protocolEndpoint = process.env.OTEL_EXPORTER_OTLP_ENDPOINT || "http://otel-collector:4318";
const traceEndpoint = process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT || `${protocolEndpoint}/v1/traces`;
const metricsEndpoint = process.env.OTEL_EXPORTER_OTLP_METRICS_ENDPOINT || `${protocolEndpoint}/v1/metrics`;
const logsEndpoint = process.env.OTEL_EXPORTER_OTLP_LOGS_ENDPOINT || `${protocolEndpoint}/v1/logs`;

const resource = new Resource({
  [SemanticResourceAttributes.SERVICE_NAME]: serviceName,
  [SemanticResourceAttributes.SERVICE_VERSION]: serviceVersion,
});

const tracerProvider = new NodeTracerProvider({ resource });
tracerProvider.addSpanProcessor(new BatchSpanProcessor(new OTLPTraceExporter({ url: traceEndpoint })));
tracerProvider.register();

const meterReader = new PeriodicExportingMetricReader({
  exporter: new OTLPMetricExporter({ url: metricsEndpoint }),
  exportIntervalMillis: Number(process.env.OTEL_METRIC_EXPORT_INTERVAL || 10000),
});
const meterProvider = new MeterProvider({ resource, readers: [meterReader] });
metrics.setGlobalMeterProvider(meterProvider);

const logRecordProcessor = new BatchLogRecordProcessor(new OTLPLogExporter({ url: logsEndpoint }));
const loggerProvider = new LoggerProvider({ resource, processors: [logRecordProcessor] });
logs.setGlobalLoggerProvider(loggerProvider);

const tracer = trace.getTracer("frontend-server");
const meter = metrics.getMeter("frontend-server");
const logger = logs.getLogger("frontend-server");

const httpServerRequests = meter.createCounter("http.server.requests", {
  description: "Total inbound HTTP requests",
});
const httpServerErrors = meter.createCounter("http.server.errors", {
  description: "Total inbound HTTP 5xx responses",
});
const httpServerDuration = meter.createHistogram("http.server.duration", {
  description: "Inbound HTTP request duration",
  unit: "ms",
});
const httpServerInFlight = meter.createUpDownCounter("http.server.in_flight", {
  description: "In-flight inbound HTTP requests",
});

const upstreamCalls = meter.createCounter("upstream.context_fetcher.calls", {
  description: "Total calls to context-fetcher",
});
const upstreamErrors = meter.createCounter("upstream.context_fetcher.errors", {
  description: "Total failed calls to context-fetcher",
});
const upstreamDuration = meter.createHistogram("upstream.context_fetcher.duration", {
  description: "Duration of calls to context-fetcher",
  unit: "ms",
});

async function shutdownTelemetry() {
  await Promise.allSettled([
    tracerProvider.shutdown(),
    meterProvider.shutdown(),
    loggerProvider.shutdown(),
  ]);
}

module.exports = {
  context,
  logger,
  propagation,
  SeverityNumber,
  trace,
  tracer,
  httpServerRequests,
  httpServerErrors,
  httpServerDuration,
  httpServerInFlight,
  upstreamCalls,
  upstreamErrors,
  upstreamDuration,
  shutdownTelemetry,
};
