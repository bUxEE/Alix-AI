package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config drives a benchmark run.
type Config struct {
	Endpoint      string // e.g. http://127.0.0.1:8000/v1
	APIKey        string
	Model         string
	Corpus        []Turn
	Concurrencies []int
	TargetFull    time.Duration // p95 full-answer budget for "max concurrent users"
	Timeout       time.Duration
	CacheSalt     string // optional; sent as cache_salt for tenant isolation tests
	HTTPClient    *http.Client
}

// Report is the JSON output.
type Report struct {
	GeneratedAt               string      `json:"generated_at"`
	Endpoint                  string      `json:"endpoint"`
	Model                     string      `json:"model"`
	TargetFull                string      `json:"target_full_answer"`
	MaxConcurrentWithinTarget int         `json:"max_concurrent_users_within_target"`
	Aggregates                []Aggregate `json:"aggregates"`
	Samples                   []Sample    `json:"samples"`
	Notes                     []string    `json:"notes"`
}

// Run executes the harness.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint required")
	}
	if len(cfg.Corpus) == 0 {
		return nil, fmt.Errorf("corpus empty")
	}
	if len(cfg.Concurrencies) == 0 {
		cfg.Concurrencies = []int{1, 4, 16, 64}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	base := strings.TrimRight(cfg.Endpoint, "/")
	var samples []Sample
	for _, c := range cfg.Concurrencies {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		wave := runWave(ctx, client, base, cfg, c)
		samples = append(samples, wave...)
	}
	for i := range samples {
		samples[i].TTFT_ms = float64(samples[i].TTFT.Milliseconds())
		samples[i].Full_ms = float64(samples[i].Full.Milliseconds())
	}
	aggs := Summarise(samples, cfg.TargetFull)
	rep := &Report{
		GeneratedAt:               time.Now().UTC().Format(time.RFC3339),
		Endpoint:                  cfg.Endpoint,
		Model:                     cfg.Model,
		TargetFull:                cfg.TargetFull.String(),
		MaxConcurrentWithinTarget: MaxConcurrentWithinTarget(aggs),
		Aggregates:                aggs,
		Samples:                   samples,
		Notes: []string{
			"TTFT from first SSE data token (or full body start if non-stream).",
			"approx_input_tokens on corpus turns are estimates, not tokenizer counts.",
		},
	}
	return rep, nil
}

func runWave(ctx context.Context, client *http.Client, base string, cfg Config, concurrency int) []Sample {
	n := concurrency
	if n > len(cfg.Corpus)*4 {
		// enough work items: cycle the corpus
	}
	jobs := make([]Turn, n)
	for i := 0; i < n; i++ {
		jobs[i] = cfg.Corpus[i%len(cfg.Corpus)]
	}
	out := make([]Sample, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i, turn := i, jobs[i]
		go func() {
			defer wg.Done()
			out[i] = doOne(ctx, client, base, cfg, turn, concurrency)
		}()
	}
	wg.Wait()
	return out
}

func doOne(ctx context.Context, client *http.Client, base string, cfg Config, turn Turn, concurrency int) Sample {
	s := Sample{TurnID: turn.ID, Concurrency: concurrency}
	body := map[string]any{
		"model":       cfg.Model,
		"messages":    turn.Messages,
		"max_tokens":  turn.MaxTokens,
		"stream":      true,
		"temperature": 0,
	}
	if len(turn.Tools) > 0 {
		body["tools"] = turn.Tools
	}
	if cfg.CacheSalt != "" {
		body["cache_salt"] = cfg.CacheSalt
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		s.Err = err.Error()
		return s
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		s.Err = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
		s.Full = time.Since(start)
		return s
	}
	ttftSet := false
	var completionToks, promptToks int
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if !ttftSet && len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			s.TTFT = time.Since(start)
			ttftSet = true
		}
		if chunk.Usage != nil {
			promptToks = chunk.Usage.PromptTokens
			completionToks = chunk.Usage.CompletionTokens
		}
	}
	s.Full = time.Since(start)
	if !ttftSet {
		s.TTFT = s.Full
	}
	s.PromptTokens = promptToks
	s.CompletionTokens = completionToks
	if s.CompletionTokens == 0 {
		// Fake / some servers omit usage: approximate from max_tokens budget unused.
		s.CompletionTokens = turn.MaxTokens / 2
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
