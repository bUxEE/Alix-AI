// Package repro is the engine behind the independently repeatable benchmark package
// (benchmarks/repro, master plan 17.3 and milestone 6): the published manifest, the
// check of its pinned inputs, and the comparison of a reproduced result with the
// published one. Nothing here starts a model, a container or the network.
package repro

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// ManifestPath is where the published manifest lives, relative to the repository root.
const ManifestPath = "benchmarks/repro/manifest.json"

// Manifest is the published list of everything a third party needs to repeat the
// published results: the runtime image, the model and corpus hashes, the exact
// benchmark configuration, the runs and the tolerances a reproduction is judged by.
type Manifest struct {
	FormatVersion int                `json:"format_version"`
	ID            string             `json:"id"`
	Title         string             `json:"title"`
	PublishedFrom string             `json:"published_from"`
	Runtime       Runtime            `json:"runtime"`
	Profiles      map[string]Profile `json:"profiles"`
	Bench         Bench              `json:"bench"`
	Corpora       []Corpus           `json:"corpora"`
	Models        []Model            `json:"models"`
	Runs          []Run              `json:"runs"`
	Tolerances    Tolerances         `json:"tolerances"`
}

// Runtime pins the container the results were measured on.
type Runtime struct {
	Compose        string   `json:"compose"`
	Image          string   `json:"image"`
	Version        string   `json:"version"`
	Backend        string   `json:"backend"`
	BackendGallery FileHash `json:"backend_gallery"`
	Override       string   `json:"override"`
}

// FileHash is a repository file and the SHA-256 of its bytes.
type FileHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Profile is a resource profile (a cgroup cap on one container; the results are emulated).
type Profile struct {
	CLI      string `json:"cli"`
	Compose  string `json:"compose"`
	MemLimit string `json:"mem_limit"`
	CPUs     int    `json:"cpus"`
	MemMB    int    `json:"mem_mb"`
}

// Bench is the benchmark configuration every run shares; a reproduction that uses
// another value is not comparable.
type Bench struct {
	Tool             string  `json:"tool"`
	Role             string  `json:"role"`
	Provider         string  `json:"provider"`
	Constraint       string  `json:"constraint"`
	PromptID         string  `json:"prompt_id"`
	PromptSHA256     string  `json:"prompt_sha256"`
	ContextToks      int     `json:"context_toks"`
	MaxToks          int     `json:"max_toks"`
	Warmup           int     `json:"warmup"`
	Timeout          string  `json:"timeout"`
	Temperature      float64 `json:"temperature"`
	SampleInterval   string  `json:"sample_interval"`
	PercentileMethod string  `json:"percentile_method"`
}

// Corpus is a benchmark corpus and its checksum.
type Corpus struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Cases  int    `json:"cases"`
}

// Model is a pinned model file. Definition is the LocalAI model definition that
// downloads it (its download_files entry carries the same hash).
type Model struct {
	ID           string `json:"id"`
	Filename     string `json:"filename"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	Quantization string `json:"quantization"`
	Definition   string `json:"definition"`
}

// Run is one published result: a model under a profile in one configuration
// (A: no gate; G: the deterministic out-of-scope gate in front of the model).
type Run struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	Profile      string `json:"profile"`
	Config       string `json:"config"`
	Gate         bool   `json:"gate"`
	Corpus       string `json:"corpus"`
	Result       string `json:"result"`
	ResultSHA256 string `json:"result_sha256"`
}

// Tolerances are the defaults of the comparison. Quality is an absolute difference
// of a pass rate; latency and memory are relative to the published value. They are
// wide on purpose: greedy decoding on another CPU or another build still moves
// some outputs (M0-17: 14 of 80 outputs differed between two identical runs).
type Tolerances struct {
	QualityRate float64 `json:"quality_rate"`
	LatencyRel  float64 `json:"latency_rel"`
	MemoryRel   float64 `json:"memory_rel"`
}

var (
	hex64   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	idRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	imageRe = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// LoadManifest reads and validates a manifest. Unknown keys are an error, so a
// typo in a pinned field cannot silently disarm a check.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseManifest(data)
}

// ParseManifest decodes and validates manifest bytes.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("manifest: trailing data after the document")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest's own consistency: shapes, hashes and references.
func (m *Manifest) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf("manifest: "+format, a...)) }
	if m.FormatVersion != 1 {
		bad("format_version %d is not supported (want 1)", m.FormatVersion)
	}
	if !idRe.MatchString(m.ID) {
		bad("id %q is not a valid id", m.ID)
	}
	if m.Runtime.Compose == "" || m.Runtime.Override == "" {
		bad("runtime.compose and runtime.override are required")
	}
	if !imageRe.MatchString(m.Runtime.Image) {
		bad("runtime.image %q is not pinned by digest", m.Runtime.Image)
	}
	if m.Runtime.Version == "" || m.Runtime.Backend == "" {
		bad("runtime.version and runtime.backend are required")
	}
	if m.Runtime.BackendGallery.Path == "" || !hex64.MatchString(m.Runtime.BackendGallery.SHA256) {
		bad("runtime.backend_gallery needs a path and a sha256")
	}
	if len(m.Profiles) == 0 {
		bad("no profiles")
	}
	for _, name := range sortedKeys(m.Profiles) {
		p := m.Profiles[name]
		if p.CLI == "" || p.Compose == "" || p.MemLimit == "" || p.CPUs <= 0 || p.MemMB <= 0 {
			bad("profile %q is incomplete", name)
		}
	}
	b := m.Bench
	if b.Tool == "" || b.Role == "" || b.Provider == "" || b.Constraint == "" || b.PromptID == "" || b.ContextToks <= 0 || b.MaxToks <= 0 || b.Timeout == "" || b.SampleInterval == "" {
		bad("bench is incomplete")
	}
	if !hex64.MatchString(b.PromptSHA256) {
		bad("bench.prompt_sha256 is not a sha256")
	}
	corpora := map[string]bool{}
	for _, c := range m.Corpora {
		if c.ID == "" || c.Path == "" || c.Cases <= 0 || !hex64.MatchString(c.SHA256) {
			bad("corpus %q is incomplete", c.ID)
		}
		if corpora[c.ID] {
			bad("duplicate corpus %q", c.ID)
		}
		corpora[c.ID] = true
	}
	models := map[string]bool{}
	for _, md := range m.Models {
		if md.ID == "" || md.Filename == "" || md.SizeBytes <= 0 || md.Definition == "" || !hex64.MatchString(md.SHA256) || filepath.Base(md.Filename) != md.Filename {
			bad("model %q is incomplete", md.ID)
		}
		if models[md.ID] {
			bad("duplicate model %q", md.ID)
		}
		models[md.ID] = true
	}
	if len(m.Runs) == 0 {
		bad("no runs")
	}
	runs := map[string]bool{}
	for _, r := range m.Runs {
		if !idRe.MatchString(r.ID) {
			bad("run id %q is not a valid id", r.ID)
		}
		if runs[r.ID] {
			bad("duplicate run %q", r.ID)
		}
		runs[r.ID] = true
		if !models[r.Model] {
			bad("run %q names unknown model %q", r.ID, r.Model)
		}
		if _, ok := m.Profiles[r.Profile]; !ok {
			bad("run %q names unknown profile %q", r.ID, r.Profile)
		}
		if !corpora[r.Corpus] {
			bad("run %q names unknown corpus %q", r.ID, r.Corpus)
		}
		if r.Result == "" || !hex64.MatchString(r.ResultSHA256) {
			bad("run %q needs a result path and a sha256", r.ID)
		}
	}
	t := m.Tolerances
	if t.QualityRate <= 0 || t.QualityRate >= 1 || t.LatencyRel <= 0 || t.MemoryRel <= 0 {
		bad("tolerances must be positive (quality_rate below 1)")
	}
	return errors.Join(errs...)
}

// Model returns the pinned model with the id.
func (m *Manifest) Model(id string) (Model, bool) {
	for _, md := range m.Models {
		if md.ID == id {
			return md, true
		}
	}
	return Model{}, false
}

// Corpus returns the pinned corpus with the id.
func (m *Manifest) Corpus(id string) (Corpus, bool) {
	for _, c := range m.Corpora {
		if c.ID == id {
			return c, true
		}
	}
	return Corpus{}, false
}

// FindRoot walks up from dir to the directory that holds go.mod.
func FindRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; {
		if st, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && !st.IsDir() {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod above %s; run from inside the repository or pass -root", abs)
		}
		d = parent
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
