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
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

var factGeneratorURL = "http://fact-generator:5000"

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
	shutdown, err := initTelemetry(context.Background())
	if err != nil {
		log.Fatalf("failed to initialize OpenTelemetry: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			log.Printf("telemetry shutdown failed: %v", err)
		}
	}()

	if url := os.Getenv("FACT_GENERATOR_URL"); url != "" {
		factGeneratorURL = url
	}

	http.Handle("/fact", otelhttp.NewHandler(http.HandlerFunc(handleFact), "GET /fact"))

	log.Println("Context-fetcher listening on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Printf("server stopped: %v", err)
	}
}

func initTelemetry(ctx context.Context) (func(context.Context) error, error) {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "context-fetcher"
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceNameKey.String(serviceName)))
	if err != nil {
		return nil, err
	}

	var exporter sdktrace.SpanExporter
	hasOTLPEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
	if hasOTLPEndpoint {
		exporter, err = otlptracehttp.New(ctx)
	} else {
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
	}
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

func handleFact(w http.ResponseWriter, r *http.Request) {
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
		commits, err := fetchCommits(r.Context(), repo)
		commitsCh <- commitsResult{commits, err}
	}()

	go func() {
		snippet, err := fetchDocSnippet(r.Context())
		docsCh <- docsResult{snippet, err}
	}()

	cr := <-commitsCh
	dr := <-docsCh

	if cr.err != nil {
		log.Printf("Failed to fetch commits: %v", cr.err)
		writeError(w, http.StatusBadGateway, "failed to fetch commits from GitHub")
		return
	}

	if dr.err != nil {
		log.Printf("Failed to fetch doc snippet: %v", dr.err)
	}

	genReq := generateRequest{
		Commits:     cr.commits,
		Repo:        repo,
		DocsSnippet: dr.snippet,
	}

	body, err := json.Marshal(genReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal request")
		return
	}

	client := newHTTPClient(30 * time.Second)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, factGeneratorURL+"/generate", bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to call fact-generator: %v", err)
		writeError(w, http.StatusBadGateway, "failed to call fact-generator")
		return
	}
	defer resp.Body.Close()

	io.Copy(w, resp.Body)
}

func fetchCommits(ctx context.Context, repo string) ([]commitPayload, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/commits?per_page=5", repo)
	client := newHTTPClient(10 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
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
	client := newHTTPClient(10 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
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

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(factResponse{Error: msg})
}
