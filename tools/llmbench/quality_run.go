package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// QualityConfig drives an Italian quality evaluation run.
type QualityConfig struct {
	Endpoint   string
	APIKey     string
	Model      string
	Corpus     []QualityCase
	Schemas    *QualitySchemas
	Timeout    time.Duration
	HTTPClient *http.Client
	// JudgeEndpoint, when set, enables optional LLM-as-judge (OFF by default).
	JudgeEndpoint string
	JudgeAPIKey   string
	JudgeModel    string
}

// QualityReport is the JSON output for quality mode (alongside the speed report shape).
type QualityReport struct {
	GeneratedAt string            `json:"generated_at"`
	Mode        string            `json:"mode"`
	Endpoint    string            `json:"endpoint"`
	Model       string            `json:"model"`
	Language    string            `json:"language"`
	Summary     QualitySummary    `json:"summary"`
	Cases       []CaseScore       `json:"cases"`
	Notes       []string          `json:"notes"`
	Judge       *QualityJudgeInfo `json:"judge,omitempty"`
}

// QualityJudgeInfo records optional judge configuration used for the run.
type QualityJudgeInfo struct {
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	Enabled  bool   `json:"enabled"`
}

// QualitySummary aggregates automatic scores (no human labels).
type QualitySummary struct {
	N                   int      `json:"n"`
	Passed              int      `json:"passed"`
	PassRate            float64  `json:"pass_rate"`
	SchemaCases         int      `json:"schema_cases"`
	SchemaValid         int      `json:"schema_valid"`
	SchemaValidRate     *float64 `json:"schema_valid_rate,omitempty"`
	VerifierCases       int      `json:"verifier_cases"`
	VerifierPassed      int      `json:"verifier_passed"`
	VerifierReplaced    int      `json:"verifier_replaced"`
	VerifierPassRate    *float64 `json:"verifier_pass_rate,omitempty"`
	VerifierReplaceRate *float64 `json:"verifier_replace_rate,omitempty"`
	FactCases           int      `json:"fact_cases"`
	FactAccuracyMean    *float64 `json:"fact_accuracy_mean,omitempty"`
	ItalianChecked      int      `json:"italian_checked"`
	ItalianOK           int      `json:"italian_ok"`
	ItalianOKRate       *float64 `json:"italian_ok_rate,omitempty"`
	JudgeMean           *float64 `json:"judge_mean,omitempty"`
}

// RunQuality evaluates the Italian corpus against an OpenAI-compatible endpoint.
func RunQuality(ctx context.Context, cfg QualityConfig) (*QualityReport, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint required")
	}
	if len(cfg.Corpus) == 0 {
		return nil, fmt.Errorf("quality corpus empty")
	}
	if cfg.Schemas == nil {
		return nil, fmt.Errorf("schemas required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	base := strings.TrimRight(cfg.Endpoint, "/")
	judgeOn := cfg.JudgeEndpoint != ""

	var scores []CaseScore
	for _, c := range cfg.Corpus {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		text, err := completeChat(ctx, client, base, cfg.APIKey, cfg.Model, c)
		sc := CaseScore{CaseID: c.ID, Kind: c.Kind}
		if err != nil {
			sc.Err = err.Error()
			sc.Problems = []string{err.Error()}
			scores = append(scores, sc)
			continue
		}
		var judgeScore *float64
		if judgeOn {
			if js, jerr := judgeCompletion(ctx, client, cfg, c, text); jerr == nil {
				judgeScore = &js
			} else {
				sc.Problems = append(sc.Problems, "judge: "+jerr.Error())
			}
		}
		scores = append(scores, scoreCase(c, text, cfg.Schemas, judgeScore))
	}

	rep := &QualityReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Mode:        "quality",
		Endpoint:    cfg.Endpoint,
		Model:       cfg.Model,
		Language:    "it",
		Summary:     summariseQuality(scores),
		Cases:       scores,
		Notes: []string{
			"Automatic scores only: JSON schema validity, verifier pass/replace, fact/number accuracy, Italian stop-word ratio.",
			"LLM-as-judge is off unless -judge-endpoint is set.",
			"No latency claims; use `llmbench run` for speed.",
		},
	}
	if judgeOn {
		rep.Judge = &QualityJudgeInfo{Endpoint: cfg.JudgeEndpoint, Model: cfg.JudgeModel, Enabled: true}
	}
	return rep, nil
}

func completeChat(ctx context.Context, client *http.Client, base, key, model string, c QualityCase) (string, error) {
	body := map[string]any{
		"model":       model,
		"messages":    c.Messages,
		"max_tokens":  c.MaxTokens,
		"stream":      false,
		"temperature": 0,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}

func judgeCompletion(ctx context.Context, client *http.Client, cfg QualityConfig, c QualityCase, answer string) (float64, error) {
	endpoint := strings.TrimRight(cfg.JudgeEndpoint, "/")
	model := cfg.JudgeModel
	if model == "" {
		model = cfg.Model
	}
	prompt := fmt.Sprintf(
		"Valuta da 1 a 5 (solo il numero) la qualità di questa risposta italiana per il caso %s (%s).\nRisposta:\n%s",
		c.ID, c.Kind, truncate(answer, 1500),
	)
	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
		"max_tokens":  8,
		"temperature": 0,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	key := cfg.JudgeAPIKey
	if key == "" {
		key = cfg.APIKey
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil || len(parsed.Choices) == 0 {
		return 0, fmt.Errorf("judge decode failed")
	}
	var score float64
	fmt.Sscanf(strings.TrimSpace(parsed.Choices[0].Message.Content), "%f", &score)
	if score < 1 || score > 5 {
		return 0, fmt.Errorf("judge score out of range")
	}
	return score, nil
}

func summariseQuality(scores []CaseScore) QualitySummary {
	s := QualitySummary{N: len(scores)}
	var factSum float64
	var judgeSum float64
	var judgeN int
	for _, c := range scores {
		if c.OK {
			s.Passed++
		}
		if c.SchemaValid != nil {
			s.SchemaCases++
			if *c.SchemaValid {
				s.SchemaValid++
			}
		}
		if c.VerifierPassed != nil {
			s.VerifierCases++
			if *c.VerifierPassed {
				s.VerifierPassed++
			}
			if c.VerifierReplaced != nil && *c.VerifierReplaced {
				s.VerifierReplaced++
			}
		}
		if c.FactAccuracy != nil {
			s.FactCases++
			factSum += *c.FactAccuracy
		}
		if c.ItalianOK != nil {
			s.ItalianChecked++
			if *c.ItalianOK {
				s.ItalianOK++
			}
		}
		if c.JudgeScore != nil {
			judgeSum += *c.JudgeScore
			judgeN++
		}
	}
	if s.N > 0 {
		s.PassRate = float64(s.Passed) / float64(s.N)
	}
	if s.SchemaCases > 0 {
		s.SchemaValidRate = floatPtr(float64(s.SchemaValid) / float64(s.SchemaCases))
	}
	if s.VerifierCases > 0 {
		s.VerifierPassRate = floatPtr(float64(s.VerifierPassed) / float64(s.VerifierCases))
		s.VerifierReplaceRate = floatPtr(float64(s.VerifierReplaced) / float64(s.VerifierCases))
	}
	if s.FactCases > 0 {
		s.FactAccuracyMean = floatPtr(factSum / float64(s.FactCases))
	}
	if s.ItalianChecked > 0 {
		s.ItalianOKRate = floatPtr(float64(s.ItalianOK) / float64(s.ItalianChecked))
	}
	if judgeN > 0 {
		s.JudgeMean = floatPtr(judgeSum / float64(judgeN))
	}
	return s
}
