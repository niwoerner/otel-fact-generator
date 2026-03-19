const { NodeSDK } = require("@opentelemetry/sdk-node");
const { resourceFromAttributes } = require("@opentelemetry/resources");
const {
  OTLPTraceExporter,
} = require("@opentelemetry/exporter-trace-otlp-grpc");
const {
  OTLPMetricExporter,
} = require("@opentelemetry/exporter-metrics-otlp-grpc");
const { OTLPLogExporter } = require("@opentelemetry/exporter-logs-otlp-grpc");
const { PeriodicExportingMetricReader } = require("@opentelemetry/sdk-metrics");
const { BatchLogRecordProcessor } = require("@opentelemetry/sdk-logs");
const {
  CompositePropagator,
  W3CBaggagePropagator,
  W3CTraceContextPropagator,
} = require("@opentelemetry/core");
const {
  ATTR_SERVICE_NAME,
  ATTR_SERVICE_VERSION,
} = require("@opentelemetry/semantic-conventions");

const telemetryResource = resourceFromAttributes({
  [ATTR_SERVICE_NAME]: process.env.OTEL_SERVICE_NAME || "frontend",
  [ATTR_SERVICE_VERSION]: process.env.npm_package_version || "1.0.0",
});

const sdk = new NodeSDK({
  resource: telemetryResource,
  traceExporter: new OTLPTraceExporter(),
  metricReader: new PeriodicExportingMetricReader({
    exporter: new OTLPMetricExporter(),
  }),
  logRecordProcessors: [new BatchLogRecordProcessor(new OTLPLogExporter())],
  textMapPropagator: new CompositePropagator({
    propagators: [new W3CTraceContextPropagator(), new W3CBaggagePropagator()],
  }),
  instrumentations: [],
});

const startTelemetry = async () => {
  try {
    await Promise.resolve(sdk.start());
    console.log("OpenTelemetry initialized (manual instrumentation only)");
  } catch (err) {
    console.error("OpenTelemetry initialization failed:", err.message);
  }
};

const shutdownTelemetry = async () => {
  try {
    await sdk.shutdown();
  } catch (err) {
    console.error("OpenTelemetry shutdown failed:", err.message);
  }
};

startTelemetry();

module.exports = {
  shutdownTelemetry,
};
