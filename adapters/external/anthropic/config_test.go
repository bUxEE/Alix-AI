package anthropic

import (
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	ok := Config{CredentialRef: "cred_1", Model: "claude-haiku-4-5"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no credential reference":              func(c *Config) { c.CredentialRef = "" },
		"no model (there is no default model)": func(c *Config) { c.Model = "" },
		"a model id with a path":               func(c *Config) { c.Model = "../x" },
		"a bad profile":                        func(c *Config) { c.Profiles = map[string]string{"a b": "claude-haiku-4-5"} },
		"unknown effort":                       func(c *Config) { c.Effort = "ludicrous" },
		"negative limit":                       func(c *Config) { c.MaxOutputToks = -1 },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestConfigStringHoldsNoSecret(t *testing.T) {
	s := (Config{CredentialRef: "cred_SECRETISH", Model: "claude-haiku-4-5"}).String()
	if strings.Contains(s, "cred_") || !strings.Contains(s, "claude-haiku-4-5") {
		t.Fatal(s)
	}
}

func TestModelTable(t *testing.T) {
	for id, wantIn := range map[string]int64{"claude-haiku-4-5": 1_000_000, "claude-haiku-4-5-20251001": 1_000_000, "claude-opus-5-5": 4_000_000} {
		p := Config{Model: "x"}.profileFor(id)
		if p.prices == nil || p.prices.ToksIn.MicroUnits != wantIn {
			t.Errorf("%s: %+v", id, p.prices)
		}
	}
	// an unknown model gets conservative limits, no price and no optional features
	p := Config{Model: "x"}.profileFor("claude-never-seen")
	if p.prices != nil || p.output != unknownOutputToks || p.forced || p.sampling || p.effort {
		t.Fatalf("%+v", p)
	}
	// the operator's override applies to the configured model only
	yes := true
	c := Config{Model: "claude-x", Sampling: &yes, ForcedToolChoice: &yes, MaxOutputToks: 1000}
	if p := c.profileFor("claude-x"); !p.sampling || !p.forced || p.output != 1000 {
		t.Fatalf("%+v", p)
	}
	if p := c.profileFor("claude-other"); p.sampling || p.forced {
		t.Fatalf("%+v", p)
	}
}
