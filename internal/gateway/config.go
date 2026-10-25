package gateway

import (
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// Defaults and ceilings. The ceilings keep a typo from turning a limit into no
// limit at all.
const (
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	// DefaultWriteTimeout exceeds the 60 s model budget of the spike
	// endpoint, so a slow generation ends in an envelope, not a reset.
	DefaultWriteTimeout    = 2 * time.Minute
	DefaultIdleTimeout     = 60 * time.Second
	DefaultShutdownTimeout = 10 * time.Second
	DefaultHealthTTL       = 5 * time.Second
	DefaultMaxBodyBytes    = 64 << 10
	DefaultMaxHeaderBytes  = 16 << 10

	maxTimeout      = time.Hour
	maxMaxBodyBytes = 16 << 20
	maxMaxHeader    = 1 << 20
)

// SectionName and DevSectionName are the configuration sections this package
// owns:
//
//	gateway:
//	  read_header_timeout: 5s
//	  read_timeout: 30s
//	  write_timeout: 2m
//	  idle_timeout: 60s
//	  shutdown_timeout: 10s
//	  health_ttl: 5s            # how long /readyz reuses a provider probe
//	  max_body_bytes: 65536
//	  max_header_bytes: 16384
//	  tls:
//	    cert_file: ""           # both, for a non-loopback listener
//	    key_file: ""
//	    client_ca_file: ""      # optional: require client certificates
//	dev:
//	  spike_endpoint: false     # POST /v0/dev/propose, unauthenticated
//
// The listen address is the core server.listen section.
const (
	SectionName    = "gateway"
	DevSectionName = "dev"
)

// Config is the "gateway" section. A zero field takes its default.
type Config struct {
	// ReadHeaderTimeout bounds reading the request line and headers
	// (slow-header clients).
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	// ReadTimeout bounds reading the whole request, body included.
	ReadTimeout time.Duration `yaml:"read_timeout"`
	// WriteTimeout bounds one handler plus its response. It must stay above
	// the longest model call a handler makes.
	WriteTimeout time.Duration `yaml:"write_timeout"`
	// IdleTimeout closes idle keep-alive connections.
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	// ShutdownTimeout is how long in-flight requests get after the process
	// is told to stop.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// HealthTTL is how long /readyz reuses the last provider probe, so an
	// unauthenticated caller cannot turn it into load on the runtime.
	HealthTTL time.Duration `yaml:"health_ttl"`
	// MaxBodyBytes caps a request body (SEC-076).
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// MaxHeaderBytes caps the request line and headers.
	MaxHeaderBytes int `yaml:"max_header_bytes"`
	// TLS is the server identity, required for any non-loopback listener.
	TLS TLSConfig `yaml:"tls"`
}

// TLSConfig is the listener's TLS identity.
type TLSConfig struct {
	// CertFile and KeyFile are the server certificate chain and key (PEM).
	// Both or neither.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ClientCAFile is a PEM bundle; when set every client must present a
	// certificate it signed (mutual TLS).
	ClientCAFile string `yaml:"client_ca_file"`
}

// Enabled reports whether a server certificate is configured.
func (t TLSConfig) Enabled() bool { return t.CertFile != "" && t.KeyFile != "" }

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		ReadTimeout:       DefaultReadTimeout,
		WriteTimeout:      DefaultWriteTimeout,
		IdleTimeout:       DefaultIdleTimeout,
		ShutdownTimeout:   DefaultShutdownTimeout,
		HealthTTL:         DefaultHealthTTL,
		MaxBodyBytes:      DefaultMaxBodyBytes,
		MaxHeaderBytes:    DefaultMaxHeaderBytes,
	}
}

// Validate implements the config section validation hook.
func (c Config) Validate() error {
	for _, f := range []struct {
		name string
		d    time.Duration
	}{
		{"read_header_timeout", c.ReadHeaderTimeout}, {"read_timeout", c.ReadTimeout},
		{"write_timeout", c.WriteTimeout}, {"idle_timeout", c.IdleTimeout},
		{"shutdown_timeout", c.ShutdownTimeout}, {"health_ttl", c.HealthTTL},
	} {
		if f.d < 0 || f.d > maxTimeout {
			return fmt.Errorf("%s %s must be between 0 and %s", f.name, f.d, maxTimeout)
		}
	}
	if c.MaxBodyBytes < 0 || c.MaxBodyBytes > maxMaxBodyBytes {
		return fmt.Errorf("max_body_bytes %d must be between 0 and %d", c.MaxBodyBytes, maxMaxBodyBytes)
	}
	if c.MaxHeaderBytes < 0 || c.MaxHeaderBytes > maxMaxHeader {
		return fmt.Errorf("max_header_bytes %d must be between 0 and %d", c.MaxHeaderBytes, maxMaxHeader)
	}
	return c.TLS.Validate()
}

// Validate checks that the TLS files are named consistently. It does not read
// them; NewServer does.
func (t TLSConfig) Validate() error {
	for _, f := range []struct{ name, v string }{
		{"cert_file", t.CertFile}, {"key_file", t.KeyFile}, {"client_ca_file", t.ClientCAFile},
	} {
		if f.v != "" && (strings.TrimSpace(f.v) == "" || strings.ContainsRune(f.v, 0)) {
			return fmt.Errorf("tls.%s is not a usable path", f.name)
		}
	}
	if (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("tls.cert_file and tls.key_file must be set together")
	}
	if t.ClientCAFile != "" && !t.Enabled() {
		return fmt.Errorf("tls.client_ca_file needs tls.cert_file and tls.key_file")
	}
	return nil
}

// withDefaults replaces zero fields with the defaults.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = d.ReadHeaderTimeout
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = d.ReadTimeout
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = d.WriteTimeout
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = d.IdleTimeout
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = d.ShutdownTimeout
	}
	if c.HealthTTL == 0 {
		c.HealthTTL = d.HealthTTL
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = d.MaxBodyBytes
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = d.MaxHeaderBytes
	}
	return c
}

// DevConfig is the "dev" section: switches that exist only for the build-out
// milestones and are off by default.
type DevConfig struct {
	// SpikeEndpoint mounts POST /v0/dev/propose. It is unauthenticated, so it
	// is refused on any listener that is not loopback or a unix socket. It is
	// deleted or replaced by the /v1 turn API in M1-16.
	SpikeEndpoint bool `yaml:"spike_endpoint"`
}

// DefaultDevConfig returns the defaults: everything off.
func DefaultDevConfig() DevConfig { return DevConfig{} }

// Registered sections. Owners read them through these pointers because
// config.Load replaces the fields wholesale.
var (
	gatewaySection = DefaultConfig()
	devSection     = DefaultDevConfig()
)

// ConfigSection returns the registered "gateway" section.
func ConfigSection() *Config { return &gatewaySection }

// DevSection returns the registered "dev" section.
func DevSection() *DevConfig { return &devSection }

// RegisterSections registers the package's sections on reg. init does it for
// config.Default; tests and tools that build their own registry call it too.
func RegisterSections(reg *config.Registry) {
	reg.Register(SectionName, &gatewaySection)
	reg.Register(DevSectionName, &devSection)
}

func init() { RegisterSections(config.Default) }
