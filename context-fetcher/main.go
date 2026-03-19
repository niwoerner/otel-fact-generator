package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var factGeneratorURL = "http://fact-generator:5000"

var tracer trace.Tracer
var meter metric.Meter
var otelLogger otellog.Logger

var requestCounter metric.Int64Counter
var requestDuration metric.Float64Histogram
var dependencyCallCounter metric.Int64Counter
var errorCounter metric.Int64Counter

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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := setupTelemetry(ctx)
	if err != nil {
		log.Fatalf("failed to initialize OpenTelemetry: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			log.Printf("failed to shutdown OpenTelemetry cleanly: %v", err)
		}
	}()

	if url := os.Getenv("FACT_GENERATOR_URL"); url != "" {
		factGeneratorURL = url
	}

	http.HandleFunc("/fact", handleFact)
	srv := &http.Server{Addr: ":8080"}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Println("Context-fetcher listening on :8080")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func handleFact(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(
		r.Context(),
		"handleFact",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.route", "/fact"),
		),
	)
	defer span.End()

	start := time.Now()
	requestCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("http.route", "/fact")))
	defer requestDuration.Record(ctx, float64(time.Since(start).Milliseconds()), metric.WithAttributes(attribute.String("http.route", "/fact")))
	emitLog(ctx, otellog.SeverityInfo, "handling fact request", otellog.String("http.route", "/fact"))

	w.Header().Set("Content-Type", "application/json")

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
		span.RecordError(cr.err)
		span.SetStatus(codes.Error, "failed to fetch commits")
		errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "fetch_commits")))
		emitLog(ctx, otellog.SeverityError, "failed to fetch commits", otellog.String("error", cr.err.Error()))
		log.Printf("Failed to fetch commits: %v", cr.err)
		writeError(w, http.StatusBadGateway, "failed to fetch commits from GitHub")
		return
	}

	if dr.err != nil {
		span.RecordError(dr.err)
		errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "fetch_doc_snippet")))
		emitLog(ctx, otellog.SeverityWarn, "failed to fetch doc snippet", otellog.String("error", dr.err.Error()))
		log.Printf("Failed to fetch doc snippet: %v", dr.err)
	}

	genReq := generateRequest{
		Commits:     cr.commits,
		Repo:        repo,
		DocsSnippet: dr.snippet,
	}

	body, err := json.Marshal(genReq)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to marshal generate request")
		errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "marshal_generate_request")))
		emitLog(ctx, otellog.SeverityError, "failed to marshal generate request", otellog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "failed to marshal request")
		return
	}

	client := &http.Client{Timeout: 30 * time.Second}
	callCtx, callSpan := tracer.Start(
		ctx,
		"fact-generator.generate",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.url", factGeneratorURL+"/generate")),
	)
	dependencyCallCounter.Add(callCtx, 1, metric.WithAttributes(attribute.String("dependency", "fact-generator"), attribute.String("operation", "generate")))

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, factGeneratorURL+"/generate", bytes.NewReader(body))
	if err != nil {
		callSpan.RecordError(err)
		callSpan.SetStatus(codes.Error, "failed to build fact-generator request")
		callSpan.End()
		span.RecordError(err)
		errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "build_fact_generator_request")))
		emitLog(ctx, otellog.SeverityError, "failed to build fact-generator request", otellog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "failed to prepare fact-generator request")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		callSpan.RecordError(err)
		callSpan.SetStatus(codes.Error, "fact-generator request failed")
		callSpan.End()
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to call fact-generator")
		errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "call_fact_generator")))
		emitLog(ctx, otellog.SeverityError, "failed to call fact-generator", otellog.String("error", err.Error()))
		log.Printf("Failed to call fact-generator: %v", err)
		writeError(w, http.StatusBadGateway, "failed to call fact-generator")
		return
	}
	defer resp.Body.Close()

	callSpan.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	if resp.StatusCode >= http.StatusBadRequest {
		callSpan.SetStatus(codes.Error, "fact-generator returned non-success status")
		errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "fact_generator_non_success")))
		emitLog(ctx, otellog.SeverityWarn, "fact-generator returned non-success status", otellog.Int("http.status_code", resp.StatusCode))
	}
	callSpan.End()

	if _, err := io.Copy(w, resp.Body); err != nil {
		span.RecordError(err)
		errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "write_response")))
		emitLog(ctx, otellog.SeverityError, "failed to write response body", otellog.String("error", err.Error()))
	}
}

func fetchCommits(ctx context.Context, repo string) ([]commitPayload, error) {
	ctx, span := tracer.Start(
		ctx,
		"fetchCommits",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("github.repo", repo),
			attribute.String("http.method", http.MethodGet),
		),
	)
	defer span.End()

	url := fmt.Sprintf("https://api.github.com/repos/%s/commits?per_page=5", repo)
	dependencyCallCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("dependency", "github"), attribute.String("operation", "commits")))

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to build GitHub request")
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "GitHub request failed")
		return nil, err
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("GitHub API returned %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(codes.Error, "GitHub API non-success status")
		return nil, err
	}

	var ghCommits []githubCommit
	if err := json.NewDecoder(resp.Body).Decode(&ghCommits); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to decode GitHub response")
		return nil, err
	}

	commits := make([]commitPayload, len(ghCommits))
	for i, c := range ghCommits {
		shortSHA := c.SHA
		if len(shortSHA) > 7 {
			shortSHA = shortSHA[:7]
		}

		commits[i] = commitPayload{
			SHA:     shortSHA,
			Message: c.Commit.Message,
			Author:  c.Commit.Author.Name,
			Date:    c.Commit.Author.Date,
		}
	}
	span.SetAttributes(attribute.Int("github.commits.count", len(commits)))
	emitLog(ctx, otellog.SeverityInfo, "fetched commits", otellog.Int("commit_count", len(commits)), otellog.String("repo", repo))
	return commits, nil
}

func fetchDocSnippet(ctx context.Context) (string, error) {
	page := docPages[rand.Intn(len(docPages))]
	ctx, span := tracer.Start(
		ctx,
		"fetchDocSnippet",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("docs.page", page),
			attribute.String("http.method", http.MethodGet),
		),
	)
	defer span.End()

	url := fmt.Sprintf(
		"https://raw.githubusercontent.com/open-telemetry/opentelemetry.io/main/content/en/docs/concepts/%s",
		page,
	)
	dependencyCallCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("dependency", "opentelemetry-docs"), attribute.String("operation", "fetch_snippet")))

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to build docs request")
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "docs request failed")
		return "", err
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("GitHub raw returned %d for %s", resp.StatusCode, page)
		span.RecordError(err)
		span.SetStatus(codes.Error, "docs source non-success status")
		return "", err
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to read docs response")
		return "", err
	}
	span.SetAttributes(attribute.Int("docs.snippet.bytes", len(data)))
	return string(data), nil
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(factResponse{Error: msg})
}

func setupTelemetry(ctx context.Context) (func(context.Context) error, error) {
	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName("context-fetcher"),
			semconv.ServiceVersion("1.0.0"),
		),
	)
	if err != nil {
		return nil, err
	}

	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(traceProvider)

	metricExporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	metricProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(metricProvider)

	logExporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}
	logProvider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)
	logglobal.SetLoggerProvider(logProvider)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	tracer = otel.Tracer("context-fetcher")
	meter = otel.Meter("context-fetcher")
	otelLogger = logglobal.GetLoggerProvider().Logger("context-fetcher")

	requestCounter, err = meter.Int64Counter("http.server.requests", metric.WithDescription("Total number of /fact requests"))
	if err != nil {
		return nil, err
	}
	requestDuration, err = meter.Float64Histogram("http.server.request.duration", metric.WithUnit("ms"), metric.WithDescription("Duration of /fact requests in milliseconds"))
	if err != nil {
		return nil, err
	}
	dependencyCallCounter, err = meter.Int64Counter("dependency.calls", metric.WithDescription("Outbound dependency calls"))
	if err != nil {
		return nil, err
	}
	errorCounter, err = meter.Int64Counter("app.errors", metric.WithDescription("Application errors by stage"))
	if err != nil {
		return nil, err
	}

	shutdown := func(ctx context.Context) error {
		return errors.Join(
			logProvider.Shutdown(ctx),
			metricProvider.Shutdown(ctx),
			traceProvider.Shutdown(ctx),
		)
	}

	return shutdown, nil
}

func emitLog(ctx context.Context, severity otellog.Severity, message string, attrs ...otellog.KeyValue) {
	if !otelLogger.Enabled(ctx, otellog.EnabledParameters{Severity: severity, EventName: "application"}) {
		return
	}

	var record otellog.Record
	now := time.Now()
	record.SetTimestamp(now)
	record.SetObservedTimestamp(now)
	record.SetEventName("application")
	record.SetSeverity(severity)
	record.SetSeverityText(severity.String())
	record.SetBody(otellog.StringValue(message))
	record.AddAttributes(attrs...)
	otelLogger.Emit(ctx, record)
}
