package dossec

import (
	"context"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/quotas"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var (
	bg = context.Background()
	t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
)

// small limits, so a flood reaches every ceiling in a few calls.
func floodLimits() quotas.Limits {
	return quotas.Limits{RequestsPerMinute: 60, Burst: 5, ConcurrentSessions: 2, IngestBytesPerDay: 100, StorageBytes: 1000}
}

func newQuotas(t *testing.T, mutate ...func(*quotas.Config)) *quotas.Quotas {
	t.Helper()
	cfg := quotas.DefaultConfig()
	cfg.Defaults = floodLimits()
	for _, m := range mutate {
		m(&cfg)
	}
	q, err := quotas.New(storetest.Open(t, nil), quotas.Options{Config: cfg, Clock: clock.NewFake(t0)})
	if err != nil {
		t.Fatalf("quotas.New: %v", err)
	}
	return q
}

func scope(app, tenant string) contracts.Scope { return storetest.Scope(app, tenant, "w1") }

// admitN admits n units one at a time and returns how many were admitted and the refusal codes.
func admitN(t *testing.T, q *quotas.Quotas, s contracts.Scope, dim quotas.Dimension, amount int64, n int) (admitted int, refusals []contracts.ErrCode) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := q.Admit(bg, s, dim, amount)
		if err == nil {
			admitted++
			continue
		}
		if code := contracts.CodeOf(err); code != contracts.ErrRateLimited && code != contracts.ErrResourceExhausted {
			t.Fatalf("%s for %s/%s: refused with %v (code %q), want rate_limited or resource_exhausted", dim, s.ApplicationID, s.TenantID, err, code)
		}
		refusals = append(refusals, contracts.CodeOf(err))
	}
	return admitted, refusals
}

func usedOf(t *testing.T, q *quotas.Quotas, s contracts.Scope, dim quotas.Dimension) int64 {
	t.Helper()
	us, err := q.Usage(bg, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		if u.Dimension == dim {
			return u.Used
		}
	}
	t.Fatalf("no usage row for %s", dim)
	return 0
}

// SEC-077: a flood from one tenant is refused with typed codes at that tenant's own limit, and does not
// take any of another tenant's allowance, in any of the four dimensions.
func TestSecOneTenantCannotStarveAnother(t *testing.T) {
	lim := floodLimits()
	flooder := scope("app-1", "flooder")
	others := map[string]contracts.Scope{
		"another tenant of the application":         scope("app-1", "victim"),
		"the same tenant id in another application": scope("app-2", "flooder"),
	}
	cases := []struct {
		dim      quotas.Dimension
		amount   int64
		allowed  int // what one tenant is admitted before its first refusal
		wantCode contracts.ErrCode
	}{
		{quotas.DimRequestsPerMinute, 1, int(lim.Burst), contracts.ErrRateLimited},
		{quotas.DimConcurrentSessions, 1, int(lim.ConcurrentSessions), contracts.ErrResourceExhausted},
		{quotas.DimIngestBytesPerDay, 10, int(lim.IngestBytesPerDay / 10), contracts.ErrResourceExhausted},
		{quotas.DimStorageBytes, 100, int(lim.StorageBytes / 100), contracts.ErrResourceExhausted},
	}
	for _, c := range cases {
		for name, victim := range others {
			t.Run(string(c.dim)+": "+name, func(t *testing.T) {
				q := newQuotas(t)
				// Control: the flood does reach the limit, with a typed refusal and never a silent drop.
				admitted, refusals := admitN(t, q, flooder, c.dim, c.amount, c.allowed*3)
				if admitted != c.allowed {
					t.Fatalf("the flooder was admitted %d times, want exactly its allowance %d", admitted, c.allowed)
				}
				if len(refusals) != c.allowed*2 {
					t.Fatalf("the flooder was refused %d times, want %d", len(refusals), c.allowed*2)
				}
				for _, code := range refusals {
					if code != c.wantCode {
						t.Fatalf("refusal code %q, want %q", code, c.wantCode)
					}
				}
				// The victim has its whole allowance, and is refused only at its own limit.
				before := usedOf(t, q, victim, c.dim)
				admitted, refusals = admitN(t, q, victim, c.dim, c.amount, c.allowed+1)
				if admitted != c.allowed || len(refusals) != 1 || refusals[0] != c.wantCode {
					t.Fatalf("the other tenant was admitted %d of %d with refusals %v: the flood took its allowance", admitted, c.allowed, refusals)
				}
				if before != 0 {
					t.Errorf("the other tenant's usage was %d before it did anything: the flood was charged to it", before)
				}
				if got := usedOf(t, q, flooder, c.dim); got > int64(c.allowed)*c.amount && c.dim != quotas.DimConcurrentSessions && c.dim != quotas.DimRequestsPerMinute {
					t.Errorf("the flooder is recorded at %d, over its limit", got)
				}
			})
		}
	}

	t.Run("a refused flood changes nothing for the other tenant", func(t *testing.T) {
		q := newQuotas(t)
		victim := others["another tenant of the application"]
		if n, _ := admitN(t, q, victim, quotas.DimIngestBytesPerDay, 10, 3); n != 3 {
			t.Fatalf("setup: the victim ingested %d times", n)
		}
		before := usedOf(t, q, victim, quotas.DimIngestBytesPerDay)
		admitN(t, q, flooder, quotas.DimIngestBytesPerDay, 10, 500)
		admitN(t, q, flooder, quotas.DimRequestsPerMinute, 1, 500)
		if after := usedOf(t, q, victim, quotas.DimIngestBytesPerDay); after != before {
			t.Errorf("the victim's ingest usage moved from %d to %d during the flood", before, after)
		}
	})

	t.Run("a limit is the tenant's own", func(t *testing.T) {
		q := newQuotas(t, func(c *quotas.Config) {
			c.Overrides = []quotas.Override{{ApplicationID: "app-1", TenantID: "big", Limits: quotas.Limits{Burst: 50}}}
		})
		if n, _ := admitN(t, q, scope("app-1", "big"), quotas.DimRequestsPerMinute, 1, 60); n != 50 {
			t.Errorf("the tenant with an override was admitted %d times, want its burst 50", n)
		}
		if n, _ := admitN(t, q, flooder, quotas.DimRequestsPerMinute, 1, 60); n != int(lim.Burst) {
			t.Errorf("the override of another tenant changed this tenant's burst: admitted %d, want %d", n, lim.Burst)
		}
	})

	// The same claim against `alixai serve` itself: real tokens, the identity middleware and a quotas section.
	t.Run("on the wired serve path", servePathStarvation)

	t.Run("no scope is no tenant", func(t *testing.T) {
		q := newQuotas(t)
		if _, err := q.Admit(bg, contracts.Scope{}, quotas.DimRequestsPerMinute, 1); err == nil {
			t.Fatal("a request with no scope was admitted: it would draw on a shared default tenant")
		}
	})
}
