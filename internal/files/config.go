package files

import (
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// Defaults and ceilings. The ceilings keep a typo from turning a limit into no
// limit at all.
const (
	// DefaultMaxFileBytes is the largest file a rule can accept. The gateway's
	// own gateway.max_body_bytes (64 KiB by default, 16 MiB at most) is applied
	// before any route runs, so over HTTP the smaller of the two decides.
	DefaultMaxFileBytes = 10 << 20
	// DefaultQuotaBytes and DefaultQuotaFiles cap the live files of one tenant.
	DefaultQuotaBytes = 1 << 30
	DefaultQuotaFiles = 5000
	// DefaultMaxTTL is the longest a reference may live.
	DefaultMaxTTL = 365 * 24 * time.Hour
	// DefaultCompactEvery is how often the background compaction runs.
	DefaultCompactEvery = 10 * time.Minute
	// DefaultCompactGrace is how long a tombstone is kept before its blob is
	// removed, so a reader that already holds the file finishes.
	DefaultCompactGrace = time.Minute
	// DefaultModelTextBytes bounds the text of one attachment given to a model.
	DefaultModelTextBytes = 32 << 10
	// DefaultModelImageBytes bounds the image bytes of one attachment given to a
	// vision-capable provider.
	DefaultModelImageBytes = 4 << 20
	// DefaultPreviewTextBytes bounds a text preview.
	DefaultPreviewTextBytes = 64 << 10

	// The reference lifetime of each treatment when its rule names none.
	DefaultEphemeralTTL = time.Hour
	DefaultRAGSourceTTL = 24 * time.Hour
	DefaultObjectTTL    = 30 * 24 * time.Hour

	maxFileBytesCap  = 1 << 30
	maxQuotaBytesCap = 1 << 40
	maxQuotaFilesCap = 10_000_000
	maxDurationCap   = 10 * 365 * 24 * time.Hour
	maxTextBytesCap  = 1 << 20
	maxImageBytesCap = 64 << 20
)

// SectionName is the configuration section this package owns:
//
//	files:
//	  max_file_bytes: 10485760     # per file; over HTTP gateway.max_body_bytes also applies
//	  quota_bytes: 1073741824      # live bytes of one tenant, all its workspaces
//	  quota_files: 5000            # live files of one tenant
//	  max_ttl: 8760h               # no reference lives longer
//	  compact_every: 10m           # background compaction period
//	  compact_grace: 1m            # tombstones are kept this long before the blob goes
//	  model_text_bytes: 32768      # text of one attachment handed to a model
//	  model_image_bytes: 4194304   # image bytes handed to a vision provider
//	  preview_text_bytes: 65536    # text preview size
//	  ephemeral_context:           # the host's attachment policy, one rule per treatment;
//	    enabled: true              # a treatment with no enabled rule is refused
//	    allowed_mime: [image/png, image/jpeg, text/plain, ...]
//	    max_bytes: 0               # 0: max_file_bytes
//	    ttl: 1h
//	    permissions: [read, preview]
//	    visibility: owner          # owner | workspace
//	  rag_source: {...}            # off until the host enables it (and a RAG ingestion is wired)
//	  application_object: {...}    # off until the host enables it
const SectionName = "files"

// RuleConfig is the host's declared policy for one treatment (master plan 14.1:
// an attachment is ephemeral turn context, an explicitly registered RAG source
// or an application object "according to the host's declared policy").
type RuleConfig struct {
	// Enabled switches the treatment on. Off, an upload that asks for it (or
	// that the policy would default to it) is refused.
	Enabled bool `yaml:"enabled"`
	// AllowedMIME lists the media types the treatment accepts: exact types and the
	// families image/* and text/*. Only types the core can verify from their bytes
	// can be listed (see KnownTypes). An enabled rule must list at least one.
	AllowedMIME []string `yaml:"allowed_mime"`
	// MaxBytes caps one file of the treatment. Zero means max_file_bytes.
	MaxBytes int64 `yaml:"max_bytes"`
	// TTL is how long a reference of the treatment resolves. Zero means the
	// treatment's default.
	TTL time.Duration `yaml:"ttl"`
	// Permissions are what the principal may do with a file of the treatment: read
	// (hand it to a model or the RAG pipeline), preview and download. Empty means
	// the treatment's default.
	Permissions []string `yaml:"permissions"`
	// Visibility is "owner" (only the uploading principal) or "workspace" (every
	// principal of the workspace). Empty means owner.
	Visibility string `yaml:"visibility"`
}

// Config is the "files" section. A zero field takes its default.
type Config struct {
	MaxFileBytes     int64         `yaml:"max_file_bytes"`
	QuotaBytes       int64         `yaml:"quota_bytes"`
	QuotaFiles       int           `yaml:"quota_files"`
	MaxTTL           time.Duration `yaml:"max_ttl"`
	CompactEvery     time.Duration `yaml:"compact_every"`
	CompactGrace     time.Duration `yaml:"compact_grace"`
	ModelTextBytes   int           `yaml:"model_text_bytes"`
	ModelImageBytes  int64         `yaml:"model_image_bytes"`
	PreviewTextBytes int           `yaml:"preview_text_bytes"`

	Ephemeral RuleConfig `yaml:"ephemeral_context"`
	RAGSource RuleConfig `yaml:"rag_source"`
	Object    RuleConfig `yaml:"application_object"`
}

// DefaultConfig returns the defaults: ephemeral turn context only, for the
// image, text and PDF types a chat attachment usually is, readable and
// previewable by its uploader for an hour. A RAG source and an application
// object are the host's explicit choice, so they are off.
func DefaultConfig() Config {
	return Config{
		MaxFileBytes:     DefaultMaxFileBytes,
		QuotaBytes:       DefaultQuotaBytes,
		QuotaFiles:       DefaultQuotaFiles,
		MaxTTL:           DefaultMaxTTL,
		CompactEvery:     DefaultCompactEvery,
		CompactGrace:     DefaultCompactGrace,
		ModelTextBytes:   DefaultModelTextBytes,
		ModelImageBytes:  DefaultModelImageBytes,
		PreviewTextBytes: DefaultPreviewTextBytes,
		Ephemeral: RuleConfig{
			Enabled: true,
			AllowedMIME: []string{
				"image/png", "image/jpeg", "image/gif", "image/webp",
				"application/pdf", "text/plain", "text/markdown", "text/csv", "application/json",
			},
			TTL:         DefaultEphemeralTTL,
			Permissions: []string{"read", "preview"},
			Visibility:  string(VisibilityOwner),
		},
		RAGSource: RuleConfig{
			AllowedMIME: []string{"application/pdf", "text/plain", "text/markdown", "text/csv"},
			TTL:         DefaultRAGSourceTTL,
			Permissions: []string{"read"},
			Visibility:  string(VisibilityOwner),
		},
		Object: RuleConfig{
			AllowedMIME: []string{
				"image/png", "image/jpeg", "image/gif", "image/webp",
				"application/pdf", "text/plain", "text/markdown", "text/csv", "application/json",
			},
			TTL:         DefaultObjectTTL,
			Permissions: []string{"read", "preview", "download"},
			Visibility:  string(VisibilityOwner),
		},
	}
}

// withDefaults replaces zero numeric fields with the defaults. The rules are
// not touched: an absent rule is a disabled one, and a rule fills its own
// zero fields when it is resolved (Rule resolution in policy.go).
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MaxFileBytes == 0 {
		c.MaxFileBytes = d.MaxFileBytes
	}
	if c.QuotaBytes == 0 {
		c.QuotaBytes = d.QuotaBytes
	}
	if c.QuotaFiles == 0 {
		c.QuotaFiles = d.QuotaFiles
	}
	if c.MaxTTL == 0 {
		c.MaxTTL = d.MaxTTL
	}
	if c.CompactEvery == 0 {
		c.CompactEvery = d.CompactEvery
	}
	if c.CompactGrace == 0 {
		c.CompactGrace = d.CompactGrace
	}
	if c.ModelTextBytes == 0 {
		c.ModelTextBytes = d.ModelTextBytes
	}
	if c.ModelImageBytes == 0 {
		c.ModelImageBytes = d.ModelImageBytes
	}
	if c.PreviewTextBytes == 0 {
		c.PreviewTextBytes = d.PreviewTextBytes
	}
	return c
}

// Validate implements the config section validation hook.
func (c Config) Validate() error {
	for _, f := range []struct {
		name string
		v    int64
		max  int64
	}{
		{"max_file_bytes", c.MaxFileBytes, maxFileBytesCap},
		{"quota_bytes", c.QuotaBytes, maxQuotaBytesCap},
		{"quota_files", int64(c.QuotaFiles), maxQuotaFilesCap},
		{"model_text_bytes", int64(c.ModelTextBytes), maxTextBytesCap},
		{"model_image_bytes", c.ModelImageBytes, maxImageBytesCap},
		{"preview_text_bytes", int64(c.PreviewTextBytes), maxTextBytesCap},
	} {
		if f.v < 0 || f.v > f.max {
			return fmt.Errorf("%s %d must be between 0 and %d", f.name, f.v, f.max)
		}
	}
	for _, f := range []struct {
		name string
		d    time.Duration
	}{
		{"max_ttl", c.MaxTTL}, {"compact_every", c.CompactEvery}, {"compact_grace", c.CompactGrace},
	} {
		if f.d < 0 || f.d > maxDurationCap {
			return fmt.Errorf("%s %s must be between 0 and %s", f.name, f.d, maxDurationCap)
		}
	}
	for name, r := range map[string]RuleConfig{
		string(TreatmentEphemeral): c.Ephemeral, string(TreatmentRAGSource): c.RAGSource, string(TreatmentObject): c.Object,
	} {
		if err := r.validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func (r RuleConfig) validate() error {
	if r.MaxBytes < 0 || r.MaxBytes > maxFileBytesCap {
		return fmt.Errorf("max_bytes %d must be between 0 and %d", r.MaxBytes, maxFileBytesCap)
	}
	if r.TTL < 0 || r.TTL > maxDurationCap {
		return fmt.Errorf("ttl %s must be between 0 and %s", r.TTL, maxDurationCap)
	}
	for _, p := range r.Permissions {
		if !Permission(p).Valid() {
			return fmt.Errorf("permission %q is not read, preview or download", p)
		}
	}
	if r.Visibility != "" && !Visibility(r.Visibility).valid() {
		return fmt.Errorf("visibility %q is not owner or workspace", r.Visibility)
	}
	for _, m := range r.AllowedMIME {
		if !knownPattern(m) {
			return fmt.Errorf("allowed_mime %q is not a type the core can verify (see files.KnownTypes)", m)
		}
	}
	if r.Enabled && len(r.AllowedMIME) == 0 {
		return fmt.Errorf("an enabled rule must list at least one allowed_mime")
	}
	return nil
}

// section is the registered "files" section. Owners read it through this
// pointer (see config.Register).
var section = DefaultConfig()

func init() { config.Register(SectionName, &section) }

// ConfigSection returns the registered "files" section.
func ConfigSection() *Config { return &section }

// RegisterSection registers the section on reg. init does it for
// config.Default; a tool or test that builds its own registry calls it too.
func RegisterSection(reg *config.Registry) { reg.Register(SectionName, &section) }

func lowerTrim(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
