const {
  context,
  metrics,
  propagation,
  trace,
  SpanKind,
  SpanStatusCode,
} = require("@opentelemetry/api");
const { logs, SeverityNumber } = require("@opentelemetry/api-logs");
const express = require("express");
const http = require("http");
const path = require("path");
const { shutdownTelemetry } = require("./telemetry");

const app = express();
const CONTEXT_FETCHER_URL = process.env.CONTEXT_FETCHER_URL || "http://context-fetcher:8080";
const tracer = trace.getTracer("frontend.server", "1.0.0");
const meter = metrics.getMeter("frontend.server", "1.0.0");
const appLogger = logs.getLogger("frontend.server", "1.0.0");

const requestCounter = meter.createCounter("frontend.http.server.requests", {
  description: "Count of handled /api/fact requests",
});
const errorCounter = meter.createCounter("frontend.http.server.errors", {
  description: "Count of failed /api/fact requests",
});
const requestDuration = meter.createHistogram("frontend.http.server.request.duration", {
  description: "Duration of /api/fact requests",
  unit: "ms",
});

app.use(express.static(path.join(__dirname, "public")));

app.get("/api/fact", (req, res) => {
  const startedAt = process.hrtime.bigint();
  const parentContext = propagation.extract(context.active(), req.headers);
  const route = "/api/fact";
  const baseAttributes = {
    "http.request.method": req.method,
    "http.route": route,
  };

  requestCounter.add(1, baseAttributes);

  let requestCompleted = false;

  const completeRequest = (serverSpan, err) => {
    if (requestCompleted) {
      return;
    }
    requestCompleted = true;

    const statusCode = res.statusCode || 500;
    const durationMs = Number(process.hrtime.bigint() - startedAt) / 1e6;
    const outcomeAttrs = {
      ...baseAttributes,
      "http.response.status_code": statusCode,
    };

    requestDuration.record(durationMs, outcomeAttrs);
    serverSpan.setAttribute("http.response.status_code", statusCode);

    if (err || statusCode >= 500) {
      if (err) {
        serverSpan.recordException(err);
      }
      serverSpan.setStatus({ code: SpanStatusCode.ERROR });
      errorCounter.add(1, outcomeAttrs);
      appLogger.emit({
        severityNumber: SeverityNumber.ERROR,
        severityText: "ERROR",
        body: "request failed",
        attributes: {
          ...outcomeAttrs,
          "error.message": err ? err.message : "upstream error",
        },
        context: trace.setSpan(parentContext, serverSpan),
      });
    } else {
      appLogger.emit({
        severityNumber: SeverityNumber.INFO,
        severityText: "INFO",
        body: "request completed",
        attributes: outcomeAttrs,
        context: trace.setSpan(parentContext, serverSpan),
      });
    }

    serverSpan.end();
  };

  tracer.startActiveSpan(
    `${req.method} ${route}`,
    {
      kind: SpanKind.SERVER,
      attributes: {
        ...baseAttributes,
        "url.path": req.path,
        "url.scheme": req.protocol,
      },
    },
    parentContext,
    (serverSpan) => {
      const url = `${CONTEXT_FETCHER_URL}/fact`;
      const upstreamUrl = new URL(url);
      const outboundHeaders = {};

      propagation.inject(context.active(), outboundHeaders);

      tracer.startActiveSpan(
        "GET /fact",
        {
          kind: SpanKind.CLIENT,
          attributes: {
            "http.request.method": "GET",
            "server.address": upstreamUrl.hostname,
            "server.port": upstreamUrl.port
              ? Number(upstreamUrl.port)
              : upstreamUrl.protocol === "https:"
                ? 443
                : 80,
            "url.path": upstreamUrl.pathname,
            "url.scheme": upstreamUrl.protocol.replace(":", ""),
          },
        },
        (clientSpan) => {
          let clientSpanCompleted = false;
          const completeClientSpan = (err) => {
            if (clientSpanCompleted) {
              return;
            }
            clientSpanCompleted = true;

            if (err) {
              clientSpan.recordException(err);
              clientSpan.setStatus({ code: SpanStatusCode.ERROR });
            }
            clientSpan.end();
          };

          const requestOptions = {
            protocol: upstreamUrl.protocol,
            hostname: upstreamUrl.hostname,
            port: upstreamUrl.port || undefined,
            path: `${upstreamUrl.pathname}${upstreamUrl.search}`,
            method: "GET",
            headers: outboundHeaders,
          };

          http
            .get(requestOptions, (upstream) => {
              clientSpan.setAttribute("http.response.status_code", upstream.statusCode || 0);

              if ((upstream.statusCode || 500) >= 500) {
                clientSpan.setStatus({ code: SpanStatusCode.ERROR });
              }

              upstream.on("end", () => {
                completeClientSpan();
              });
              upstream.on("error", (err) => {
                completeClientSpan(err);
              });

              res.writeHead(upstream.statusCode, {
                "Content-Type": "application/json",
              });
              upstream.pipe(res);
            })
            .on("error", (err) => {
              completeClientSpan(err);

              console.error("Proxy error:", err.message);
              res.status(502).json({ error: "Failed to reach context-fetcher" });
            });
        }
      );

      res.on("finish", () => {
        completeRequest(serverSpan);
      });
    }
  );
});

const PORT = 3000;
const server = app.listen(PORT, () => {
  console.log(`Frontend listening on :${PORT}`);
});

const stopServer = async () => {
  server.close(async () => {
    await shutdownTelemetry();
    process.exit(0);
  });
};

process.on("SIGTERM", stopServer);
process.on("SIGINT", stopServer);
