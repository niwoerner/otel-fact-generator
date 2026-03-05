package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"
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
	if url := os.Getenv("FACT_GENERATOR_URL"); url != "" {
		factGeneratorURL = url
	}

	http.HandleFunc("/fact", handleFact)

	log.Println("Context-fetcher listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
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
		commits, err := fetchCommits(repo)
		commitsCh <- commitsResult{commits, err}
	}()

	go func() {
		snippet, err := fetchDocSnippet()
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

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(factGeneratorURL+"/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("Failed to call fact-generator: %v", err)
		writeError(w, http.StatusBadGateway, "failed to call fact-generator")
		return
	}
	defer resp.Body.Close()

	io.Copy(w, resp.Body)
}

func fetchCommits(repo string) ([]commitPayload, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/commits?per_page=5", repo)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
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

func fetchDocSnippet() (string, error) {
	page := docPages[rand.Intn(len(docPages))]
	url := fmt.Sprintf(
		"https://raw.githubusercontent.com/open-telemetry/opentelemetry.io/main/content/en/docs/concepts/%s",
		page,
	)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
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
