package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Turn is one recorded AlixAI-shaped chat completion request body fragment.
type Turn struct {
	ID       string           `json:"id"`
	Language string           `json:"language"` // en | it
	Messages []map[string]any `json:"messages"`
	Tools    []map[string]any `json:"tools,omitempty"`
	// ApproxInputTokens is a recorded estimate for sizing (not a tokenizer count).
	ApproxInputTokens int `json:"approx_input_tokens"`
	MaxTokens         int `json:"max_tokens"`
}

// LoadCorpus reads every *.json turn under dir (non-recursive).
func LoadCorpus(dir string) ([]Turn, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Turn
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var t Turn
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if t.ID == "" {
			t.ID = e.Name()
		}
		if t.MaxTokens <= 0 {
			t.MaxTokens = 300
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no turns in %s", dir)
	}
	return out, nil
}
