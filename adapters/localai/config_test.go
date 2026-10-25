package localai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/config"
)

func TestConfigValidate(t *testing.T) {
	good := testConfig("http://127.0.0.1:8080")
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	long := strings.Repeat("m", 201)
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"model with a space", func(c *Config) { c.Models.Controller = "a model" }},
		{"model with a newline", func(c *Config) { c.Models.Router = "a\nb" }},
		{"model starting with a dash", func(c *Config) { c.Models.Embedder = "-x" }},
		{"model too long", func(c *Config) { c.Models.Controller = long }},
		{"profile name with capitals", func(c *Config) { c.Profiles["Lite"] = ModelSet{} }},
		{"profile name empty", func(c *Config) { c.Profiles[""] = ModelSet{} }},
		{"profile model invalid", func(c *Config) { c.Profiles["lite"] = ModelSet{Controller: "bad name"} }},
		{"batch size too large", func(c *Config) { c.EmbedBatchSize = 257 }},
		{"batch size negative", func(c *Config) { c.EmbedBatchSize = -1 }},
		{"negative max output", func(c *Config) { c.DefaultMaxOutputToks = -1 }},
		{"negative request timeout", func(c *Config) { c.RequestTimeout = -time.Second }},
		{"negative health timeout", func(c *Config) { c.HealthTimeout = -time.Second }},
		{"negative response limit", func(c *Config) { c.MaxResponseBytes = -1 }},
		{"qualified version with a space", func(c *Config) { c.QualifiedVersions = []string{"v4 .10"} }},
		{"qualified version empty", func(c *Config) { c.QualifiedVersions = []string{""} }},
		{"a literal key instead of a reference", func(c *Config) { c.APIKeyRef = "sk-pasted-key" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig("http://127.0.0.1:8080")
			tt.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "sk-pasted-key") {
				t.Errorf("error repeats the pasted key: %v", err)
			}
		})
	}
}

// The adapter does not register itself; the composition root registers
// Config as the "localai" section. This checks the type works as one: YAML
// keys, environment overrides, and refusal of an unsafe endpoint at load time
// (the `config check` of SEC-065).
func TestConfigWorksAsASection(t *testing.T) {
	yamlText := `
localai:
  endpoint: https://localai.internal:8443
  api_key_ref: env:ALIXAI_LOCALAI_KEY
  tls:
    server_name: localai.internal
  models:
    controller: granite-controller
    embedder: granite-embed
  profiles:
    lite:
      controller: granite-lite
  embed_batch_size: 8
  embed_query_prefix: "query: "
  request_timeout: 90s
  qualified_versions: [v4.10.0]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "alixai.yaml")
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	newSection := func() (*config.Registry, *Config) {
		r := config.NewRegistry()
		c := &Config{}
		r.Register("localai", c)
		return r, c
	}
	lookup := func(env map[string]string) config.EnvLookup {
		return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	}

	r, c := newSection()
	if err := r.LoadFrom(path, lookup(map[string]string{"ALIXAI_LOCALAI_HEALTH_TIMEOUT": "2s"})); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	want := Config{
		Endpoint:          "https://localai.internal:8443",
		APIKeyRef:         "env:ALIXAI_LOCALAI_KEY",
		TLS:               TLSConfig{ServerName: "localai.internal"},
		Models:            ModelSet{Controller: "granite-controller", Embedder: "granite-embed"},
		Profiles:          map[string]ModelSet{"lite": {Controller: "granite-lite"}},
		EmbedBatchSize:    8,
		EmbedQueryPrefix:  "query: ",
		RequestTimeout:    90 * time.Second,
		HealthTimeout:     2 * time.Second,
		QualifiedVersions: []string{"v4.10.0"},
	}
	if diff := cmp.Diff(want, *c); diff != "" {
		t.Errorf("loaded section (-want +got):\n%s", diff)
	}

	for name, endpoint := range map[string]string{
		"plaintext remote": "http://10.1.2.3:8080",
		"all interfaces":   "http://0.0.0.0:8080",
		"remote hostname":  "http://localai.internal:8080",
	} {
		bad := strings.Replace(yamlText, "https://localai.internal:8443", endpoint, 1)
		bad = strings.Replace(bad, "  tls:\n    server_name: localai.internal\n", "", 1)
		p := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		r2, _ := newSection()
		if err := r2.LoadFrom(p, nil); err == nil {
			t.Errorf("%s: config check accepted %s", name, endpoint)
		}
	}

	t.Run("environment override cannot make the endpoint unsafe", func(t *testing.T) {
		r3, _ := newSection()
		if err := r3.LoadFrom(path, lookup(map[string]string{"ALIXAI_LOCALAI_ENDPOINT": "http://203.0.113.9:8080"})); err == nil {
			t.Error("an environment override to a plaintext remote endpoint was accepted")
		}
	})
	t.Run("the live-test variables are not configuration", func(t *testing.T) {
		r4, _ := newSection()
		env := map[string]string{"ALIXAI_LOCALAI_URL": "http://203.0.113.9:8080", "ALIXAI_LOCALAI_KEY": "x"}
		if err := r4.LoadFrom(path, lookup(env)); err != nil {
			t.Errorf("ALIXAI_LOCALAI_URL / ALIXAI_LOCALAI_KEY must not collide with a config field: %v", err)
		}
	})
}

func TestWithDefaultsCopiesMutableFields(t *testing.T) {
	c := testConfig("")
	c.QualifiedVersions = []string{"v4.10.0"}
	d := c.withDefaults()
	c.Profiles["lite"] = ModelSet{Controller: "changed"}
	c.QualifiedVersions[0] = "changed"
	if d.Profiles["lite"].Controller != "ctl-lite" || d.QualifiedVersions[0] != "v4.10.0" {
		t.Error("the client's configuration shares maps or slices with the caller's")
	}
	if d.Endpoint != DefaultEndpoint || d.EmbedBatchSize != DefaultEmbedBatchSize || d.RequestTimeout != DefaultRequestTimeout ||
		d.HealthTimeout != DefaultHealthTimeout || d.MaxResponseBytes != DefaultMaxResponseBytes || d.DefaultMaxOutputToks != DefaultMaxOutputTokens {
		t.Errorf("defaults not applied: %+v", d)
	}
}
