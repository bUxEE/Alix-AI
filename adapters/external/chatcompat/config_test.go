package chatcompat

import (
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	ok := Config{BaseURL: "https://llama-host.example.com", CredentialRef: "cred_1", WireModel: "meta-llama/Llama-3.3-70B-Instruct-Turbo"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no credential reference":  func(c *Config) { c.CredentialRef = "" },
		"no base URL":              func(c *Config) { c.BaseURL = "" },
		"unknown reasoning effort": func(c *Config) { c.ReasoningEffort = "none" },
		"bad base URL":             func(c *Config) { c.BaseURL = "ftp://x.com" },
		"no model":                 func(c *Config) { c.WireModel = "" },
		"bad model id":             func(c *Config) { c.WireModel = "../x" },
		"negative limit":           func(c *Config) { c.MaxOutputToks = -1 },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestConfigStringHoldsNoSecret(t *testing.T) {
	s := (Config{BaseURL: "https://llama-host.example.com", CredentialRef: "cred_SECRETISH", WireModel: "llama-4-scout"}).String()
	if strings.Contains(s, "cred_") || !strings.Contains(s, "llama-4-scout") {
		t.Fatal(s)
	}
}

func TestReplyModelMapping(t *testing.T) {
	c := Config{BaseURL: "https://x.com", CredentialRef: "cred_1", WireModel: "vendor/model", ReplyModel: "registry-model"}
	if c.profileFor("vendor/model").id != "vendor/model" {
		t.Fatal()
	}
}
