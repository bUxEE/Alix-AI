package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestDefaultConfigIsValidAndFinite(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	for _, class := range Classes() {
		cc := c.ForClass(class)
		if cc.Workers < 1 || cc.MaxDepth < 1 {
			t.Errorf("class %s has workers %d depth %d: every class needs a worker limit and a finite depth", class, cc.Workers, cc.MaxDepth)
		}
	}
	// The zero Config means "all defaults", never "unbounded".
	if got := (Config{}).withDefaults(); got != DefaultConfig() {
		t.Fatalf("zero config does not resolve to the defaults: %+v", got)
	}
	if got := (Config{}).ForClass(ClassIngest); got != DefaultConfig().Class5 {
		t.Fatalf("zero config class 5 = %+v", got)
	}
}

func TestConfigValidateRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"negative workers":          func(c *Config) { c.Class3.Workers = -1 },
		"too many workers":          func(c *Config) { c.Class5.Workers = 1000 },
		"negative depth":            func(c *Config) { c.Class2.MaxDepth = -5 },
		"poll interval too small":   func(c *Config) { c.PollInterval = time.Nanosecond },
		"poll interval too large":   func(c *Config) { c.PollInterval = time.Hour },
		"lease ttl too small":       func(c *Config) { c.LeaseTTL = time.Nanosecond },
		"zero-ish attempts":         func(c *Config) { c.MaxAttempts = -1 },
		"backoff base above max":    func(c *Config) { c.BackoffBase, c.BackoffMax = time.Hour, time.Minute },
		"payload cap absurd":        func(c *Config) { c.MaxPayloadBytes = 1 << 30 },
		"yield classes inverted":    func(c *Config) { c.InteractiveUpTo, c.BackgroundFrom = 4, 3 },
		"yield classes overlapping": func(c *Config) { c.InteractiveUpTo, c.BackgroundFrom = 3, 3 },
		"interactive class 6":       func(c *Config) { c.InteractiveUpTo = 6 },
		"policy window absurd":      func(c *Config) { c.PolicyWindow = 1 << 20 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := DefaultConfig()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", c)
			}
			// NewQueue refuses it too, with a typed error.
			h := newHarness(t)
			_, err := NewQueue(h.db, Options{Config: c})
			codeIs(t, err, contracts.ErrValidationFailed)
		})
	}
	// Turning the yield rule off is allowed.
	c := DefaultConfig()
	c.BackgroundFrom = 7
	if err := c.Validate(); err != nil {
		t.Fatalf("yield off rejected: %v", err)
	}
}

func TestNewQueueNeedsAStore(t *testing.T) {
	_, err := NewQueue(nil, Options{})
	codeIs(t, err, contracts.ErrValidationFailed)
}

func TestConfigSectionLoadsFromYAMLAndEnvironment(t *testing.T) {
	if !strings.Contains(strings.Join(config.Default.Sections(), ","), "jobs") {
		t.Fatalf("the jobs section is not registered: %v", config.Default.Sections())
	}
	// A private registry with the same shape as the real section.
	reg := config.NewRegistry()
	c := DefaultConfig()
	reg.Register("jobs", &c)
	path := filepath.Join(t.TempDir(), "alixai.yaml")
	yaml := "jobs:\n  poll_interval: 250ms\n  class5:\n    workers: 3\n    max_depth: 77\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"ALIXAI_JOBS_LEASE_TTL": "45s", "ALIXAI_JOBS_CLASS1_WORKERS": "4"}
	if err := reg.LoadFrom(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PollInterval != 250*time.Millisecond || c.Class5.Workers != 3 || c.Class5.MaxDepth != 77 || c.LeaseTTL != 45*time.Second || c.Class1.Workers != 4 {
		t.Fatalf("loaded config = %+v", c)
	}
	// An invalid value is a load error, not a silent default.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("jobs:\n  class5:\n    workers: -2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadFrom(bad, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("negative workers loaded")
	}
}

func TestClassHelpers(t *testing.T) {
	if got := Classes(); len(got) != 6 || got[0] != ClassAuthCommit || got[5] != ClassMaintenance {
		t.Fatalf("Classes() = %v", got)
	}
	for _, c := range Classes() {
		if !c.Valid() || strings.HasPrefix(c.String(), "class(") {
			t.Errorf("class %d: valid=%v name=%q", c, c.Valid(), c.String())
		}
	}
	if Class(0).Valid() || Class(7).Valid() || Class(7).String() != "class(7)" {
		t.Fatalf("out-of-range classes must be invalid")
	}
	for _, s := range []State{StateSucceeded, StateFailed, StateCancelled} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []State{StatePending, StateRunning} {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestPermanentWrapper(t *testing.T) {
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must stay nil")
	}
	err := Permanent(errUnavailable())
	if !IsPermanent(err) || IsPermanent(errUnavailable()) {
		t.Fatalf("IsPermanent misreports")
	}
	if contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("Permanent hides the error's code")
	}
}
