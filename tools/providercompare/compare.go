package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProviderReport is everything the comparison holds about one provider.
type ProviderReport struct {
	Provider string       `json:"provider"`
	Profile  profileFacts `json:"profile"`
	// Scenarios is the capability and failure-behaviour replay (recorded fixtures).
	Scenarios []Outcome `json:"scenarios"`
	// Corpora is the replay of the shared corpora.
	Corpora []CorpusRun `json:"corpora"`
	// ReferenceTurnCostMicros is the cost the adapter computed for the 1,500 in / 200 out turn.
	ReferenceTurnCostMicros int64  `json:"reference_turn_cost_micros"`
	Currency                string `json:"currency"`
}

// LocalRow is one recorded local-mode run (benchmarks/results/m5).
type LocalRow struct {
	File      string   `json:"file"`
	Model     string   `json:"model"`
	Profile   string   `json:"profile"`
	Pass      string   `json:"pass"`
	Routing   string   `json:"routing"`
	Refusal   string   `json:"refusal"`
	Injection string   `json:"injection"`
	P50MS     *float64 `json:"p50_ms"`
	P95MS     *float64 `json:"p95_ms"`
	PeakRSSMB *float64 `json:"peak_rss_mb"`
	Label     string   `json:"label"`
}

// Comparison is the whole report in data form.
type Comparison struct {
	Providers []ProviderReport `json:"providers"`
	Local     []LocalRow       `json:"local"`
}

// Build replays the recorded fixtures through the four real adapters and reads the recorded
// local results. Nothing leaves the machine: every adapter dials a loopback stub.
func Build(ctx context.Context, root string) (*Comparison, error) {
	tasks := map[string][]task{}
	for _, c := range corpusOrder {
		ts, err := loadTasks(root, c)
		if err != nil {
			return nil, err
		}
		tasks[c] = ts
	}
	cmp := &Comparison{}
	for _, name := range providerNames {
		s, closeFn, err := newSubject(name, latencyModel{})
		if err != nil {
			return nil, err
		}
		rep := ProviderReport{Provider: name, Profile: s.profile, Currency: s.profile.Pricing.Currency}
		rep.Scenarios = runAllScenarios(ctx, s)
		for _, o := range rep.Scenarios {
			if o.Scenario == "text" && o.Result == "ok" {
				rep.ReferenceTurnCostMicros = o.CostMicros
			}
		}
		for _, c := range corpusOrder {
			rep.Corpora = append(rep.Corpora, runCorpus(ctx, s, c, tasks[c]))
		}
		closeFn()
		cmp.Providers = append(cmp.Providers, rep)
	}
	local, err := loadLocal(root)
	if err != nil {
		return nil, err
	}
	cmp.Local = local
	return cmp, nil
}

// loadLocal reads the recorded local-mode results of M5-09 (real LocalAI runs in cgroup-capped
// containers; their own files say emulated).
func loadLocal(root string) ([]LocalRow, error) {
	files, err := filepath.Glob(filepath.Join(root, "benchmarks", "results", "m5", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	type ratio struct {
		Passed int `json:"passed"`
		Total  int `json:"total"`
	}
	var rows []LocalRow
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var d struct {
			Profile string `json:"profile"`
			Model   struct {
				RegistryID string `json:"registry_id"`
			} `json:"model"`
			Metrics struct {
				Pass      ratio `json:"pass"`
				Routing   ratio `json:"capability_routing"`
				Refusal   ratio `json:"refusal_correctness"`
				Injection ratio `json:"injection_resistance"`
			} `json:"metrics"`
			Latency struct {
				Total struct {
					P50 *float64 `json:"p50"`
					P95 *float64 `json:"p95"`
				} `json:"total_ms"`
			} `json:"latency"`
			Memory struct {
				Peak *float64 `json:"peak_rss_mb"`
			} `json:"memory"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		frac := func(r ratio) string { return fmt.Sprintf("%d/%d", r.Passed, r.Total) }
		rows = append(rows, LocalRow{
			File: filepath.Base(f), Model: d.Model.RegistryID, Profile: d.Profile,
			Pass: frac(d.Metrics.Pass), Routing: frac(d.Metrics.Routing), Refusal: frac(d.Metrics.Refusal), Injection: frac(d.Metrics.Injection),
			P50MS: d.Latency.Total.P50, P95MS: d.Latency.Total.P95, PeakRSSMB: d.Memory.Peak, Label: LabelRecorded,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		if rows[i].Profile != rows[j].Profile {
			return rows[i].Profile < rows[j].Profile
		}
		return rows[i].File < rows[j].File
	})
	return rows, nil
}

// scenarioByName finds an outcome.
func (p ProviderReport) scenario(name string) (Outcome, bool) {
	for _, o := range p.Scenarios {
		if o.Scenario == name {
			return o, true
		}
	}
	return Outcome{}, false
}

func (p ProviderReport) corpus(name string) (CorpusRun, bool) {
	for _, c := range p.Corpora {
		if c.Corpus == name {
			return c, true
		}
	}
	return CorpusRun{}, false
}

// micros formats integer micro-units as a decimal amount without floats: 2725 -> "0.002725".
func micros(m int64, currency string) string {
	whole, frac := m/1_000_000, m%1_000_000
	s := fmt.Sprintf("%d.%06d", whole, frac)
	if currency != "" {
		s += " " + currency
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func join(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}
