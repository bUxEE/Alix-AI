package repro

import (
	"encoding/json"
	"fmt"
	"os"
)

// Result is the part of a tools/bench result (schemas/bench/result.schema.json)
// that the reproduction reads. Unknown keys are ignored here on purpose: the
// result schema and `bench check` own the full shape, this package owns only
// what it compares.
type Result struct {
	FormatVersion int    `json:"format_version"`
	RunID         string `json:"run_id"`
	Profile       string `json:"profile"`
	Emulated      bool   `json:"emulated"`
	Synthetic     bool   `json:"synthetic"`
	Host          struct {
		CPUModel     string `json:"cpu_model"`
		LogicalCores int    `json:"logical_cores"`
		RAMMB        int    `json:"ram_mb"`
		OS           string `json:"os"`
		Arch         string `json:"arch"`
	} `json:"host"`
	Provider struct {
		Name string `json:"name"`
	} `json:"provider"`
	Backend struct {
		Runtime        string `json:"runtime"`
		Name           string `json:"name"`
		RuntimeVersion string `json:"runtime_version"`
	} `json:"backend"`
	Model struct {
		RegistryID     string `json:"registry_id"`
		ArtifactSHA256 string `json:"artifact_sha256"`
		Quantization   string `json:"quantization"`
	} `json:"model"`
	Context struct {
		ConfiguredToks int `json:"configured_toks"`
	} `json:"context"`
	Corpus struct {
		ID         string `json:"id"`
		SHA256     string `json:"sha256"`
		CasesTotal int    `json:"cases_total"`
		CasesRun   int    `json:"cases_run"`
	} `json:"corpus"`
	Config struct {
		Role        string  `json:"role"`
		Constraint  string  `json:"constraint"`
		Stream      bool    `json:"stream"`
		Temperature float64 `json:"temperature"`
		MaxToks     int     `json:"max_toks"`
		Limit       *int    `json:"limit"`
		Prompt      struct {
			ID     string `json:"id"`
			SHA256 string `json:"sha256"`
		} `json:"prompt"`
		PercentileMethod string `json:"percentile_method"`
	} `json:"config"`
	Metrics Metrics `json:"metrics"`
	Latency struct {
		TotalMS             Stats   `json:"total_ms"`
		TTFTMS              Stats   `json:"ttft_ms"`
		DecodeToksPerSecond float64 `json:"decode_toks_per_second"`
	} `json:"latency"`
	Memory struct {
		Sampled   bool    `json:"sampled"`
		PeakRSSMB float64 `json:"peak_rss_mb"`
		SteadyMB  float64 `json:"steady_rss_mb"`
	} `json:"memory"`
	Errors struct {
		Count int `json:"count"`
	} `json:"errors"`
}

// Metric is one pass rate: passed of total, rate null when total is 0.
type Metric struct {
	Passed int      `json:"passed"`
	Total  int      `json:"total"`
	Rate   *float64 `json:"rate"`
}

// Metrics is the metric set of a result.
type Metrics struct {
	Cases                int    `json:"cases"`
	SchemaValid          Metric `json:"schema_valid"`
	CapabilityRouting    Metric `json:"capability_routing"`
	Intent               Metric `json:"intent"`
	FieldExactMatch      Metric `json:"field_exact_match"`
	FieldValueAccuracy   Metric `json:"field_value_accuracy"`
	CatalogueConformance Metric `json:"catalogue_conformance"`
	RefusalCorrectness   Metric `json:"refusal_correctness"`
	InjectionResistance  Metric `json:"injection_resistance"`
	Pass                 Metric `json:"pass"`
}

// Stats is the statistics block of a latency series; N is 0 when it was not measured.
type Stats struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
}

// named lists the metrics in report order.
func (m Metrics) named() []namedMetric {
	return []namedMetric{
		{"schema_valid", m.SchemaValid},
		{"capability_routing", m.CapabilityRouting},
		{"intent", m.Intent},
		{"field_exact_match", m.FieldExactMatch},
		{"field_value_accuracy", m.FieldValueAccuracy},
		{"catalogue_conformance", m.CatalogueConformance},
		{"refusal_correctness", m.RefusalCorrectness},
		{"injection_resistance", m.InjectionResistance},
		{"pass", m.Pass},
	}
}

type namedMetric struct {
	name string
	m    Metric
}

// LoadResult reads one bench result file.
func LoadResult(path string) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.FormatVersion != 1 {
		return nil, fmt.Errorf("%s: format_version %d is not supported (want 1)", path, r.FormatVersion)
	}
	return &r, nil
}
