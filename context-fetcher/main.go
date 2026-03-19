package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

var factGeneratorURL = "http://fact-generator:5000"

const serviceName = "context-fetcher"

var tracer = otel.Tracer("context-fetcher/http")
var meter = otel.Meter("context-fetcher/http")
var appLogger = global.Logger("context-fetcher")

var (
	requestCounter          metric.Int64Counter
	requestLatencyHistogram metric.Float64Histogram
	externalCallCounter     metric.Int64Counter
	externalCallLatencyMs   metric.Float64Histogram
)

var docPages = []string{
	"signals.md",
	"context.md",
	"observability-primer.md",
	"components.md",
	"instrumentation.md",
}

type githubCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

type commitPayload struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

type generateRequest struct {
	Commits     []commitPayload `json:"commits"`
	Repo        string          `json:"repo"`
	DocsSnippet string          `json:"docs_snippet"`
}

type factResponse struct {
	Title      string `json:"title,omitempty"`
	Fact       string `json:"fact,omitempty"`
	SourceType string `json:"source_type,omitempty"`
	Error      string `json:"error,omitempty"`
}

func main() {
	if url := os.Getenv("FACT_GENERATOR_URL"); url != "" {
		factGeneratorURL = url
	}

	shutdownOTel, err := setupOpenTelemetry(context.Background())
	if err != nil {
		log.Fatalf("failed to initialize OpenTelemetry: %v", err)
	}
	defer func() {
		if err := shutdownOTel(context.Background()); err != nil {
			log.Printf("failed to shutdown OpenTelemetry cleanly: %v", err)
		}
	}()

	if err := setupMetrics(); err != nil {
		log.Fatalf("failed to initialize metrics instruments: %v", err)
	}

	http.HandleFunc("/fact", handleFact)

	log.Println("Context-fetcher listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleFact(w http.ResponseWriter, r *http.Request) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	method := normalizedMethod(r.Method)
	ctx, span := tracer.Start(
		ctx,
		fmt.Sprintf("%s /fact", method),
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			semconv.HTTPRequestMethodKey.String(method),
			semconv.HTTPRoute("/fact"),
			semconv.URLPath("/fact"),
			semconv.URLScheme("http"),
		),
	)
	defer span.End()

	start := time.Now()
	statusWriter := &responseStatusWriter{ResponseWriter: w, statusCode: http.StatusOK}
	statusWriter.Header().Set("Content-Type", "application/json")
	defer func() {
		statusCode := statusWriter.statusCode
		span.SetAttributes(semconv.HTTPResponseStatusCode(statusCode))
		if statusCode >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(statusCode))
		}

		requestAttrs := []attribute.KeyValue{
			attribute.String("http.request.method", method),
			attribute.String("http.route", "/fact"),
			attribute.Int("http.response.status_code", statusCode),
		}
		requestCounter.Add(ctx, 1, metric.WithAttributes(requestAttrs...))
		requestLatencyHistogram.Record(ctx, float64(time.Since(start).Milliseconds()), metric.WithAttributes(requestAttrs...))
		recordLog(ctx, otellog.SeverityInfo, "request completed", otellog.Int("http.response.status_code", statusCode))
	}()

	repo := "open-telemetry/opentelemetry-collector"

	type commitsResult struct {
		commits []commitPayload
		err     error
	}
	type docsResult struct {
		snippet string
		err     error
	}

	commitsCh := make(chan commitsResult, 1)
	docsCh := make(chan docsResult, 1)

	go func() {
		commits, err := fetchCommits(ctx, repo)
		commitsCh <- commitsResult{commits, err}
	}()

	go func() {
		snippet, err := fetchDocSnippet(ctx)
		docsCh <- docsResult{snippet, err}
	}()

	cr := <-commitsCh
	dr := <-docsCh

	if cr.err != nil {
		log.Printf("Failed to fetch commits: %v", cr.err)
		recordLog(ctx, otellog.SeverityError, "failed to fetch commits", otellog.String("error.type", "github_fetch_failed"))
		span.RecordError(cr.err)
		span.SetStatus(codes.Error, "failed to fetch commits")
		writeError(statusWriter, http.StatusBadGateway, "failed to fetch commits from GitHub")
		return
	}

	if dr.err != nil {
		log.Printf("Failed to fetch doc snippet: %v", dr.err)
		recordLog(ctx, otellog.SeverityWarn, "failed to fetch doc snippet", otellog.String("error.type", "doc_fetch_failed"))
	}

	genReq := generateRequest{
		Commits:     cr.commits,
		Repo:        repo,
		DocsSnippet: dr.snippet,
	}

	body, err := json.Marshal(genReq)
	if err != nil {
		recordLog(ctx, otellog.SeverityError, "failed to marshal request payload", otellog.String("error.type", "marshal_failed"))
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to marshal request")
		writeError(statusWriter, http.StatusInternalServerError, "failed to marshal request")
		return
	}

	resp, err := postJSON(ctx, factGeneratorURL+"/generate", bytes.NewReader(body), "fact-generator")
	if err != nil {
		log.Printf("Failed to call fact-generator: %v", err)
		recordLog(ctx, otellog.SeverityError, "failed to call fact-generator", otellog.String("error.type", "fact_generator_call_failed"))
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to call fact-generator")
		writeError(statusWriter, http.StatusBadGateway, "failed to call fact-generator")
		return
	}
	defer resp.Body.Close()

	statusWriter.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(statusWriter, resp.Body); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to write response body")
		recordLog(ctx, otellog.SeverityError, "failed to write response body", otellog.String("error.type", "response_write_failed"))
	}

}

func fetchCommits(ctx context.Context, repo string) ([]commitPayload, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/commits?per_page=5", repo)
	resp, err := getRequest(ctx, url, "github-api")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var ghCommits []githubCommit
	if err := json.NewDecoder(resp.Body).Decode(&ghCommits); err != nil {
		return nil, err
	}

	commits := make([]commitPayload, len(ghCommits))
	for i, c := range ghCommits {
		commits[i] = commitPayload{
			SHA:     c.SHA[:7],
			Message: c.Commit.Message,
			Author:  c.Commit.Author.Name,
			Date:    c.Commit.Author.Date,
		}
	}
	return commits, nil
}

func fetchDocSnippet(ctx context.Context) (string, error) {
	page := docPages[rand.Intn(len(docPages))]
	url := fmt.Sprintf(
		"https://raw.githubusercontent.com/open-telemetry/opentelemetry.io/main/content/en/docs/concepts/%s",
		page,
	)
	resp, err := getRequest(ctx, url, "opentelemetry-docs")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub raw returned %d for %s", resp.StatusCode, page)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(factResponse{Error: msg})
}

func setupOpenTelemetry(ctx context.Context) (func(context.Context) error, error) {
	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, err
	}

	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	metricExporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	logExporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)
	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	global.SetLoggerProvider(loggerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	tracer = otel.Tracer("context-fetcher/http")
	meter = otel.Meter("context-fetcher/http")
	appLogger = global.Logger("context-fetcher")

	return func(shutdownCtx context.Context) error {
		var shutdownErr error
		if err := loggerProvider.Shutdown(shutdownCtx); err != nil {
			shutdownErr = err
		}
		if err := meterProvider.Shutdown(shutdownCtx); err != nil {
			shutdownErr = err
		}
		if err := tracerProvider.Shutdown(shutdownCtx); err != nil {
			shutdownErr = err
		}
		return shutdownErr
	}, nil
}

func setupMetrics() error {
	var err error
	requestCounter, err = meter.Int64Counter("context_fetcher_http_server_requests_total", metric.WithDescription("Total number of /fact requests"))
	if err != nil {
		return err
	}
	requestLatencyHistogram, err = meter.Float64Histogram("context_fetcher_http_server_request_duration_ms", metric.WithDescription("/fact request duration in milliseconds"))
	if err != nil {
		return err
	}
	externalCallCounter, err = meter.Int64Counter("context_fetcher_external_calls_total", metric.WithDescription("Total number of outbound HTTP calls"))
	if err != nil {
		return err
	}
	externalCallLatencyMs, err = meter.Float64Histogram("context_fetcher_external_call_duration_ms", metric.WithDescription("Outbound HTTP call duration in milliseconds"))
	if err != nil {
		return err
	}
	return nil
}

func getRequest(ctx context.Context, rawURL, dependency string) (*http.Response, error) {
	return doRequest(ctx, http.MethodGet, rawURL, nil, dependency)
}

func postJSON(ctx context.Context, rawURL string, body io.Reader, dependency string) (*http.Response, error) {
	return doRequest(ctx, http.MethodPost, rawURL, body, dependency)
}

func doRequest(ctx context.Context, method, rawURL string, body io.Reader, dependency string) (*http.Response, error) {
	requestURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	serverPort := resolveServerPort(requestURL)
	ctx, span := tracer.Start(
		ctx,
		method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPRequestMethodKey.String(method),
			semconv.URLFull(rawURL),
			semconv.ServerAddress(requestURL.Hostname()),
			semconv.ServerPort(serverPort),
		),
	)
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to build outbound request")
		return nil, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	start := time.Now()
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	durationMs := float64(time.Since(start).Milliseconds())

	metricAttrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("dependency.name", dependency),
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "outbound call failed")
		externalCallCounter.Add(ctx, 1, metric.WithAttributes(append(metricAttrs, attribute.String("error.type", "request_failed"))...))
		externalCallLatencyMs.Record(ctx, durationMs, metric.WithAttributes(metricAttrs...))
		recordLog(ctx, otellog.SeverityError, "outbound HTTP call failed", otellog.String("dependency.name", dependency), otellog.String("error.type", "request_failed"))
		return nil, err
	}

	statusCode := resp.StatusCode
	span.SetAttributes(semconv.HTTPResponseStatusCode(statusCode))
	if statusCode >= http.StatusBadRequest {
		errType := fmt.Sprintf("%d", statusCode)
		span.SetAttributes(attribute.String("error.type", errType))
	}
	if statusCode >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, http.StatusText(statusCode))
	}

	metricAttrs = append(metricAttrs, attribute.Int("http.response.status_code", statusCode))
	externalCallCounter.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
	externalCallLatencyMs.Record(ctx, durationMs, metric.WithAttributes(metricAttrs...))

	return resp, nil
}

func normalizedMethod(method string) string {
	if method == "" {
		return "HTTP"
	}
	known := map[string]struct{}{
		http.MethodConnect: {},
		http.MethodDelete:  {},
		http.MethodGet:     {},
		http.MethodHead:    {},
		http.MethodOptions: {},
		http.MethodPatch:   {},
		http.MethodPost:    {},
		http.MethodPut:     {},
		http.MethodTrace:   {},
		"QUERY":            {},
	}
	upper := strings.ToUpper(method)
	if _, ok := known[upper]; ok {
		return upper
	}
	return "HTTP"
}

func resolveServerPort(requestURL *url.URL) int {
	if requestURL.Port() != "" {
		if p, err := strconv.Atoi(requestURL.Port()); err == nil {
			return p
		}
	}
	if requestURL.Scheme == "https" {
		return 443
	}
	return 80
}

func recordLog(ctx context.Context, severity otellog.Severity, body string, attrs ...otellog.KeyValue) {
	rec := otellog.Record{}
	rec.SetTimestamp(time.Now())
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(severity)
	rec.SetSeverityText(severityText(severity))
	rec.SetBody(otellog.StringValue(body))
	rec.AddAttributes(attrs...)

	spanCtx := trace.SpanContextFromContext(ctx)
	if spanCtx.IsValid() {
		rec.AddAttributes(
			otellog.String("trace_id", spanCtx.TraceID().String()),
			otellog.String("span_id", spanCtx.SpanID().String()),
		)
	}

	appLogger.Emit(ctx, rec)
}

func severityText(level otellog.Severity) string {
	switch level {
	case otellog.SeverityDebug:
		return "DEBUG"
	case otellog.SeverityInfo:
		return "INFO"
	case otellog.SeverityWarn:
		return "WARN"
	case otellog.SeverityError:
		return "ERROR"
	default:
		return "UNSPECIFIED"
	}
}

type responseStatusWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *responseStatusWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}
