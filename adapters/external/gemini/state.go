package gemini

import (
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// The API does not always label a function call with an id, and a thinking
// model attaches an opaque thoughtSignature to the part that holds a call. A
// proposal handed to the caller has to carry an id, and a later request that
// echoes the proposal should carry the signature back. Both are bookkeeping of
// this adapter, not data of the caller:
//
//   - a missing or unusable id gets a synthesised one, "gem_" + the response id
//     + "_" + the call's index. Such an id is never sent back to the API (the
//     call and its result are paired by name and order there);
//   - the signature is kept in a small bounded in-memory map keyed by tenant and
//     call id, and put back on the call part when the same proposal is echoed.
//     It is an opaque value, never logged or returned, and a restart or an
//     eviction only loses it, in which case the call goes without a signature
//     (a live risk for the newest models, owed to X-H02).

const (
	sigCacheSize = 256
	synthPrefix  = "gem_"
)

var (
	callIDRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	responseIDRE  = regexp.MustCompile(`[^A-Za-z0-9]`)
	maxSignatures = 8 << 10
)

type sigCache struct {
	mu    sync.Mutex
	cap   int
	order []string
	m     map[string]string
}

func newSigCache(n int) *sigCache { return &sigCache{cap: n, m: map[string]string{}} }

func sigKey(scope contracts.Scope, id string) string { return scope.TenantID + "\x00" + id }

func (s *sigCache) put(scope contracts.Scope, id, sig string) {
	if sig == "" || len(sig) > maxSignatures {
		return
	}
	k := sigKey(scope, id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[k]; !ok {
		if len(s.order) >= s.cap {
			delete(s.m, s.order[0])
			s.order = s.order[1:]
		}
		s.order = append(s.order, k)
	}
	s.m[k] = sig
}

func (s *sigCache) get(scope contracts.Scope, id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[sigKey(scope, id)]
}

// idSource mints the response-scoped part of a synthesised call id when the
// response carries no id of its own.
type idSource struct{ n atomic.Uint64 }

// callID returns the id of call number idx of a response.
func (s *idSource) callID(provided, responseID string, idx int) string {
	if provided != "" && callIDRE.MatchString(provided) && len(provided) <= 100 && len(provided) >= 1 && provided[:min(len(provided), len(synthPrefix))] != synthPrefix {
		return provided
	}
	rid := responseIDRE.ReplaceAllString(responseID, "")
	if len(rid) > 40 {
		rid = rid[:40]
	}
	if rid == "" {
		rid = "n" + strconv.FormatUint(s.n.Add(1), 36)
	}
	return synthPrefix + rid + "_" + strconv.Itoa(idx)
}

func synthesised(id string) bool {
	return len(id) >= len(synthPrefix) && id[:len(synthPrefix)] == synthPrefix
}
