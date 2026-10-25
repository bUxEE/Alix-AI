package quotas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var (
	bg = context.Background()
	// noon on a UTC day, so a test can move to just before and just after midnight.
	t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
)

// testLimits are small, so a test exhausts a dimension in a few calls.
func testLimits() Limits {
	return Limits{RequestsPerMinute: 60, Burst: 5, ConcurrentSessions: 2, IngestBytesPerDay: 100, StorageBytes: 1000}
}

func newQ(t testing.TB, mutate ...func(*Config)) (*Quotas, *clock.Fake) {
	t.Helper()
	q, clk, _ := newQDB(t, mutate...)
	return q, clk
}

func newQDB(t testing.TB, mutate ...func(*Config)) (*Quotas, *clock.Fake, *store.DB) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Defaults = testLimits()
	for _, m := range mutate {
		m(&cfg)
	}
	clk := clock.NewFake(t0)
	db := storetest.Open(t, nil)
	q, err := New(db, Options{Config: cfg, Clock: clk})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return q, clk, db
}

func scopeOf(app, tenant string) contracts.Scope { return storetest.Scope(app, tenant, "w1") }

func code(t testing.TB, err error) contracts.ErrCode {
	t.Helper()
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *contracts.Error", err)
	}
	return ce.Code
}

func detail(t testing.TB, err error, key string) any {
	t.Helper()
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *contracts.Error", err)
	}
	v, ok := ce.Details[key]
	if !ok {
		t.Fatalf("error %v has no %q detail (has %v)", err, key, ce.Details)
	}
	return v
}

// exhaust spends every unit of dim for the scope and returns the leases it holds, so a
// concurrency dimension stays exhausted while the test looks at it.
func exhaust(t testing.TB, q *Quotas, scope contracts.Scope, dim Dimension) []*Lease {
	t.Helper()
	l := testLimits()
	var leases []*Lease
	switch dim {
	case DimRequestsPerMinute:
		for i := int64(0); i < l.Burst; i++ {
			mustAdmit(t, q, scope, dim, 1)
		}
	case DimConcurrentSessions:
		for i := int64(0); i < l.ConcurrentSessions; i++ {
			leases = append(leases, mustAdmit(t, q, scope, dim, 1))
		}
	case DimIngestBytesPerDay:
		mustAdmit(t, q, scope, dim, l.IngestBytesPerDay)
	case DimStorageBytes:
		mustAdmit(t, q, scope, dim, l.StorageBytes)
	}
	return leases
}

func mustAdmit(t testing.TB, q *Quotas, scope contracts.Scope, dim Dimension, n int64) *Lease {
	t.Helper()
	l, err := q.Admit(bg, scope, dim, n)
	if err != nil {
		t.Fatalf("Admit(%s, %d) = %v, want admitted", dim, n, err)
	}
	if l == nil {
		t.Fatalf("Admit(%s, %d) admitted with a nil lease", dim, n)
	}
	return l
}

func usageOf(t testing.TB, q *Quotas, scope contracts.Scope, dim Dimension) Usage {
	t.Helper()
	us, err := q.Usage(bg, scope)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	for _, u := range us {
		if u.Dimension == dim {
			return u
		}
	}
	t.Fatalf("Usage has no %s", dim)
	return Usage{}
}

// SEC-ISO tenant isolation of the quota keys: one tenant's exhaustion never reaches another's.
func TestTenantQuotaEnforced(t *testing.T) {
	for _, dim := range Dimensions() {
		t.Run(string(dim)+" is per application and tenant", func(t *testing.T) {
			q, _ := newQ(t)
			a := scopeOf("app", "tenant-a")
			held := exhaust(t, q, a, dim)

			if l, err := q.Admit(bg, a, dim, 1); err == nil || l != nil {
				t.Fatalf("an exhausted tenant was admitted again: lease %v err %v", l, err)
			}
			// Another tenant of the same application has its own quota.
			mustAdmit(t, q, scopeOf("app", "tenant-b"), dim, 1)
			// The same tenant id under another application is another tenant.
			mustAdmit(t, q, scopeOf("other-app", "tenant-a"), dim, 1)
			// A different workspace of the same tenant shares the tenant's quota.
			sameTenant := storetest.Scope("app", "tenant-a", "another-workspace")
			if _, err := q.Admit(bg, sameTenant, dim, 1); err == nil {
				t.Fatal("a second workspace of the exhausted tenant escaped the tenant's quota")
			}
			for _, l := range held {
				l.Release()
			}
		})
	}

	t.Run("keys do not collide on concatenation", func(t *testing.T) {
		q, _ := newQ(t)
		exhaust(t, q, scopeOf("ab", "c"), DimRequestsPerMinute)
		mustAdmit(t, q, scopeOf("a", "bc"), DimRequestsPerMinute, 1)
		exhaust(t, q, scopeOf("ab", "c"), DimStorageBytes)
		mustAdmit(t, q, scopeOf("a", "bc"), DimStorageBytes, 1)
	})

	t.Run("the limit is exact: the admit that reaches it passes and the next one fails", func(t *testing.T) {
		q, _ := newQ(t)
		s := scopeOf("app", "t")
		mustAdmit(t, q, s, DimIngestBytesPerDay, 99)
		mustAdmit(t, q, s, DimIngestBytesPerDay, 1) // exactly 100
		if _, err := q.Admit(bg, s, DimIngestBytesPerDay, 1); code(t, err) != contracts.ErrResourceExhausted {
			t.Fatalf("101st byte: %v, want resource_exhausted", err)
		}
		// A request larger than the whole limit fails whole.
		if _, err := q.Admit(bg, scopeOf("app", "u"), DimIngestBytesPerDay, 101); code(t, err) != contracts.ErrResourceExhausted {
			t.Fatalf("101 bytes at once: %v, want resource_exhausted", err)
		}
		// Sessions: two pass, the third fails.
		a, b := mustAdmit(t, q, s, DimConcurrentSessions, 1), mustAdmit(t, q, s, DimConcurrentSessions, 1)
		if _, err := q.Admit(bg, s, DimConcurrentSessions, 1); err == nil {
			t.Fatal("the third session was admitted under a limit of two")
		}
		a.Release()
		mustAdmit(t, q, s, DimConcurrentSessions, 1) // a released slot is reusable
		_ = b
	})

	t.Run("an invalid scope is refused with its own code and nothing is recorded", func(t *testing.T) {
		q, _ := newQ(t)
		noTenant := scopeOf("app", "t")
		noTenant.TenantID = ""
		wildcard := scopeOf("app", "*")
		for name, c := range map[string]struct {
			scope contracts.Scope
			want  contracts.ErrCode
		}{
			"missing tenant": {noTenant, contracts.ErrScopeMissing},
			"wildcard":       {wildcard, contracts.ErrScopeViolation},
			"zero scope":     {contracts.Scope{}, contracts.ErrScopeMissing},
		} {
			for _, dim := range Dimensions() {
				l, err := q.Admit(bg, c.scope, dim, 1)
				if l != nil || err == nil || code(t, err) != c.want {
					t.Errorf("%s / %s: lease %v err %v, want %s", name, dim, l, err, c.want)
				}
			}
			if _, err := q.Usage(bg, c.scope); err == nil {
				t.Errorf("%s: Usage answered for an invalid scope", name)
			}
		}
		if n := q.Stats().TrackedTenants; n != 0 {
			t.Errorf("%d tenants tracked after only invalid scopes", n)
		}
	})
}

// Section 18 (quota exhaustion is never a silent drop) and ADR 0018: rate_limited for rates,
// resource_exhausted for volumes, naming what ran out.
func TestQuotaExhaustionIsTypedError(t *testing.T) {
	want := map[Dimension]contracts.ErrCode{
		DimRequestsPerMinute:  contracts.ErrRateLimited,
		DimConcurrentSessions: contracts.ErrResourceExhausted,
		DimIngestBytesPerDay:  contracts.ErrResourceExhausted,
		DimStorageBytes:       contracts.ErrResourceExhausted,
	}
	for _, dim := range Dimensions() {
		t.Run(string(dim), func(t *testing.T) {
			q, _ := newQ(t)
			s := scopeOf("app", "secret-tenant-id")
			exhaust(t, q, s, dim)
			lease, err := q.Admit(bg, s, dim, 1)
			if err == nil {
				t.Fatal("an exhausted quota admitted the request")
			}
			if lease != nil {
				t.Errorf("a lease %v came back with the refusal", lease)
			}
			if got := code(t, err); got != want[dim] {
				t.Fatalf("code = %s, want %s", got, want[dim])
			}
			if !contracts.CodeOf(err).Retryable() {
				t.Errorf("%s is not retryable", contracts.CodeOf(err))
			}
			if !errors.Is(err, contracts.New(want[dim], "")) {
				t.Errorf("errors.Is does not match %s", want[dim])
			}
			if d := detail(t, err, "dimension"); d != string(dim) {
				t.Errorf("dimension detail = %v, want %s", d, dim)
			}
			if strings.Contains(err.Error(), "secret-tenant-id") {
				t.Errorf("the message names the tenant: %v", err)
			}
			switch dim {
			case DimRequestsPerMinute:
				// 60 a minute is one a second, and the bucket is empty.
				if ms := detail(t, err, "retry_after_ms"); ms != int64(1000) {
					t.Errorf("retry_after_ms = %v, want 1000", ms)
				}
				if lim := detail(t, err, "limit"); lim != int64(60) {
					t.Errorf("limit = %v, want 60", lim)
				}
			case DimIngestBytesPerDay:
				if r := detail(t, err, "resets_at"); r != "2026-10-03T00:00:00Z" {
					t.Errorf("resets_at = %v, want the next UTC midnight", r)
				}
				fallthrough
			case DimStorageBytes, DimConcurrentSessions:
				lim := map[Dimension]int64{DimIngestBytesPerDay: 100, DimStorageBytes: 1000, DimConcurrentSessions: 2}[dim]
				if got := detail(t, err, "limit"); got != lim {
					t.Errorf("limit = %v, want %d", got, lim)
				}
				if got := detail(t, err, "requested"); got != int64(1) {
					t.Errorf("requested = %v, want 1", got)
				}
			}
		})
	}

	t.Run("a request that can never fit is not a retry", func(t *testing.T) {
		q, _ := newQ(t)
		_, err := q.Admit(bg, scopeOf("app", "t"), DimRequestsPerMinute, 6) // burst is 5
		if code(t, err) != contracts.ErrResourceExhausted {
			t.Fatalf("a request larger than the burst: %v, want resource_exhausted (rate_limited would promise a retry that cannot work)", err)
		}
	})

	t.Run("no usable result comes back with a refusal, whatever the dimension", func(t *testing.T) {
		q, _ := newQ(t)
		for _, dim := range Dimensions() {
			s := scopeOf("app", "t-"+string(dim))
			exhaust(t, q, s, dim)
			l, err := q.Admit(bg, s, dim, 1)
			if err == nil || l != nil {
				t.Errorf("%s: lease %v err %v", dim, l, err)
			}
			l.Release() // a nil lease is safe to release
			if err := l.Renew(); err != nil {
				t.Errorf("%s: renewing a nil lease: %v", dim, err)
			}
		}
	})
}

func TestRateBucketBoundaryAndRefill(t *testing.T) {
	q, clk := newQ(t)
	s := scopeOf("app", "t")
	exhaust(t, q, s, DimRequestsPerMinute) // 5 admitted
	_, err := q.Admit(bg, s, DimRequestsPerMinute, 1)
	if code(t, err) != contracts.ErrRateLimited {
		t.Fatalf("the 6th request: %v, want rate_limited", err)
	}
	// One token a second: a nanosecond short is still refused, the second itself admits.
	clk.Advance(time.Second - time.Nanosecond)
	if _, err := q.Admit(bg, s, DimRequestsPerMinute, 1); err == nil {
		t.Fatal("admitted a nanosecond before the token existed")
	}
	clk.Advance(time.Nanosecond)
	mustAdmit(t, q, s, DimRequestsPerMinute, 1)
	if _, err := q.Admit(bg, s, DimRequestsPerMinute, 1); err == nil {
		t.Fatal("the refill gave more than one token")
	}

	t.Run("retry_after_ms is true: waiting exactly that long admits", func(t *testing.T) {
		q, clk := newQ(t)
		s := scopeOf("app", "t")
		exhaust(t, q, s, DimRequestsPerMinute)
		clk.Advance(300 * time.Millisecond)
		_, err := q.Admit(bg, s, DimRequestsPerMinute, 1)
		ms := detail(t, err, "retry_after_ms").(int64)
		if ms != 700 {
			t.Fatalf("retry_after_ms = %d, want 700", ms)
		}
		clk.Advance(time.Duration(ms) * time.Millisecond)
		mustAdmit(t, q, s, DimRequestsPerMinute, 1)
	})

	t.Run("a long idle refills to the burst and no further", func(t *testing.T) {
		q, clk := newQ(t)
		s := scopeOf("app", "t")
		exhaust(t, q, s, DimRequestsPerMinute)
		clk.Advance(24 * time.Hour)
		for i := 0; i < 5; i++ {
			mustAdmit(t, q, s, DimRequestsPerMinute, 1)
		}
		if _, err := q.Admit(bg, s, DimRequestsPerMinute, 1); err == nil {
			t.Fatal("a day of idling banked more than the burst")
		}
	})

	t.Run("a clock that moves back never mints tokens", func(t *testing.T) {
		q, clk := newQ(t)
		s := scopeOf("app", "t")
		exhaust(t, q, s, DimRequestsPerMinute)
		clk.Set(t0.Add(-time.Hour))
		if _, err := q.Admit(bg, s, DimRequestsPerMinute, 1); err == nil {
			t.Fatal("a backwards clock refilled the bucket")
		}
	})

	t.Run("a multi-token request needs all its tokens", func(t *testing.T) {
		q, _ := newQ(t)
		s := scopeOf("app", "t")
		mustAdmit(t, q, s, DimRequestsPerMinute, 4)
		if _, err := q.Admit(bg, s, DimRequestsPerMinute, 2); err == nil {
			t.Fatal("2 tokens admitted with 1 left")
		}
		mustAdmit(t, q, s, DimRequestsPerMinute, 1) // the refusal took nothing
	})
}

// A refusal takes nothing: not a token, not a slot, not a byte (the counter is as it was).
func TestRejectedAdmitChargesNothing(t *testing.T) {
	q, _ := newQ(t)
	s := scopeOf("app", "t")
	mustAdmit(t, q, s, DimIngestBytesPerDay, 60)
	mustAdmit(t, q, s, DimStorageBytes, 900)
	a := mustAdmit(t, q, s, DimConcurrentSessions, 1)
	mustAdmit(t, q, s, DimRequestsPerMinute, 4)

	for i := 0; i < 20; i++ {
		_, _ = q.Admit(bg, s, DimIngestBytesPerDay, 41) // 60+41 > 100
		_, _ = q.Admit(bg, s, DimStorageBytes, 101)     // 900+101 > 1000
		_, _ = q.Admit(bg, s, DimConcurrentSessions, 2) // 1+2 > 2
		_, _ = q.Admit(bg, s, DimRequestsPerMinute, 2)  // 1 token left
	}
	for dim, want := range map[Dimension]int64{
		DimIngestBytesPerDay: 60, DimStorageBytes: 900, DimConcurrentSessions: 1, DimRequestsPerMinute: 4,
	} {
		if got := usageOf(t, q, s, dim).Used; got != want {
			t.Errorf("%s used = %d after refusals, want %d", dim, got, want)
		}
	}
	mustAdmit(t, q, s, DimIngestBytesPerDay, 40) // the room that was left is still there
	a.Release()
}

func TestAmountsAreValidated(t *testing.T) {
	q, _ := newQ(t)
	s := scopeOf("app", "t")
	for _, dim := range Dimensions() {
		for _, n := range []int64{0, -1, -100, MaxAmount + 1} {
			l, err := q.Admit(bg, s, dim, n)
			if l != nil || err == nil || code(t, err) != contracts.ErrValidationFailed {
				t.Errorf("Admit(%s, %d) = %v, %v: want validation_failed (a negative amount would be a credit)", dim, n, l, err)
			}
		}
	}
	if _, err := q.Admit(bg, s, Dimension("made_up"), 1); code(t, err) != contracts.ErrValidationFailed {
		t.Errorf("unknown dimension: %v", err)
	}
	if got := usageOf(t, q, s, DimStorageBytes).Used; got != 0 {
		t.Errorf("storage = %d after invalid amounts", got)
	}
}

// Missing or zero configuration is finite; unlimited exists only as an explicit -1.
func TestDefaultsAreFiniteAndUnlimitedIsExplicit(t *testing.T) {
	db := storetest.Open(t, nil)
	for _, profile := range []string{"", ProfileExperimental4G, ProfileLite, ProfileBalanced, ProfilePerformance} {
		q, err := New(db, Options{Config: Config{Profile: profile}})
		if err != nil {
			t.Fatalf("profile %q: %v", profile, err)
		}
		l, err := q.Limits(scopeOf("app", "t"))
		if err != nil {
			t.Fatal(err)
		}
		for _, dim := range Dimensions() {
			if v := l.of(dim); v <= 0 {
				t.Errorf("profile %q: %s = %d: a missing configuration must be a finite limit", profile, dim, v)
			}
		}
		if l.Burst < 1 {
			t.Errorf("profile %q: burst %d", profile, l.Burst)
		}
	}
	// A zero-value Options is the lite profile.
	q, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	lite, _ := ProfileLimits(ProfileLite)
	got, _ := q.Limits(scopeOf("app", "t"))
	if got.RequestsPerMinute != lite.RequestsPerMinute || got.StorageBytes != lite.StorageBytes {
		t.Errorf("zero Options gave %v, want the lite profile %v", got, lite)
	}
	// The profiles grow with the hardware.
	prev := Limits{}
	for _, p := range []string{ProfileExperimental4G, ProfileLite, ProfileBalanced, ProfilePerformance} {
		l, _ := ProfileLimits(p)
		if l.RequestsPerMinute <= prev.RequestsPerMinute || l.IngestBytesPerDay <= prev.IngestBytesPerDay || l.StorageBytes <= prev.StorageBytes {
			t.Errorf("%s does not exceed the smaller profile: %v vs %v", p, l, prev)
		}
		prev = l
	}

	t.Run("-1 is the explicit unlimited", func(t *testing.T) {
		q, _ := newQ(t, func(c *Config) {
			c.Defaults = Limits{RequestsPerMinute: Unlimited, ConcurrentSessions: Unlimited, IngestBytesPerDay: Unlimited, StorageBytes: Unlimited}
		})
		s := scopeOf("app", "t")
		for i := 0; i < 2000; i++ {
			mustAdmit(t, q, s, DimRequestsPerMinute, 1)
		}
		for i := 0; i < 50; i++ {
			mustAdmit(t, q, s, DimConcurrentSessions, 1)
		}
		mustAdmit(t, q, s, DimIngestBytesPerDay, 1<<40)
		mustAdmit(t, q, s, DimStorageBytes, 1<<40)
		if u := usageOf(t, q, s, DimStorageBytes); u.Limit != Unlimited || u.Remaining != Unlimited {
			t.Errorf("an unlimited dimension reports %+v", u)
		}
	})
	t.Run("one explicit unlimited leaves the others finite", func(t *testing.T) {
		q, _ := newQ(t, func(c *Config) { c.Defaults.StorageBytes = Unlimited })
		s := scopeOf("app", "t")
		mustAdmit(t, q, s, DimStorageBytes, 1<<40)
		exhaust(t, q, s, DimIngestBytesPerDay)
		if _, err := q.Admit(bg, s, DimIngestBytesPerDay, 1); err == nil {
			t.Fatal("ingestion became unlimited with it")
		}
	})
}

func TestConfigValidateAndOverrides(t *testing.T) {
	bad := map[string]Config{
		"unknown profile":     {Profile: "huge"},
		"negative rate":       {Defaults: Limits{RequestsPerMinute: -5}},
		"negative storage":    {Defaults: Limits{StorageBytes: -2}},
		"unlimited burst":     {Defaults: Limits{Burst: -1}},
		"rate over ceiling":   {Defaults: Limits{RequestsPerMinute: maxRate + 1}},
		"negative lease ttl":  {LeaseTTL: -time.Second},
		"override no app":     {Overrides: []Override{{Limits: Limits{StorageBytes: 5}}}},
		"override wildcard":   {Overrides: []Override{{ApplicationID: "*"}}},
		"tenant wildcard":     {Overrides: []Override{{ApplicationID: "a", TenantID: "t*"}}},
		"blank padded app":    {Overrides: []Override{{ApplicationID: " a "}}},
		"duplicate override":  {Overrides: []Override{{ApplicationID: "a"}, {ApplicationID: "a"}}},
		"override bad limits": {Overrides: []Override{{ApplicationID: "a", Limits: Limits{ConcurrentSessions: -9}}}},
	}
	db := storetest.Open(t, nil)
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
		if _, err := New(db, Options{Config: c}); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	if _, err := New(nil, Options{}); err == nil {
		t.Error("New accepted a nil store")
	}

	t.Run("tenant over application over defaults over profile", func(t *testing.T) {
		q, _ := newQ(t, func(c *Config) {
			c.Defaults = Limits{StorageBytes: 1000}
			c.Overrides = []Override{
				{ApplicationID: "big", Limits: Limits{StorageBytes: 5000, ConcurrentSessions: 9}},
				{ApplicationID: "big", TenantID: "vip", Limits: Limits{StorageBytes: 9000}},
				{ApplicationID: "solo", TenantID: "t", Limits: Limits{RequestsPerMinute: 7}},
			}
		})
		lite, _ := ProfileLimits(ProfileLite)
		cases := []struct {
			app, tenant      string
			storage, session int64
			rpm              int64
		}{
			{"other", "x", 1000, lite.ConcurrentSessions, lite.RequestsPerMinute}, // defaults, then the profile
			{"big", "x", 5000, 9, lite.RequestsPerMinute},                         // the application's
			{"big", "vip", 9000, 9, lite.RequestsPerMinute},                       // the tenant's, inheriting the application's sessions
			{"solo", "t", 1000, lite.ConcurrentSessions, 7},                       // a tenant override with no application override
			{"solo", "u", 1000, lite.ConcurrentSessions, lite.RequestsPerMinute},  // not the overridden tenant
			{"other", "vip", 1000, lite.ConcurrentSessions, lite.RequestsPerMinute},
		}
		for _, c := range cases {
			l, err := q.Limits(scopeOf(c.app, c.tenant))
			if err != nil {
				t.Fatal(err)
			}
			if l.StorageBytes != c.storage || l.ConcurrentSessions != c.session || l.RequestsPerMinute != c.rpm {
				t.Errorf("%s/%s: %v, want storage=%d sessions=%d rpm=%d", c.app, c.tenant, l, c.storage, c.session, c.rpm)
			}
		}
		if _, err := q.Admit(bg, scopeOf("big", "x"), DimStorageBytes, 5001); err == nil {
			t.Error("the application's override is not enforced")
		}
		mustAdmit(t, q, scopeOf("big", "vip"), DimStorageBytes, 9000)
	})
}

func TestConfigLoadsFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alixai.yaml")
	yaml := `quotas:
  profile: balanced
  defaults:
    requests_per_minute: 77
    storage_bytes: -1
  lease_ttl: 30m
  overrides:
    - application: clinic
      tenant: north
      limits:
        concurrent_sessions: 3
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	r := config.NewRegistry()
	c := DefaultConfig()
	r.Register("quotas", &c)
	if err := r.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Profile != ProfileBalanced || c.Defaults.RequestsPerMinute != 77 || c.Defaults.StorageBytes != Unlimited ||
		c.LeaseTTL != 30*time.Minute || len(c.Overrides) != 1 || c.Overrides[0].Limits.ConcurrentSessions != 3 {
		t.Fatalf("decoded %+v", c)
	}
	if err := os.WriteFile(path, []byte("quotas:\n  profile: nonsense\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := config.NewRegistry()
	c2 := DefaultConfig()
	r2.Register("quotas", &c2)
	if err := r2.Load(path); err == nil {
		t.Fatal("Load accepted an unknown profile: the section's Validate does not run")
	}
}

// The count is exact under contention: no overshoot, no lost update.
func TestAdmitIsExactUnderContention(t *testing.T) {
	const workers = 32
	run := func(t *testing.T, dim Dimension, attempts int, want int64) {
		t.Helper()
		q, _ := newQ(t, func(c *Config) {
			c.Defaults = Limits{RequestsPerMinute: 60, Burst: 100, ConcurrentSessions: 50, IngestBytesPerDay: 1000, StorageBytes: 1000}
		})
		s := scopeOf("app", "t")
		var ok, refused atomic.Int64
		var wg sync.WaitGroup
		var held sync.Map
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < attempts; i++ {
					amount := int64(1)
					if dim == DimIngestBytesPerDay || dim == DimStorageBytes {
						amount = 10
					}
					l, err := q.Admit(bg, s, dim, amount)
					switch {
					case err == nil:
						ok.Add(1)
						held.Store(l, true) // keep every lease held
					case code(t, err) == contracts.ErrRateLimited || code(t, err) == contracts.ErrResourceExhausted:
						refused.Add(1)
					default:
						t.Errorf("unexpected error %v", err)
					}
				}
			}()
		}
		wg.Wait()
		if ok.Load() != want {
			t.Errorf("%s: %d admitted, want exactly %d (refused %d)", dim, ok.Load(), want, refused.Load())
		}
		if ok.Load()+refused.Load() != int64(workers*attempts) {
			t.Errorf("%s: %d decisions for %d attempts: a request was dropped", dim, ok.Load()+refused.Load(), workers*attempts)
		}
	}
	t.Run("requests", func(t *testing.T) { run(t, DimRequestsPerMinute, 10, 100) })
	t.Run("sessions", func(t *testing.T) { run(t, DimConcurrentSessions, 10, 50) })
	t.Run("ingest bytes", func(t *testing.T) { run(t, DimIngestBytesPerDay, 5, 100) })
	t.Run("storage", func(t *testing.T) { run(t, DimStorageBytes, 5, 100) })
}

func TestLeaseReleaseIsIdempotentAndExpires(t *testing.T) {
	q, clk := newQ(t, func(c *Config) { c.LeaseTTL = time.Minute })
	s := scopeOf("app", "t")

	a, b := mustAdmit(t, q, s, DimConcurrentSessions, 1), mustAdmit(t, q, s, DimConcurrentSessions, 1)
	a.Release()
	a.Release()
	a.Release() // the counter must not go below zero or free b's slot
	if got := usageOf(t, q, s, DimConcurrentSessions).Used; got != 1 {
		t.Fatalf("slots = %d after a triple release of one lease, want 1", got)
	}
	c := mustAdmit(t, q, s, DimConcurrentSessions, 1)
	if _, err := q.Admit(bg, s, DimConcurrentSessions, 1); err == nil {
		t.Fatal("a repeated Release freed a slot it did not hold")
	}

	// An unrenewed lease expires and its slot is reusable; a renewed one stays.
	clk.Advance(40 * time.Second)
	if err := c.Renew(); err != nil {
		t.Fatalf("Renew of a live lease: %v", err)
	}
	clk.Advance(30 * time.Second) // b is 70s old (expired), c was renewed 30s ago
	d := mustAdmit(t, q, s, DimConcurrentSessions, 1)
	if err := b.Renew(); code(t, err) != contracts.ErrExpired {
		t.Fatalf("Renew of an expired lease: %v, want expired", err)
	}
	b.Release() // releasing the reclaimed lease must not free d's slot
	if _, err := q.Admit(bg, s, DimConcurrentSessions, 1); err == nil {
		t.Fatal("the released-after-expiry lease freed a slot that belongs to another session")
	}
	if got := usageOf(t, q, s, DimConcurrentSessions).Used; got != 2 {
		t.Fatalf("slots = %d, want 2 (c and d)", got)
	}
	c.Release()
	d.Release()
	if got := usageOf(t, q, s, DimConcurrentSessions).Used; got != 0 {
		t.Fatalf("slots = %d after everything was released", got)
	}
}

func TestDailyCounterRollsOverAtUTCMidnight(t *testing.T) {
	q, clk := newQ(t)
	s := scopeOf("app", "t")
	clk.Set(time.Date(2026, 10, 2, 23, 59, 59, 0, time.UTC))
	mustAdmit(t, q, s, DimIngestBytesPerDay, 100)
	if _, err := q.Admit(bg, s, DimIngestBytesPerDay, 1); err == nil {
		t.Fatal("the day's quota was not enforced")
	}
	clk.Advance(time.Second) // 00:00:00 on the 3rd
	mustAdmit(t, q, s, DimIngestBytesPerDay, 100)
	u := usageOf(t, q, s, DimIngestBytesPerDay)
	if u.Used != 100 || !u.ResetsAt.Equal(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("usage after the rollover = %+v", u)
	}

	t.Run("a clock in another zone does not move the day", func(t *testing.T) {
		msk := time.FixedZone("MSK", 3*3600)
		zc := &zonedClock{t: time.Date(2026, 10, 3, 2, 30, 0, 0, msk)} // 23:30 UTC on the 2nd
		db := storetest.Open(t, nil)
		cfg := DefaultConfig()
		cfg.Defaults = testLimits()
		q, err := New(db, Options{Config: cfg, Clock: zc})
		if err != nil {
			t.Fatal(err)
		}
		s := scopeOf("app", "t")
		mustAdmit(t, q, s, DimIngestBytesPerDay, 100)
		_, err = q.Admit(bg, s, DimIngestBytesPerDay, 1)
		if r := detail(t, err, "resets_at"); r != "2026-10-03T00:00:00Z" {
			t.Errorf("resets_at = %v, want UTC midnight in UTC", r)
		}
		zc.t = time.Date(2026, 10, 3, 2, 59, 59, 0, msk) // 23:59:59 UTC: still the 2nd
		if _, err := q.Admit(bg, s, DimIngestBytesPerDay, 1); err == nil {
			t.Error("the day rolled over at local midnight, not UTC midnight")
		}
		zc.t = time.Date(2026, 10, 3, 3, 0, 0, 0, msk) // 00:00:00 UTC on the 3rd
		mustAdmit(t, q, s, DimIngestBytesPerDay, 100)
	})

	t.Run("the gauge does not reset", func(t *testing.T) {
		q, clk := newQ(t)
		s := scopeOf("app", "t")
		mustAdmit(t, q, s, DimStorageBytes, 700)
		clk.Advance(72 * time.Hour)
		if got := usageOf(t, q, s, DimStorageBytes).Used; got != 700 {
			t.Errorf("storage = %d three days later, want 700", got)
		}
	})

	t.Run("Prune keeps recent windows and never touches the gauge", func(t *testing.T) {
		q, clk := newQ(t)
		s := scopeOf("app", "t")
		for i := 0; i < 10; i++ {
			mustAdmit(t, q, s, DimIngestBytesPerDay, 1)
			clk.Advance(24 * time.Hour)
		}
		mustAdmit(t, q, s, DimStorageBytes, 5)
		n, err := q.Prune(bg, 3)
		if err != nil {
			t.Fatal(err)
		}
		if n != 8 {
			t.Errorf("pruned %d rows, want 8: the windows of the 10 days before the last 3 days (today and two before)", n)
		}
		if got := usageOf(t, q, s, DimStorageBytes).Used; got != 5 {
			t.Errorf("the gauge was touched: %d", got)
		}
		if _, err := q.Prune(bg, 0); err == nil {
			t.Error("Prune(0) would delete today's counter")
		}
	})
}

func TestCountersSurviveReopeningTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	open := func() (*Quotas, *store.DB) {
		db, err := store.Open(bg, store.Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.Defaults = testLimits()
		q, err := New(db, Options{Config: cfg, Clock: clock.NewFake(t0)})
		if err != nil {
			t.Fatal(err)
		}
		return q, db
	}
	q, db := open()
	s := scopeOf("app", "t")
	mustAdmit(t, q, s, DimIngestBytesPerDay, 70)
	mustAdmit(t, q, s, DimStorageBytes, 400)
	exhaust(t, q, s, DimRequestsPerMinute)
	held := mustAdmit(t, q, s, DimConcurrentSessions, 1)
	_ = held
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	q, db = open()
	defer db.Close()
	if got := usageOf(t, q, s, DimIngestBytesPerDay).Used; got != 70 {
		t.Errorf("ingest bytes after a restart = %d, want 70", got)
	}
	if got := usageOf(t, q, s, DimStorageBytes).Used; got != 400 {
		t.Errorf("storage after a restart = %d, want 400", got)
	}
	if _, err := q.Admit(bg, s, DimIngestBytesPerDay, 31); err == nil {
		t.Error("the persisted counter was not enforced after the restart")
	}
	// In-memory state starts clean: the sessions died with the process, the bucket is full.
	if got := usageOf(t, q, s, DimConcurrentSessions).Used; got != 0 {
		t.Errorf("sessions after a restart = %d, want 0", got)
	}
	mustAdmit(t, q, s, DimRequestsPerMinute, 5)
}

func TestRefundAndSetStorageNeverGoNegative(t *testing.T) {
	q, _ := newQ(t)
	s := scopeOf("app", "t")
	mustAdmit(t, q, s, DimStorageBytes, 300)
	if err := q.Refund(bg, s, DimStorageBytes, 100); err != nil {
		t.Fatal(err)
	}
	if got := usageOf(t, q, s, DimStorageBytes).Used; got != 200 {
		t.Fatalf("storage = %d, want 200", got)
	}
	if err := q.Refund(bg, s, DimStorageBytes, 10_000); err != nil {
		t.Fatal(err)
	}
	if got := usageOf(t, q, s, DimStorageBytes).Used; got != 0 {
		t.Fatalf("storage = %d after over-refunding, want 0 (never negative)", got)
	}
	if err := q.Refund(bg, scopeOf("app", "nobody"), DimStorageBytes, 5); err != nil {
		t.Errorf("refunding a tenant with no counter: %v", err)
	}
	for _, dim := range []Dimension{DimRequestsPerMinute, DimConcurrentSessions} {
		if err := q.Refund(bg, s, dim, 1); code(t, err) != contracts.ErrValidationFailed {
			t.Errorf("Refund(%s) = %v: only persisted dimensions can be refunded", dim, err)
		}
	}
	if err := q.Refund(bg, s, DimStorageBytes, -5); err == nil {
		t.Error("a negative refund is a charge in disguise")
	}
	// A refund that frees room makes the dimension admit again; a refund of another tenant does not.
	exhaust(t, q, s, DimIngestBytesPerDay)
	_ = q.Refund(bg, scopeOf("app", "other"), DimIngestBytesPerDay, 50)
	if _, err := q.Admit(bg, s, DimIngestBytesPerDay, 1); err == nil {
		t.Fatal("a refund of another tenant gave this tenant room")
	}
	if err := q.Refund(bg, s, DimIngestBytesPerDay, 10); err != nil {
		t.Fatal(err)
	}
	mustAdmit(t, q, s, DimIngestBytesPerDay, 10)

	if err := q.SetStorage(bg, s, 123); err != nil {
		t.Fatal(err)
	}
	if got := usageOf(t, q, s, DimStorageBytes).Used; got != 123 {
		t.Errorf("SetStorage: %d", got)
	}
	if err := q.SetStorage(bg, s, -1); err == nil {
		t.Error("SetStorage took a negative size")
	}
}

// The data and its charge commit or roll back together.
func TestChargeTxIsAtomicWithTheData(t *testing.T) {
	q, _, db := newQDB(t)
	if _, err := db.ExecContext(bg, `CREATE TABLE blobs (id TEXT PRIMARY KEY, n INTEGER NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	s := scopeOf("app", "t")
	write := func(id string, n int64) error {
		return db.Tx(bg, func(tx *store.Tx) error {
			if _, err := tx.ExecContext(bg, `INSERT INTO blobs VALUES (?, ?)`, id, n); err != nil {
				return err
			}
			return q.ChargeTx(bg, tx, s, DimIngestBytesPerDay, n)
		})
	}
	if err := write("a", 60); err != nil {
		t.Fatal(err)
	}
	err := write("b", 50) // 110 > 100
	if code(t, err) != contracts.ErrResourceExhausted {
		t.Fatalf("over-quota write: %v, want resource_exhausted", err)
	}
	var rows int
	if err := db.QueryRowContext(bg, `SELECT count(*) FROM blobs`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d blobs after the refused write (err %v): the data must not be written when the quota refuses", rows, err)
	}
	// The other way round: the data write fails after the charge, and the charge goes with it.
	err = db.Tx(bg, func(tx *store.Tx) error {
		if err := q.ChargeTx(bg, tx, s, DimIngestBytesPerDay, 40); err != nil {
			return err
		}
		_, err := tx.ExecContext(bg, `INSERT INTO blobs VALUES ('a', 40)`) // duplicate key
		return err
	})
	if !store.IsUniqueViolation(err) {
		t.Fatalf("expected the duplicate key to fail the write, got %v", err)
	}
	if got := usageOf(t, q, s, DimIngestBytesPerDay).Used; got != 60 {
		t.Fatalf("used = %d after the data write rolled back, want 60: the charge outlived its data", got)
	}
	// A caller that swallows the refusal and commits anyway has still been charged nothing.
	if err := db.Tx(bg, func(tx *store.Tx) error {
		if err := q.ChargeTx(bg, tx, s, DimIngestBytesPerDay, 41); code(t, err) != contracts.ErrResourceExhausted { // 60+41 > 100
			t.Errorf("ChargeTx over the limit: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := usageOf(t, q, s, DimIngestBytesPerDay).Used; got != 60 {
		t.Fatalf("used = %d after a refused ChargeTx whose caller committed anyway, want 60: a refusal must charge nothing", got)
	}
	if err := db.Tx(bg, func(tx *store.Tx) error {
		return q.ChargeTx(bg, tx, s, DimRequestsPerMinute, 1)
	}); code(t, err) != contracts.ErrValidationFailed {
		t.Errorf("ChargeTx of an in-memory dimension: %v", err)
	}
	if err := db.Tx(bg, func(tx *store.Tx) error {
		return q.ChargeTx(bg, tx, contracts.Scope{}, DimStorageBytes, 1)
	}); err == nil {
		t.Error("ChargeTx took an empty scope")
	}
}

func TestUsageReportsOnlyTheCallersTenant(t *testing.T) {
	q, _ := newQ(t)
	a, b := scopeOf("app", "a"), scopeOf("app", "b")
	mustAdmit(t, q, a, DimStorageBytes, 600)
	mustAdmit(t, q, a, DimRequestsPerMinute, 3)
	held := mustAdmit(t, q, a, DimConcurrentSessions, 1)
	mustAdmit(t, q, b, DimStorageBytes, 10)

	got := map[Dimension]Usage{}
	us, err := q.Usage(bg, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		got[u.Dimension] = u
	}
	if got[DimStorageBytes].Used != 600 || got[DimStorageBytes].Remaining != 400 || got[DimStorageBytes].Limit != 1000 {
		t.Errorf("storage %+v", got[DimStorageBytes])
	}
	if got[DimRequestsPerMinute].Used != 3 || got[DimRequestsPerMinute].Remaining != 2 {
		t.Errorf("requests %+v", got[DimRequestsPerMinute])
	}
	if got[DimConcurrentSessions].Used != 1 || got[DimConcurrentSessions].Remaining != 1 {
		t.Errorf("sessions %+v", got[DimConcurrentSessions])
	}
	if len(us) != 4 {
		t.Errorf("%d dimensions reported, want 4", len(us))
	}
	if ub := usageOf(t, q, b, DimStorageBytes); ub.Used != 10 {
		t.Errorf("tenant b sees %+v", ub)
	}
	held.Release()
}

// Idle tenants are forgotten (memory follows the active tenants), but never in a way that
// gives anyone tokens or frees a held session.
func TestIdleTenantsAreEvictedWithoutAGift(t *testing.T) {
	q, clk := newQ(t, func(c *Config) {
		c.IdleEvictAfter = 2 * time.Second // a sweep every second, a tenant is idle after two
		c.Defaults.RequestsPerMinute = 1   // one token a minute: a drained bucket takes minutes to refill
		c.Defaults.Burst = 5
	})
	trigger := scopeOf("app", "trigger")
	drained, withSession := scopeOf("app", "drained"), scopeOf("app", "session")

	// 100 tenants spend one token each; a minute later they are full again.
	for i := 0; i < 100; i++ {
		mustAdmit(t, q, scopeOf("app", fmt.Sprintf("bulk-%d", i)), DimRequestsPerMinute, 1)
	}
	session := mustAdmit(t, q, withSession, DimConcurrentSessions, 1)
	clk.Advance(61 * time.Second)
	exhaust(t, q, drained, DimRequestsPerMinute)

	clk.Advance(3 * time.Second)
	mustAdmit(t, q, trigger, DimRequestsPerMinute, 1) // the sweep runs on the way in
	if st := q.Stats().TrackedTenants; st != 3 {
		t.Errorf("%d tenants tracked, want 3: the drained one, the one with a session and the trigger (the 100 full idle ones are forgotten)", st)
	}
	if _, err := q.Admit(bg, drained, DimRequestsPerMinute, 1); code(t, err) != contracts.ErrRateLimited {
		t.Errorf("a drained tenant was forgotten and got a fresh bucket: %v", err)
	}
	if got := usageOf(t, q, withSession, DimConcurrentSessions).Used; got != 1 {
		t.Errorf("a tenant with an open session was forgotten: %d slots held, want 1", got)
	}
	session.Release()

	// Once the drained bucket would be full again, the tenant is forgotten too.
	clk.Advance(10 * time.Minute)
	mustAdmit(t, q, trigger, DimRequestsPerMinute, 1)
	if st := q.Stats().TrackedTenants; st != 1 {
		t.Errorf("%d tenants tracked after everyone refilled, want only the trigger", st)
	}
}

// zonedClock reports its time in whatever zone the test chose.
type zonedClock struct{ t time.Time }

func (z *zonedClock) Now() time.Time { return z.t }

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func TestStatsCountDecisions(t *testing.T) {
	q, _ := newQ(t)
	s := scopeOf("app", "t")
	exhaust(t, q, s, DimRequestsPerMinute)
	_, _ = q.Admit(bg, s, DimRequestsPerMinute, 1)
	_, _ = q.Admit(bg, s, DimRequestsPerMinute, 1)
	st := q.Stats()
	if st.Admitted[DimRequestsPerMinute] != 5 || st.Rejected[DimRequestsPerMinute] != 2 {
		t.Errorf("admitted %d rejected %d, want 5 and 2", st.Admitted[DimRequestsPerMinute], st.Rejected[DimRequestsPerMinute])
	}
}

func TestQuotaTablesCarryTheScopeColumns(t *testing.T) {
	q, _, db := newQDB(t)
	storetest.AssertScopedTables(t, db)
	s := scopeOf("app", "t")
	mustAdmit(t, q, s, DimIngestBytesPerDay, 5)
	mustAdmit(t, q, s, DimStorageBytes, 5)
	var rows, bad int
	if err := db.QueryRowContext(bg, `SELECT count(*), sum(workspace_id <> '' OR application_id <> 'app' OR tenant_id <> 't') FROM quota_counters`).Scan(&rows, &bad); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || bad != 0 {
		t.Errorf("%d rows, %d with the wrong key: quotas are per application and tenant, workspace_id is empty", rows, bad)
	}
	// The schema itself refuses a negative counter and a workspace-level row.
	for _, stmt := range []string{
		`INSERT INTO quota_counters VALUES ('a', 't', '', 'storage_bytes', 0, -1, 0)`,
		`INSERT INTO quota_counters VALUES ('a', 't', 'w', 'storage_bytes', 0, 1, 0)`,
		`INSERT INTO quota_counters VALUES ('a', 't', '', 'storage_bytes', 5, 1, 0)`,
		`INSERT INTO quota_counters VALUES ('a', 't', '', 'made_up', 0, 1, 0)`,
	} {
		if _, err := db.ExecContext(bg, stmt); !store.IsConstraint(err) {
			t.Errorf("%s: %v, want a constraint violation", stmt, err)
		}
	}
}

// ADR 0018 measurement 1: the cost of admission and the memory per active tenant.
func TestMemoryPerActiveTenant(t *testing.T) {
	const tenants = 10_000
	q, _ := newQ(t)
	scopes := make([]contracts.Scope, tenants)
	for i := range scopes {
		scopes[i] = scopeOf("app", fmt.Sprintf("tenant-%06d", i))
	}
	before := heapInUse()
	for i := range scopes {
		mustAdmit(t, q, scopes[i], DimRequestsPerMinute, 1)
	}
	after := heapInUse()
	runtime.KeepAlive(scopes)
	if n := q.Stats().TrackedTenants; n != tenants {
		t.Fatalf("%d tenants tracked, want %d", n, tenants)
	}
	per := float64(after-before) / tenants
	t.Logf("memory per active tenant: %.0f bytes (%d tenants, heap %d -> %d bytes)", per, tenants, before, after)
	if per > 1024 {
		t.Errorf("%.0f bytes per active tenant, the ADR's budget is a few hundred and the ceiling 1 KiB", per)
	}
}

func BenchmarkAdmitRequest10kTenants(b *testing.B) {
	q, clk := newQ(b)
	const tenants = 10_000
	scopes := make([]contracts.Scope, tenants)
	for i := range scopes {
		scopes[i] = scopeOf("app", fmt.Sprintf("tenant-%06d", i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%tenants == 0 {
			clk.Advance(time.Minute) // refill everyone: measure the admit, not the refusal
		}
		if _, err := q.Admit(bg, scopes[i%tenants], DimRequestsPerMinute, 1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAdmitRequestOneTenantParallel(b *testing.B) {
	q, _ := newQ(b, func(c *Config) { c.Defaults.RequestsPerMinute = Unlimited })
	s := scopeOf("app", "t")
	// Unlimited returns before the lock; use a huge finite limit to take the full path.
	q2, _ := newQ(b, func(c *Config) { c.Defaults.RequestsPerMinute = maxRate; c.Defaults.Burst = maxBurst })
	_ = q
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := q2.Admit(bg, s, DimRequestsPerMinute, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkAdmitRefusedRequest(b *testing.B) {
	q, _ := newQ(b)
	s := scopeOf("app", "t")
	exhaust(b, q, s, DimRequestsPerMinute)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := q.Admit(bg, s, DimRequestsPerMinute, 1); err == nil {
			b.Fatal("admitted")
		}
	}
}

func BenchmarkChargeIngestBytes(b *testing.B) {
	q, _ := newQ(b, func(c *Config) { c.Defaults.IngestBytesPerDay = 1 << 50 })
	s := scopeOf("app", "t")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := q.Admit(bg, s, DimIngestBytesPerDay, 1024); err != nil {
			b.Fatal(err)
		}
	}
}

// ADR 0018 measurement 2: a tenant that floods requests and bulk-ingests does not take the other
// tenants' admissions away, and does not slow them down by more than the shared writer costs.
func TestNoisyNeighbourDoesNotStarveOthers(t *testing.T) {
	q, _ := newQ(t, func(c *Config) {
		c.Overrides = []Override{{ApplicationID: "app", TenantID: "quiet", Limits: Limits{
			RequestsPerMinute: maxRate, Burst: maxBurst, IngestBytesPerDay: Unlimited, StorageBytes: Unlimited, ConcurrentSessions: 1000}}}
	})
	noisy, quiet := scopeOf("app", "noisy"), scopeOf("app", "quiet")

	measure := func() (p95 [2]time.Duration, refused int) {
		var rate, ingest []time.Duration
		for i := 0; i < 300; i++ {
			start := time.Now()
			_, e1 := q.Admit(bg, quiet, DimRequestsPerMinute, 1)
			rate = append(rate, time.Since(start))
			start = time.Now()
			_, e2 := q.Admit(bg, quiet, DimIngestBytesPerDay, 1)
			ingest = append(ingest, time.Since(start))
			if e1 != nil || e2 != nil {
				refused++
			}
		}
		for i, l := range [][]time.Duration{rate, ingest} {
			sort.Slice(l, func(a, b int) bool { return l[a] < l[b] })
			p95[i] = l[len(l)*95/100]
		}
		return p95, refused
	}
	baseline, refused := measure()
	if refused != 0 {
		t.Fatalf("the quiet tenant was refused %d times with no neighbour", refused)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var noisyRefused, noisyAdmitted atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				dim := DimRequestsPerMinute
				if w == 0 {
					dim = DimIngestBytesPerDay // the writer: every call is a write transaction
				}
				if _, err := q.Admit(bg, noisy, dim, 1); err != nil {
					noisyRefused.Add(1)
				} else {
					noisyAdmitted.Add(1)
				}
			}
		}(w)
	}
	loaded, refused := measure()
	close(stop)
	wg.Wait()

	t.Logf("quiet tenant p95: request admit %s alone, %s beside a flooding neighbour; ingest charge %s alone, %s beside it (neighbour: %d admitted, %d refused)",
		baseline[0], loaded[0], baseline[1], loaded[1], noisyAdmitted.Load(), noisyRefused.Load())
	if refused != 0 {
		t.Errorf("the quiet tenant was refused %d times because of its neighbour", refused)
	}
	if noisyRefused.Load() == 0 {
		t.Error("the flooding tenant was never refused: the test did not flood")
	}
	if got := usageOf(t, q, noisy, DimIngestBytesPerDay).Used; got > 100 {
		t.Errorf("the noisy tenant ingested %d bytes under a limit of 100", got)
	}
}
