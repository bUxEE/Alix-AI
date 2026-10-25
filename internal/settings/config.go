package settings

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SectionName is the configuration section this package owns. It holds what only
// the operator decides, because a setting may select among these and never
// widen them:
//
//	settings:
//	  approved_profiles: [lite]        # model and resource profiles a setting may select
//	  default_profile: lite            # used when no level states one; must be approved
//	  approved_voices: []              # installed voices a setting may select (none until voice is installed)
//	  max_history_retention_days: 365  # ceiling for every retention a setting states
//	  default_history_retention_days: 90
//	  max_attachment_mb: 25            # ceiling for every attachment size a setting states
//
// with ALIXAI_SETTINGS_APPROVED_PROFILES (comma separated), ALIXAI_SETTINGS_DEFAULT_PROFILE
// and so on as overrides. The section is deliberately NOT a settings document: no
// request can reach it, so an application can not approve itself a profile, a voice
// or a larger ceiling.
const SectionName = "settings"

// Config is the settings section.
type Config struct {
	// ApprovedProfiles are the model and resource profiles a setting may select.
	ApprovedProfiles []string `yaml:"approved_profiles"`
	// DefaultProfile is the profile in force when no level states one. It must be
	// one of ApprovedProfiles.
	DefaultProfile string `yaml:"default_profile"`
	// ApprovedVoices are the installed voices a setting may select. Empty means
	// none: a document that names a voice is then refused.
	ApprovedVoices []string `yaml:"approved_voices"`
	// MaxHistoryRetentionDays is the ceiling for every history retention.
	MaxHistoryRetentionDays int `yaml:"max_history_retention_days"`
	// DefaultHistoryRetentionDays is the retention in force when no level states
	// one. It never exceeds MaxHistoryRetentionDays.
	DefaultHistoryRetentionDays int `yaml:"default_history_retention_days"`
	// MaxAttachmentMB is the ceiling for every attachment size, and the size in
	// force when no level states one.
	MaxAttachmentMB int `yaml:"max_attachment_mb"`
}

// Hard bounds mirrored from schemas/settings/shared.schema.json.
const (
	maxRetentionDays = 3650
	maxAttachmentMB  = 1024
	maxApprovedList  = 32
)

var (
	profileIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	voiceIDRE   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

// DefaultConfig returns the defaults: the Lite profile only, no voices, 90 days of
// history up to a year, attachments up to 25 MB.
func DefaultConfig() Config {
	return Config{
		ApprovedProfiles:            []string{"lite"},
		DefaultProfile:              "lite",
		MaxHistoryRetentionDays:     365,
		DefaultHistoryRetentionDays: 90,
		MaxAttachmentMB:             25,
	}
}

// Validate implements the configuration validation hook. A configuration that
// cannot be reconciled (a default that is not approved, a default above its
// ceiling) is an error, never silently corrected.
func (c Config) Validate() error {
	if len(c.ApprovedProfiles) == 0 || len(c.ApprovedProfiles) > maxApprovedList {
		return fmt.Errorf("settings.approved_profiles must list between 1 and %d profiles", maxApprovedList)
	}
	for _, p := range c.ApprovedProfiles {
		if !profileIDRE.MatchString(p) {
			return fmt.Errorf("settings.approved_profiles: %q is not a profile id (lower case letters, digits and hyphens, at most 32 characters)", p)
		}
	}
	if !slices.Contains(c.ApprovedProfiles, c.DefaultProfile) {
		return fmt.Errorf("settings.default_profile %q is not one of settings.approved_profiles (%s)", c.DefaultProfile, strings.Join(c.ApprovedProfiles, ", "))
	}
	if len(c.ApprovedVoices) > maxApprovedList {
		return fmt.Errorf("settings.approved_voices lists more than %d voices", maxApprovedList)
	}
	for _, v := range c.ApprovedVoices {
		if !voiceIDRE.MatchString(v) {
			return fmt.Errorf("settings.approved_voices: %q is not a voice id", v)
		}
	}
	if c.MaxHistoryRetentionDays < 1 || c.MaxHistoryRetentionDays > maxRetentionDays {
		return fmt.Errorf("settings.max_history_retention_days must be between 1 and %d", maxRetentionDays)
	}
	if c.DefaultHistoryRetentionDays < 1 || c.DefaultHistoryRetentionDays > c.MaxHistoryRetentionDays {
		return fmt.Errorf("settings.default_history_retention_days must be between 1 and settings.max_history_retention_days (%d)", c.MaxHistoryRetentionDays)
	}
	if c.MaxAttachmentMB < 1 || c.MaxAttachmentMB > maxAttachmentMB {
		return fmt.Errorf("settings.max_attachment_mb must be between 1 and %d", maxAttachmentMB)
	}
	return nil
}

// Limits is what a Service enforces, taken from the validated configuration.
// A Limits is a value: the service holds its own copy and a later change to the
// registered section does not move a running service.
type Limits struct {
	ApprovedProfiles            []string
	DefaultProfile              string
	ApprovedVoices              []string
	MaxHistoryRetentionDays     int
	DefaultHistoryRetentionDays int
	MaxAttachmentMB             int
}

// Limits returns the limits of the section. It does not validate; Service does.
func (c Config) Limits() Limits {
	return Limits{
		ApprovedProfiles:            slices.Clone(c.ApprovedProfiles),
		DefaultProfile:              c.DefaultProfile,
		ApprovedVoices:              slices.Clone(c.ApprovedVoices),
		MaxHistoryRetentionDays:     c.MaxHistoryRetentionDays,
		DefaultHistoryRetentionDays: c.DefaultHistoryRetentionDays,
		MaxAttachmentMB:             c.MaxAttachmentMB,
	}
}

// DefaultLimits are the limits of DefaultConfig.
func DefaultLimits() Limits { return DefaultConfig().Limits() }

// validate checks the limits the way Config.Validate checks the section, as a
// typed error. A service is never built over limits that contradict themselves.
func (l Limits) validate() error {
	if err := (Config{
		ApprovedProfiles: l.ApprovedProfiles, DefaultProfile: l.DefaultProfile, ApprovedVoices: l.ApprovedVoices,
		MaxHistoryRetentionDays: l.MaxHistoryRetentionDays, DefaultHistoryRetentionDays: l.DefaultHistoryRetentionDays,
		MaxAttachmentMB: l.MaxAttachmentMB,
	}).Validate(); err != nil {
		return contracts.New(contracts.ErrValidationFailed, "settings: "+err.Error())
	}
	return nil
}

// registered is the section registered with the configuration loader. Owners
// read it through this pointer (config.Load replaces the fields wholesale).
var registered = func() *Config { c := DefaultConfig(); return &c }()

func init() { config.Register(SectionName, registered) }

// CurrentConfig returns the settings section as of the last successful
// config.Load (the defaults before any).
func CurrentConfig() Config {
	c := *registered
	c.ApprovedProfiles = slices.Clone(c.ApprovedProfiles)
	c.ApprovedVoices = slices.Clone(c.ApprovedVoices)
	return c
}

// ConfigSection returns the registered section itself: what a registry other than
// config.Default (a test, a tool, a second process wiring) registers, so a
// settings block in its file is read into the section CurrentConfig reads.
func ConfigSection() *Config { return registered }

// RegisterSection registers the settings section on reg. The process's own
// registry, config.Default, has it from init().
func RegisterSection(reg *config.Registry) { reg.Register(SectionName, registered) }
