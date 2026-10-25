package supervisor

import (
	"strings"
	"testing"
	"time"
)

func TestBackoffDelay(t *testing.T) {
	b := Backoff{Initial: time.Second, Max: 30 * time.Second, Factor: 2, Jitter: 0.2}
	tests := []struct {
		n    int
		r    float64
		want time.Duration
	}{
		{1, 0.5, time.Second},
		{2, 0.5, 2 * time.Second},
		{3, 0.5, 4 * time.Second},
		{5, 0.5, 16 * time.Second},
		{6, 0.5, 30 * time.Second}, // 32s capped
		{1000, 0.5, 30 * time.Second},
		{0, 0.5, time.Second}, // clamped to the first failure
		{1, 0, 800 * time.Millisecond},
		{1, 0.999999, 1199999 * time.Microsecond},
		{6, 0.999999, 30 * time.Second}, // jitter never exceeds the cap
	}
	for _, tt := range tests {
		got := b.Delay(tt.n, tt.r)
		// float rounding: allow a microsecond
		if d := got - tt.want; d < -time.Microsecond || d > time.Microsecond {
			t.Errorf("Delay(%d, %v) = %s, want %s", tt.n, tt.r, got, tt.want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the defaults are invalid: %v", err)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("the zero value must mean the defaults: %v", err)
	}
	tests := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"negative restarts", func(c *Config) { c.MaxRestarts = -1 }, "max_restarts"},
		{"unbounded restarts", func(c *Config) { c.MaxRestarts = 100000 }, "max_restarts"},
		{"negative unhealthy_after", func(c *Config) { c.UnhealthyAfter = -2 }, "unhealthy_after"},
		{"shrinking factor", func(c *Config) { c.Backoff.Factor = 0.5 }, "factor"},
		{"jitter of one", func(c *Config) { c.Backoff.Jitter = 1 }, "jitter"},
		{"negative jitter", func(c *Config) { c.Backoff.Jitter = -0.1 }, "jitter"},
		{"negative window", func(c *Config) { c.RestartWindow = -time.Minute }, "restart_window"},
		{"week-long start timeout", func(c *Config) { c.StartTimeout = 7 * 24 * time.Hour }, "start_timeout"},
		{"initial above max", func(c *Config) { c.Backoff.Initial = time.Minute; c.Backoff.Max = time.Second }, "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := DefaultConfig()
			tt.mut(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate = %v, want an error mentioning %q", err, tt.want)
			}
			if _, nerr := New(c, &Unit{}, &scriptProber{seq: nil}); nerr == nil {
				t.Error("New accepted an invalid config")
			}
		})
	}
	if _, err := New(DefaultConfig(), nil, &scriptProber{}); err == nil {
		t.Error("New accepted a nil runtime")
	}
	if _, err := New(DefaultConfig(), &Unit{}, nil); err == nil {
		t.Error("New accepted a nil probe")
	}
}
