package localai

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Defaults applied to zero values of Config.
const (
	DefaultEndpoint         = "http://127.0.0.1:8080"
	DefaultEmbedBatchSize   = 16
	DefaultMaxOutputTokens  = 1024
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultHealthTimeout    = 5 * time.Second
	DefaultMaxResponseBytes = 16 << 20

	maxEmbedBatchSize = 256
)

// Config configures the adapter. It is a plain struct so the composition root
// can register it as the "localai" configuration section (config.Register);
// the adapter does not register itself. Validate is the section's validation
// hook, so `alixai config check` refuses an unsafe endpoint before anything
// starts (SEC-065). Zero values take the Default* constants.
type Config struct {
	// Endpoint is where LocalAI listens: http://127.0.0.1:8080 (loopback),
	// https://host:port (any host, TLS verified) or unix:///path/to.sock.
	Endpoint string `yaml:"endpoint"`
	// APIKeyRef names the LocalAI API key: "env:NAME" or "file:PATH". The key
	// itself is never configuration. Empty sends no key.
	APIKeyRef string `yaml:"api_key_ref"`
	// TLS adjusts certificate verification for an https endpoint. There is no
	// switch that disables verification.
	TLS TLSConfig `yaml:"tls"`
	// Models names the model each role uses.
	Models ModelSet `yaml:"models"`
	// Profiles maps a request's Profile hint to model names. A field left
	// empty falls back to Models. A request naming an unknown profile is
	// refused, so callers never reach a model that was not configured.
	Profiles map[string]ModelSet `yaml:"profiles"`
	// EmbedBatchSize is the most inputs sent in one embeddings request.
	EmbedBatchSize int `yaml:"embed_batch_size"`
	// EmbedQueryPrefix and EmbedDocumentPrefix are prepended to each input
	// for models that need an instruction prefix (providers.EmbedPurpose).
	EmbedQueryPrefix    string `yaml:"embed_query_prefix"`
	EmbedDocumentPrefix string `yaml:"embed_document_prefix"`
	// DefaultMaxOutputToks caps a chat request that asks for no limit
	// (MaxTokens 0). The Budget decorator normally fills the field first.
	DefaultMaxOutputToks int `yaml:"default_max_output_toks"`
	// RequestTimeout bounds one call, a whole stream included, on top of any
	// shorter deadline the caller's context carries.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// HealthTimeout bounds one Health probe.
	HealthTimeout time.Duration `yaml:"health_timeout"`
	// MaxResponseBytes bounds a response body or a whole stream.
	MaxResponseBytes int64 `yaml:"max_response_bytes"`
	// QualifiedVersions lists the LocalAI versions AlixAI was qualified
	// against (for example "v4.10.0"). When set, Health reports any other
	// runtime version as unavailable (SEC-072). Empty disables the check and
	// leaves the comparison to the caller of Health.
	QualifiedVersions []string `yaml:"qualified_versions"`
}

// ModelSet names the LocalAI model used for each role.
type ModelSet struct {
	Controller string `yaml:"controller"`
	Router     string `yaml:"router"`
	Embedder   string `yaml:"embedder"`
}

// TLSConfig is the TLS client configuration for an https endpoint.
type TLSConfig struct {
	// CAFile is a PEM bundle that replaces the system roots.
	CAFile string `yaml:"ca_file"`
	// CertFile and KeyFile are a client certificate for mutual TLS.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ServerName overrides the name the server certificate must match.
	ServerName string `yaml:"server_name"`
}

func (t TLSConfig) isZero() bool { return t == TLSConfig{} }

var (
	modelNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+/-]{0,199}$`)
	profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Validate checks the configuration without touching the network or the
// secret store. It returns the first problem; messages never include the key.
func (c Config) Validate() error {
	spec, err := parseEndpoint(c.endpoint())
	if err != nil {
		return err
	}
	if !c.TLS.isZero() && spec.scheme != "https" {
		return errors.New("localai: tls settings need an https endpoint")
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return errors.New("localai: tls.cert_file and tls.key_file must be set together")
	}
	if c.APIKeyRef != "" {
		if !strings.HasPrefix(c.APIKeyRef, "env:") && !strings.HasPrefix(c.APIKeyRef, "file:") {
			return errors.New("localai: api_key_ref must start with env: or file: (a key itself is never accepted in configuration)")
		}
	}
	if err := validateModelSet("models", c.Models); err != nil {
		return err
	}
	for name, set := range c.Profiles {
		if !profileNameRE.MatchString(name) {
			return fmt.Errorf("localai: profile name %q is not valid", name)
		}
		if err := validateModelSet("profiles."+name, set); err != nil {
			return err
		}
	}
	if c.EmbedBatchSize < 0 || c.EmbedBatchSize > maxEmbedBatchSize {
		return fmt.Errorf("localai: embed_batch_size must be between 1 and %d", maxEmbedBatchSize)
	}
	if c.DefaultMaxOutputToks < 0 {
		return errors.New("localai: default_max_output_toks is negative")
	}
	if c.RequestTimeout < 0 || c.HealthTimeout < 0 {
		return errors.New("localai: timeouts must not be negative")
	}
	if c.MaxResponseBytes < 0 {
		return errors.New("localai: max_response_bytes is negative")
	}
	for _, v := range c.QualifiedVersions {
		if sanitizeVersion(v) == "" || sanitizeVersion(v) != v {
			return fmt.Errorf("localai: qualified version %q is not a valid version token", v)
		}
	}
	return nil
}

func validateModelSet(where string, s ModelSet) error {
	for role, name := range map[string]string{"controller": s.Controller, "router": s.Router, "embedder": s.Embedder} {
		if name != "" && !modelNameRE.MatchString(name) {
			return fmt.Errorf("localai: %s.%s is not a valid model name", where, role)
		}
	}
	return nil
}

func (c Config) endpoint() string {
	if strings.TrimSpace(c.Endpoint) == "" {
		return DefaultEndpoint
	}
	return strings.TrimSpace(c.Endpoint)
}

// withDefaults returns a copy with zero values replaced by the defaults and
// the maps and slices copied, so later changes by the caller cannot reach the
// client.
func (c Config) withDefaults() Config {
	c.Endpoint = c.endpoint()
	if c.EmbedBatchSize == 0 {
		c.EmbedBatchSize = DefaultEmbedBatchSize
	}
	if c.DefaultMaxOutputToks == 0 {
		c.DefaultMaxOutputToks = DefaultMaxOutputTokens
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.HealthTimeout == 0 {
		c.HealthTimeout = DefaultHealthTimeout
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}
	profiles := make(map[string]ModelSet, len(c.Profiles))
	for k, v := range c.Profiles {
		profiles[k] = v
	}
	c.Profiles = profiles
	c.QualifiedVersions = append([]string(nil), c.QualifiedVersions...)
	return c
}
