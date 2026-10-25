package meta

import (
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	ok := Config{Dialect: DialectOpenAICompat, BaseURL: "https://llama-host.example.com", CredentialRef: "cred_1", Model: "meta-llama/Llama-3.3-70B-Instruct-Turbo"} // TEST-ONLY opaque reference id, not a key
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	// the Model API needs no base URL (it has a default) and no dialect (it is the default)
	if err := (Config{CredentialRef: "cred_1", Model: "muse-spark-1.3"}).Validate(); err != nil { // TEST-ONLY opaque reference id, not a key
		t.Fatal(err)
	}
	if c := (Config{CredentialRef: "cred_1", Model: "muse-spark-1.3"}).withDefaults(); c.BaseURL != "https://api.meta.ai" || c.Dialect != DialectModelAPI || c.APIPrefix != "/v1" { // TEST-ONLY opaque reference id, not a key
		t.Fatalf("defaults %+v", c)
	}
	for name, mut := range map[string]func(*Config){
		"no credential reference":              func(c *Config) { c.CredentialRef = "" },
		"no base URL on a hosted-Llama host":   func(c *Config) { c.BaseURL = "" },
		"an unknown dialect":                   func(c *Config) { c.Dialect = "gemini" },
		"an unknown reasoning effort":          func(c *Config) { c.ReasoningEffort = "none" },
		"a base URL that is not http(s)":       func(c *Config) { c.BaseURL = "ftp://llama-host.example.com" },
		"no model (there is no default model)": func(c *Config) { c.Model = "" },
		"a model id with a dot-dot path":       func(c *Config) { c.Model = "../x" },
		"a model id ending in a slash":         func(c *Config) { c.Model = "meta-llama/" },
		"a model id with a query":              func(c *Config) { c.Model = "llama?x=1" },
		"a bad profile":                        func(c *Config) { c.Profiles = map[string]string{"a b": "llama-4-scout"} },
		"an API prefix with dot-dot":           func(c *Config) { c.APIPrefix = "/v1/../admin" },
		"an API prefix with a query":           func(c *Config) { c.APIPrefix = "/v1?x=1" },
		"an API prefix without a slash":        func(c *Config) { c.APIPrefix = "v1" },
		"negative limit":                       func(c *Config) { c.MaxOutputToks = -1 },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, prefix := range []string{"/v1", "/openai/v1", "/api/v1"} {
		c := ok
		c.APIPrefix = prefix
		if err := c.Validate(); err != nil {
			t.Errorf("prefix %s: %v", prefix, err)
		}
	}
}

func TestContributorTierRefusedByDefault(t *testing.T) {
	base := Config{CredentialRef: "cred_1", Model: "muse-spark-1.3-contributor"} // TEST-ONLY opaque reference id, not a key
	if err := base.Validate(); err == nil {
		t.Fatal("a contributor-tier model was accepted without AllowContributorTier")
	}
	base.Model, base.Profiles = "muse-spark-1.3", map[string]string{"cheap": "muse-spark-1.2-contributor"}
	if err := base.Validate(); err == nil {
		t.Fatal("a contributor-tier profile model was accepted without AllowContributorTier")
	}
	base.AllowContributorTier = true
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	// the contributor tier has no built-in price: it cannot be budgeted without an operator table
	if p := (Config{Model: "x"}).profileFor("muse-spark-1.3-contributor"); p.prices != nil {
		t.Fatalf("%+v", p)
	}
}

func TestConfigStringHoldsNoSecret(t *testing.T) {
	s := (Config{BaseURL: "https://llama-host.example.com", CredentialRef: "cred_SECRETISH", Model: "llama-4-scout"}).String() // TEST-ONLY opaque reference id, not a key
	if strings.Contains(s, "cred_") || !strings.Contains(s, "llama-4-scout") {
		t.Fatal(s)
	}
}

func TestModelTable(t *testing.T) {
	for _, id := range []string{"muse-spark-1.3", "muse-spark-1.1"} {
		p := Config{Model: "x"}.profileFor(id)
		if p.prices == nil || p.prices.ToksIn.MicroUnits != 1_250_000 || p.prices.ToksOut.MicroUnits != 4_250_000 || p.context != 1_048_576 || p.output != 32768 {
			t.Errorf("%s: %+v", id, p)
		}
	}
	for _, id := range []string{"llama-4-scout", "meta-llama/llama-4-scout", "meta-llama/Llama-4-Scout-Instruct", "llama-4-scout-2026-04-05"} {
		p := Config{Model: "x"}.profileFor(id)
		if p.prices == nil || p.prices.ToksIn.MicroUnits != 100_000 || p.prices.ToksOut.MicroUnits != 300_000 || p.context != 131072 {
			t.Errorf("%s: %+v", id, p)
		}
	}
	// an unknown model gets conservative limits and no price
	p := Config{Model: "x"}.profileFor("acme/llama-never-seen")
	if p.prices != nil || p.output != unknownOutputToks || p.context != unknownContextToks {
		t.Fatalf("%+v", p)
	}
	// the operator's override applies to the configured model only
	c := Config{Model: "acme/llama-x", MaxOutputToks: 1000, MaxContextToks: 2000}
	if p := c.profileFor("acme/llama-x"); p.output != 1000 || p.context != 2000 {
		t.Fatalf("%+v", p)
	}
	if p := c.profileFor("acme/llama-other"); p.output != unknownOutputToks {
		t.Fatalf("%+v", p)
	}
}
