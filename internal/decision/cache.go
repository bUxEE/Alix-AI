package decision

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strconv"
	"sync"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// CacheKey identifies one cacheable scoring. Every part that could change the answer
// or who may see it is a field, so two keys are equal only when all of them are:
// the full authorisation scope (application, tenant, workspace, principal and the
// sorted roles and capability scopes), the conversation-contract version, the model
// identity and revision, the decision type, schema version and locale, and a digest of
// the inputs the scorer sees. The tenant and application are plain fields, never only
// inside a hash, so a cache can be purged per tenant and the entry is re-checked on
// read.
type CacheKey struct {
	Application string
	Tenant      string
	Workspace   string
	Principal   string
	Grants      string

	ContractVersion string
	ModelID         string
	ModelRevision   string

	DecisionType  gen.DecisionType
	Locale        gen.Locale
	SchemaVersion int64

	Input [32]byte
}

// cacheKey builds the key for a request. Time-varying fields (built_at, deadline) and
// the session id are not part of it: they do not change what the scorer sees.
func cacheKey(scope contracts.Scope, m gen.ScorerManifest, req gen.DecisionRequest) CacheKey {
	return CacheKey{
		Application:     scope.ApplicationID,
		Tenant:          scope.TenantID,
		Workspace:       scope.WorkspaceID,
		Principal:       scope.PrincipalID,
		Grants:          grants(scope),
		ContractVersion: string(req.ContractVersion),
		ModelID:         string(m.ModelID),
		ModelRevision:   string(m.ModelRevision),
		DecisionType:    req.DecisionType,
		Locale:          req.Locale,
		SchemaVersion:   int64(req.SchemaVersion),
		Input:           inputDigest(req),
	}
}

func grants(scope contracts.Scope) string {
	roles := append([]string(nil), scope.Roles...)
	caps := append([]string(nil), scope.CapabilityScopes...)
	sort.Strings(roles)
	sort.Strings(caps)
	b := make([]byte, 0, 64)
	for _, r := range roles {
		b = strconv.AppendQuote(b, r)
		b = append(b, ' ')
	}
	b = append(b, '|')
	for _, c := range caps {
		b = strconv.AppendQuote(b, c)
		b = append(b, ' ')
	}
	return string(b)
}

func inputDigest(req gen.DecisionRequest) [32]byte {
	h := sha256.New()
	var lenBuf [8]byte
	str := func(s string) {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	opt := func(p *string) {
		if p == nil {
			str("-")
			return
		}
		str("+")
		str(*p)
	}
	str(strconv.FormatInt(req.CandidateSet.MaxSelections, 10))
	for _, c := range req.CandidateSet.Candidates {
		str(string(c.CandidateID))
		str(c.Label)
		opt(c.Text)
		if c.DataVersion == nil {
			str("-")
		} else {
			str("+")
			str(string(*c.DataVersion))
		}
	}
	str("|actions")
	for _, a := range req.AllowedNextActions {
		str(string(a.Action))
		if a.CapabilityID == nil {
			str("-")
		} else {
			str("+")
			str(string(*a.CapabilityID))
		}
	}
	str("|state")
	keys := make([]string, 0, len(req.BoundedState))
	for k := range req.BoundedState {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		str(k)
		switch v := req.BoundedState[k].(type) {
		case nil:
			str("n")
		case bool:
			str("b" + strconv.FormatBool(v))
		case string:
			str("s" + v)
		case int:
			str("f" + strconv.FormatFloat(float64(v), 'g', -1, 64))
		case int32:
			str("f" + strconv.FormatFloat(float64(v), 'g', -1, 64))
		case int64:
			str("f" + strconv.FormatFloat(float64(v), 'g', -1, 64))
		case float32:
			str("f" + strconv.FormatFloat(float64(v), 'g', -1, 64))
		case float64:
			str("f" + strconv.FormatFloat(v, 'g', -1, 64))
		default:
			str("?")
		}
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// resultCache holds provider results (never fallbacks) for a short time. It is
// bounded (FIFO eviction) and expires entries on the injected clock when they are
// read; there is no background goroutine. A hit still goes through the acceptance
// gates and the resolver, so the cache only saves the scorer call.
type resultCache struct {
	clk clock.Clock
	max int
	ttl int64 // nanoseconds

	mu    sync.Mutex
	m     map[CacheKey]*list.Element // Value is *cacheEntry
	order *list.List                 // insertion order, oldest at the front
}

type cacheEntry struct {
	key     CacheKey
	res     gen.DecisionScoreResult
	expires int64 // unix nanoseconds
}

func newResultCache(clk clock.Clock, max int, ttlNanos int64) *resultCache {
	if max <= 0 {
		return nil
	}
	return &resultCache{clk: clk, max: max, ttl: ttlNanos, m: make(map[CacheKey]*list.Element, max), order: list.New()}
}

func (c *resultCache) get(k CacheKey) (gen.DecisionScoreResult, bool) {
	if c == nil {
		return gen.DecisionScoreResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[k]
	if !ok {
		return gen.DecisionScoreResult{}, false
	}
	e := el.Value.(*cacheEntry)
	// Belt and braces: the key already carries the tenant, but a stored result that
	// names another tenant is never served.
	if c.clk.Now().UnixNano() >= e.expires || string(e.res.TenantID) != k.Tenant || string(e.res.ApplicationID) != k.Application {
		c.deleteLocked(k)
		return gen.DecisionScoreResult{}, false
	}
	return cloneResult(e.res), true
}

func (c *resultCache) put(k CacheKey, res gen.DecisionScoreResult) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := &cacheEntry{key: k, res: cloneResult(res), expires: c.clk.Now().UnixNano() + c.ttl}
	if el, ok := c.m[k]; ok {
		el.Value = e // a re-put keeps its place in the order
		return
	}
	for len(c.m) >= c.max && c.order.Len() > 0 {
		c.deleteLocked(c.order.Front().Value.(*cacheEntry).key)
	}
	c.m[k] = c.order.PushBack(e)
}

func (c *resultCache) deleteLocked(k CacheKey) {
	if el, ok := c.m[k]; ok {
		c.order.Remove(el)
		delete(c.m, k)
	}
}

func (c *resultCache) purgeTenant(application, tenant string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		if k.Application == application && k.Tenant == tenant {
			c.deleteLocked(k)
		}
	}
}

func (c *resultCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func cloneResult(r gen.DecisionScoreResult) gen.DecisionScoreResult {
	r.Selections = append([]gen.DecisionScoreResultSelection(nil), r.Selections...)
	if r.ModelID != nil {
		v := *r.ModelID
		r.ModelID = &v
	}
	if r.ModelRevision != nil {
		v := *r.ModelRevision
		r.ModelRevision = &v
	}
	if r.FallbackReason != nil {
		v := *r.FallbackReason
		r.FallbackReason = &v
	}
	return r
}
