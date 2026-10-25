package gemini

import (
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	ok := Config{CredentialRef: "cred_1", Model: testModel}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	oauth := Config{CredentialRef: "cred_1", Model: testModel, Auth: AuthOAuth, UserProject: "my-clinic-project"}
	if err := oauth.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no credential reference":              func(c *Config) { c.CredentialRef = "" },
		"no model (there is no default model)": func(c *Config) { c.Model = "" },
		"a model id with a path":               func(c *Config) { c.Model = "../x" },
		"a model id with the models/ prefix":   func(c *Config) { c.Model = "models/gemini-x" },
		"a model id with a colon":              func(c *Config) { c.Model = "gemini-x:generateContent" },
		"a bad profile":                        func(c *Config) { c.Profiles = map[string]string{"a b": testModel} },
		"unknown auth kind":                    func(c *Config) { c.Auth = "basic" },
		"a user project with an API key":       func(c *Config) { c.UserProject = "my-clinic-project" },
		"scopes with an API key":               func(c *Config) { c.OAuthScopes = []string{ScopeCloudPlatform} },
		"unknown tool mode":                    func(c *Config) { c.ToolMode = "ANY" },
		"negative limit":                       func(c *Config) { c.MaxOutputToks = -1 },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, mut := range map[string]func(*Config){
		"a malformed project id": func(c *Config) { c.UserProject = "Not A Project" },
		"a malformed scope":      func(c *Config) { c.OAuthScopes = []string{"openid"} },
	} {
		c := oauth
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestConfigStringHoldsNoSecret(t *testing.T) {
	s := (Config{CredentialRef: "cred_SECRETISH", Model: testModel}).String()
	if strings.Contains(s, "cred_") || !strings.Contains(s, testModel) {
		t.Fatal(s)
	}
}

func TestModelTable(t *testing.T) {
	for id, wantOut := range map[string]int64{"gemini-3.5-flash-lite": 2_500_000, "gemini-2.5-flash-lite": 400_000, "gemini-3.8-flash": 7_500_000} {
		p := Config{Model: "x"}.profileFor(id)
		if p.prices == nil || p.prices.ToksOut.MicroUnits != wantOut {
			t.Errorf("%s: %+v", id, p.prices)
		}
	}
	// a model whose limits were not read gets the conservative ones but keeps its price
	if p := (Config{Model: "x"}).profileFor("gemini-3.8-flash"); p.context != unknownContextToks || p.output != unknownOutputToks {
		t.Fatalf("%+v", p)
	}
	// an unknown model gets conservative limits and no price
	if p := (Config{Model: "x"}).profileFor("gemini-never-seen"); p.prices != nil || p.output != unknownOutputToks {
		t.Fatalf("%+v", p)
	}
	// the operator's override applies to the configured model only
	c := Config{Model: "gemini-x", MaxOutputToks: 1000}
	if p := c.profileFor("gemini-x"); p.output != 1000 {
		t.Fatalf("%+v", p)
	}
	if p := c.profileFor("gemini-other"); p.output != unknownOutputToks {
		t.Fatalf("%+v", p)
	}
}
