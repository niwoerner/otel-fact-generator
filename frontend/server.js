const express = require("express");
const http = require("http");
const path = require("path");
const { SpanStatusCode } = require("@opentelemetry/api");
const {
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
} = require("./telemetry");

const app = express();
const CONTEXT_FETCHER_URL = process.env.CONTEXT_FETCHER_URL || "http://context-fetcher:8080";

app.use(express.static(path.join(__dirname, "public")));

app.use((req, res, next) => {
  const start = process.hrtime.bigint();
  const parentContext = propagation.extract(context.active(), req.headers);
  const span = tracer.startSpan(
    `${req.method} ${req.path}`,
    {
      attributes: {
        "http.method": req.method,
        "http.route": req.path,
        "http.target": req.originalUrl,
      },
    },
    parentContext,
  );
  const requestContext = trace.setSpan(parentContext, span);
  const metricAttributes = { method: req.method, route: req.path };

  httpServerInFlight.add(1, metricAttributes);
  httpServerRequests.add(1, metricAttributes);

  res.on("finish", () => {
    const durationMs = Number(process.hrtime.bigint() - start) / 1e6;
    const statusCode = res.statusCode;

    span.setAttribute("http.status_code", statusCode);
    httpServerDuration.record(durationMs, { ...metricAttributes, status_code: statusCode });

    if (statusCode >= 500) {
      span.setStatus({ code: SpanStatusCode.ERROR });
      httpServerErrors.add(1, metricAttributes);
    } else {
      span.setStatus({ code: SpanStatusCode.OK });
    }

    logger.emit({
      severityNumber: SeverityNumber.INFO,
      severityText: "INFO",
      body: "request completed",
      attributes: {
        path: req.path,
        method: req.method,
        status_code: statusCode,
        duration_ms: durationMs,
      },
    });

    httpServerInFlight.add(-1, metricAttributes);
    span.end();
  });

  context.with(requestContext, next);
});

app.get("/api/fact", (req, res) => {
  const url = `${CONTEXT_FETCHER_URL}/fact`;
  const start = process.hrtime.bigint();
  const currentContext = context.active();
  const span = tracer.startSpan(
    "context-fetcher.request",
    {
      attributes: {
        "http.method": "GET",
        "http.url": url,
      },
    },
    currentContext,
  );
  const upstreamContext = trace.setSpan(currentContext, span);
  const propagatedHeaders = {};

  propagation.inject(upstreamContext, propagatedHeaders);
  upstreamCalls.add(1);

  context.with(upstreamContext, () => {
    http
      .get(url, { headers: propagatedHeaders }, (upstream) => {
        span.setAttribute("http.status_code", upstream.statusCode || 0);
        res.writeHead(upstream.statusCode, {
          "Content-Type": "application/json",
        });

        upstream.on("end", () => {
          const durationMs = Number(process.hrtime.bigint() - start) / 1e6;
          upstreamDuration.record(durationMs);

          if ((upstream.statusCode || 0) >= 500) {
            span.setStatus({ code: SpanStatusCode.ERROR });
            upstreamErrors.add(1);
          } else {
            span.setStatus({ code: SpanStatusCode.OK });
          }

          span.end();
        });

        upstream.on("error", (err) => {
          const durationMs = Number(process.hrtime.bigint() - start) / 1e6;
          upstreamDuration.record(durationMs);
          upstreamErrors.add(1);

          span.recordException(err);
          span.setStatus({ code: SpanStatusCode.ERROR, message: err.message });

          logger.emit({
            severityNumber: SeverityNumber.ERROR,
            severityText: "ERROR",
            body: "upstream stream error",
            attributes: {
              error: err.message,
              upstream_url: url,
            },
          });

          span.end();
        });

        upstream.pipe(res);
      })
      .on("error", (err) => {
        const durationMs = Number(process.hrtime.bigint() - start) / 1e6;
        upstreamDuration.record(durationMs);
        upstreamErrors.add(1);

        span.recordException(err);
        span.setStatus({ code: SpanStatusCode.ERROR, message: err.message });

        logger.emit({
          severityNumber: SeverityNumber.ERROR,
          severityText: "ERROR",
          body: "proxy request failed",
          attributes: {
            error: err.message,
            upstream_url: url,
          },
        });

        console.error("Proxy error:", err.message);
        res.status(502).json({ error: "Failed to reach context-fetcher" });
        span.end();
      });
  });
});

const PORT = 3000;
const server = app.listen(PORT, () => {
  console.log(`Frontend listening on :${PORT}`);
});

async function stop() {
  await new Promise((resolve) => {
    server.close(resolve);
  });
  await shutdownTelemetry();
  process.exit(0);
}

process.on("SIGINT", () => {
  void stop();
});
process.on("SIGTERM", () => {
  void stop();
});
