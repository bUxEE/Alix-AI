package answer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the defaults are invalid: %v", err)
	}
	if FromConfig() != DefaultConfig() {
		t.Errorf("the registered section does not start from the defaults")
	}
	d := DefaultConfig()
	if d.MaxClaims > MaxClaimsCeiling || d.MaxCitations > MaxCitationsCeiling {
		t.Errorf("a default breaks a schema cap: %+v", d)
	}
	bad := map[string]func(*Config){
		"max_claims 0":              func(c *Config) { c.MaxClaims = 0 },
		"max_claims over schema":    func(c *Config) { c.MaxClaims = MaxClaimsCeiling + 1 },
		"max_citations 0":           func(c *Config) { c.MaxCitations = 0 },
		"max_citations over schema": func(c *Config) { c.MaxCitations = MaxCitationsCeiling + 1 },
		"max_output_toks tiny":      func(c *Config) { c.MaxOutputToks = 1 },
		"max_output_toks huge":      func(c *Config) { c.MaxOutputToks = 1 << 20 },
		"min_coverage low":          func(c *Config) { c.MinCoverage = 0.3 },
		"min_coverage over 1":       func(c *Config) { c.MinCoverage = 1.5 },
		"min_coverage NaN":          func(c *Config) { c.MinCoverage = nan() },
		"window 0":                  func(c *Config) { c.WindowSentences = 0 },
		"window 9":                  func(c *Config) { c.WindowSentences = 9 },
		"min_claim_terms 0":         func(c *Config) { c.MinClaimTerms = 0 },
		"generate_timeout 0":        func(c *Config) { c.GenerateTimeout = 0 },
		"generate_timeout 1h":       func(c *Config) { c.GenerateTimeout = time.Hour },
		"ref_ttl 0":                 func(c *Config) { c.RefTTL = 0 },
		"ref_ttl 30d":               func(c *Config) { c.RefTTL = 30 * 24 * time.Hour },
		"max_refs 0":                func(c *Config) { c.MaxRefs = 0 },
		"max_refs unbounded":        func(c *Config) { c.MaxRefs = maxRefsCeiling + 1 },
		"preview negative":          func(c *Config) { c.PreviewRunes = -1 },
		"preview huge":              func(c *Config) { c.PreviewRunes = 1 << 20 },
	}
	for name, mut := range bad {
		c := DefaultConfig()
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Previews can be switched off.
	c := DefaultConfig()
	c.PreviewRunes = 0
	if err := c.Validate(); err != nil {
		t.Errorf("preview_runes 0: %v", err)
	}
}

// The section loads from YAML and from the environment, and a bad value fails the load.
func TestConfigLoadsFromYAMLAndEnvironment(t *testing.T) {
	load := func(yaml string, env map[string]string) (Config, error) {
		reg := config.NewRegistry()
		c := DefaultConfig()
		reg.Register(SectionName, &c)
		path := filepath.Join(t.TempDir(), "alixai.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		err := reg.LoadFrom(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		return c, err
	}
	c, err := load("rag_answer:\n  max_claims: 6\n  min_coverage: 0.9\n  ref_ttl: 5m\n", map[string]string{"ALIXAI_RAG_ANSWER_MAX_OUTPUT_TOKS": "300"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.MaxClaims != 6 || c.MinCoverage != 0.9 || c.RefTTL != 5*time.Minute || c.MaxOutputToks != 300 || c.MaxCitations != DefaultMaxCitations {
		t.Errorf("loaded %+v", c)
	}
	if _, err := load("rag_answer:\n  max_claims: 500\n", nil); err == nil {
		t.Error("an out-of-range value loaded")
	}
	if _, err := load("rag_answer:\n  max_claimz: 5\n", nil); err == nil {
		t.Error("an unknown key loaded")
	}
}

func nan() float64 {
	var z float64
	return z / z
}
