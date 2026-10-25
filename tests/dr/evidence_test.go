package dr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The drill records what it measured and what it observed. A default `go test` only logs; with
// ALIXAI_DR_EVIDENCE_DIR set, TestMain writes the report and the raw results there.

type timing struct {
	Name   string `json:"name"`
	Millis int64  `json:"ms"`
	Detail string `json:"detail,omitempty"`
}

type observation struct {
	Scenario string `json:"scenario"`
	Text     string `json:"text"`
}

var evidence struct {
	mu           sync.Mutex
	Timings      []timing
	Observations []observation
	Env          []envRow
}

func recordTiming(name string, d time.Duration, detail string, args ...any) {
	evidence.mu.Lock()
	defer evidence.mu.Unlock()
	evidence.Timings = append(evidence.Timings, timing{Name: name, Millis: d.Milliseconds(), Detail: fmt.Sprintf(detail, args...)})
}

func recordObservation(scenario, text string, args ...any) {
	evidence.mu.Lock()
	defer evidence.mu.Unlock()
	evidence.Observations = append(evidence.Observations, observation{Scenario: scenario, Text: fmt.Sprintf(text, args...)})
}

func recordEnv(key, value string) {
	evidence.mu.Lock()
	defer evidence.mu.Unlock()
	for i := range evidence.Env {
		if evidence.Env[i].Key == key {
			evidence.Env[i].Value = value
			return
		}
	}
	evidence.Env = append(evidence.Env, envRow{key, value})
}

func writeEvidence(dir string) error {
	evidence.mu.Lock()
	defer evidence.mu.Unlock()
	if err := checkComplete(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(map[string]any{
		"env": evidence.Env, "timings": evidence.Timings, "observations": evidence.Observations,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "results.json"), append(raw, byte(10)), 0o644); err != nil {
		return err
	}
	report, err := renderReport(time.Now().UTC())
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), report, 0o644)
}
