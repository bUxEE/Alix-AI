package quotas

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// Dimension is one thing a tenant's consumption is limited in.
type Dimension string

// The four dimensions of master plan 25 item 9 that ADR 0018 finalises.
const (
	// DimRequestsPerMinute is a token bucket per tenant (refill per minute plus a burst).
	DimRequestsPerMinute Dimension = "requests_per_minute"
	// DimConcurrentSessions is a counting semaphore with expiring leases.
	DimConcurrentSessions Dimension = "concurrent_sessions"
	// DimIngestBytesPerDay is a persisted counter per UTC day.
	DimIngestBytesPerDay Dimension = "ingest_bytes_per_day"
	// DimStorageBytes is a persisted gauge: what the tenant holds now.
	DimStorageBytes Dimension = "storage_bytes"
)

// Dimensions returns the four dimensions in a stable order.
func Dimensions() []Dimension {
	return []Dimension{DimRequestsPerMinute, DimConcurrentSessions, DimIngestBytesPerDay, DimStorageBytes}
}

func (d Dimension) valid() bool {
	for _, v := range Dimensions() {
		if d == v {
			return true
		}
	}
	return false
}

// persisted reports whether the dimension lives in SQLite (the others live in
// memory and start empty at every start).
func (d Dimension) persisted() bool { return d == DimIngestBytesPerDay || d == DimStorageBytes }

// Unlimited is the one explicit way to say "no limit". A zero field never
// means unlimited: it means "inherit", and the chain ends in a finite default.
const Unlimited int64 = -1

// Bounds. A limit above its cap is a configuration error, not a clamp; the
// caps keep every product in the bucket's integer arithmetic inside int64.
const (
	maxRate     = 10_000_000
	maxBurst    = 10_000_000
	maxSessions = 1_000_000
	maxBytes    = 1 << 60

	DefaultLeaseTTL       = time.Hour
	DefaultIdleEvictAfter = 10 * time.Minute
	maxLeaseTTL           = 7 * 24 * time.Hour
	maxIdleEvict          = 7 * 24 * time.Hour
	// MaxAmount bounds one Admit so a caller-supplied size cannot overflow a counter.
	MaxAmount = maxBytes
)

// Limits are the limits of one tenant. For every field: 0 inherits the next
// level up (tenant override, application override, Config.Defaults, profile
// default), Unlimited (-1) removes the limit, and a positive number is the
// limit. Any other value is refused by Validate.
type Limits struct {
	// RequestsPerMinute is the sustained request rate (the bucket's refill).
	RequestsPerMinute int64 `yaml:"requests_per_minute"`
	// Burst is the bucket's capacity. Zero derives it: a tenth of a minute of
	// traffic, at least 1. Over any minute a tenant can spend at most
	// Burst + RequestsPerMinute requests.
	Burst int64 `yaml:"burst"`
	// ConcurrentSessions is how many sessions the tenant may hold open.
	ConcurrentSessions int64 `yaml:"concurrent_sessions"`
	// IngestBytesPerDay is the bytes the tenant may ingest per UTC day.
	IngestBytesPerDay int64 `yaml:"ingest_bytes_per_day"`
	// StorageBytes is the bytes the tenant may hold.
	StorageBytes int64 `yaml:"storage_bytes"`
}

// of returns the limit of one dimension.
func (l Limits) of(d Dimension) int64 {
	switch d {
	case DimRequestsPerMinute:
		return l.RequestsPerMinute
	case DimConcurrentSessions:
		return l.ConcurrentSessions
	case DimIngestBytesPerDay:
		return l.IngestBytesPerDay
	case DimStorageBytes:
		return l.StorageBytes
	}
	return 0
}

// over returns l with every zero field taken from base.
func (l Limits) over(base Limits) Limits {
	pick := func(v, b int64) int64 {
		if v == 0 {
			return b
		}
		return v
	}
	return Limits{
		RequestsPerMinute:  pick(l.RequestsPerMinute, base.RequestsPerMinute),
		Burst:              pick(l.Burst, base.Burst),
		ConcurrentSessions: pick(l.ConcurrentSessions, base.ConcurrentSessions),
		IngestBytesPerDay:  pick(l.IngestBytesPerDay, base.IngestBytesPerDay),
		StorageBytes:       pick(l.StorageBytes, base.StorageBytes),
	}
}

// burst is the bucket capacity once the limits are resolved.
func (l Limits) burst() int64 {
	if l.Burst > 0 {
		return l.Burst
	}
	return max(1, l.RequestsPerMinute/10)
}

func (l Limits) validate(where string) error {
	check := func(name string, v, ceiling int64, allowUnlimited bool) error {
		switch {
		case v == 0:
			return nil
		case v == Unlimited && allowUnlimited:
			return nil
		case v < 0:
			return fmt.Errorf("quotas: %s %s is %d: use a positive number, or -1 for unlimited (0 inherits)", where, name, v)
		case v > ceiling:
			return fmt.Errorf("quotas: %s %s is %d, above the ceiling %d", where, name, v, ceiling)
		}
		return nil
	}
	for _, c := range []struct {
		name      string
		v, ceil   int64
		unlimited bool
	}{
		{"requests_per_minute", l.RequestsPerMinute, maxRate, true},
		{"burst", l.Burst, maxBurst, false},
		{"concurrent_sessions", l.ConcurrentSessions, maxSessions, true},
		{"ingest_bytes_per_day", l.IngestBytesPerDay, maxBytes, true},
		{"storage_bytes", l.StorageBytes, maxBytes, true},
	} {
		if err := check(c.name, c.v, c.ceil, c.unlimited); err != nil {
			return err
		}
	}
	return nil
}

// Profile names, the same as the resource budgets (docs/operations/resource-budgets.md).
const (
	ProfileExperimental4G = "experimental-4g"
	ProfileLite           = "lite"
	ProfileBalanced       = "balanced"
	ProfilePerformance    = "performance"
)

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// profileLimits are the finite defaults per profile. Derivation and the
// arithmetic are in ADR 0018: a request budget well under what one worker
// serves, session counts a fraction of the resident controller's queue (one
// interactive slot on the small profiles), and daily and storage volumes
// sized against the disk rows of the resource budgets.
var profileLimits = map[string]Limits{
	ProfileExperimental4G: {RequestsPerMinute: 60, ConcurrentSessions: 4, IngestBytesPerDay: 256 * mib, StorageBytes: 2 * gib},
	ProfileLite:           {RequestsPerMinute: 120, ConcurrentSessions: 8, IngestBytesPerDay: 1 * gib, StorageBytes: 8 * gib},
	ProfileBalanced:       {RequestsPerMinute: 300, ConcurrentSessions: 32, IngestBytesPerDay: 2 * gib, StorageBytes: 16 * gib},
	ProfilePerformance:    {RequestsPerMinute: 600, ConcurrentSessions: 64, IngestBytesPerDay: 8 * gib, StorageBytes: 64 * gib},
}

// ProfileLimits returns the finite defaults of a profile, and false for an
// unknown profile name.
func ProfileLimits(profile string) (Limits, bool) {
	l, ok := profileLimits[profile]
	return l, ok
}

// Override sets limits for one application (Tenant empty) or for one tenant of
// it. A tenant override inherits from its application's, then from Defaults.
type Override struct {
	ApplicationID string `yaml:"application"`
	TenantID      string `yaml:"tenant"`
	Limits        Limits `yaml:"limits"`
}

// Config is the "quotas" configuration section. A missing or zero value is
// never unlimited: every field inherits down to the profile's finite defaults.
type Config struct {
	// Profile names the finite defaults: experimental-4g, lite (the default),
	// balanced or performance.
	Profile string `yaml:"profile"`
	// Defaults replace the profile's values field by field.
	Defaults Limits `yaml:"defaults"`
	// Overrides are per application and per tenant.
	Overrides []Override `yaml:"overrides"`
	// LeaseTTL is how long a session slot is held without Renew or Release: a
	// holder that forgot to release cannot leak a slot for ever.
	LeaseTTL time.Duration `yaml:"lease_ttl"`
	// IdleEvictAfter is how long an idle tenant's in-memory state stays.
	IdleEvictAfter time.Duration `yaml:"idle_evict_after"`
}

// DefaultConfig returns the Lite profile with no overrides.
func DefaultConfig() Config {
	return Config{Profile: ProfileLite, LeaseTTL: DefaultLeaseTTL, IdleEvictAfter: DefaultIdleEvictAfter}
}

// WithDefaults replaces zero fields: an empty profile is lite, zero durations
// are the defaults.
func (c Config) WithDefaults() Config {
	if c.Profile == "" {
		c.Profile = ProfileLite
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = DefaultLeaseTTL
	}
	if c.IdleEvictAfter == 0 {
		c.IdleEvictAfter = DefaultIdleEvictAfter
	}
	return c
}

// Validate refuses what cannot be run: an unknown profile, a malformed limit,
// a blank or wildcard identifier, a repeated override. It implements the
// config package's section validation.
func (c Config) Validate() error {
	if c.Profile != "" {
		if _, ok := profileLimits[c.Profile]; !ok {
			names := make([]string, 0, len(profileLimits))
			for n := range profileLimits {
				names = append(names, n)
			}
			sort.Strings(names)
			return fmt.Errorf("quotas: unknown profile %q (want one of %s)", c.Profile, strings.Join(names, ", "))
		}
	}
	if err := c.Defaults.validate("defaults"); err != nil {
		return err
	}
	if c.LeaseTTL < 0 || c.LeaseTTL > maxLeaseTTL {
		return fmt.Errorf("quotas: lease_ttl %s is outside 0..%s", c.LeaseTTL, maxLeaseTTL)
	}
	if c.IdleEvictAfter < 0 || c.IdleEvictAfter > maxIdleEvict {
		return fmt.Errorf("quotas: idle_evict_after %s is outside 0..%s", c.IdleEvictAfter, maxIdleEvict)
	}
	seen := map[[2]string]bool{}
	for i, o := range c.Overrides {
		where := fmt.Sprintf("overrides[%d]", i)
		if !plainID(o.ApplicationID) {
			return fmt.Errorf("quotas: %s needs an application id without wildcards or blanks", where)
		}
		if o.TenantID != "" && !plainID(o.TenantID) {
			return fmt.Errorf("quotas: %s tenant %q has a wildcard or blank", where, o.TenantID)
		}
		k := [2]string{o.ApplicationID, o.TenantID}
		if seen[k] {
			return fmt.Errorf("quotas: %s repeats an earlier override for the same application and tenant", where)
		}
		seen[k] = true
		if err := o.Limits.validate(where); err != nil {
			return err
		}
	}
	return nil
}

// plainID is an identifier a scope could carry: non-blank, trimmed, no wildcard.
func plainID(s string) bool {
	return s != "" && s == strings.TrimSpace(s) && !strings.ContainsAny(s, "*?")
}

// section is the registered "quotas" configuration section; owners read it
// through this pointer (see config.Register).
var section = DefaultConfig()

func init() { config.Register("quotas", &section) }

// FromConfig returns the loaded "quotas" section (call after config.Load).
func FromConfig() Config { return section }
