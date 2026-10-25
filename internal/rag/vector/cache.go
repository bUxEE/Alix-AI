package vector

import (
	"sort"
	"sync"
)

// cacheKey identifies one generation of one scope. The whole scope is part of
// the key: a cache keyed by anything shorter could serve one tenant's vectors
// to another.
type cacheKey struct {
	application, tenant, workspace, generation string
}

// resident is one generation held in memory: the int8 codes of every vector
// back to back, their reciprocal norms and their chunk ids. It is immutable
// once published, so searches read it without locking.
type resident struct {
	rev   int64
	dim   int
	n     int
	codes []byte    // n*dim
	inv   []float32 // n
	idEnd []uint32  // end offset of each chunk id in idData
	ids   []byte
	size  int64  // bytes held, for the budget
	last  uint64 // tick of the last search this entry served; guarded by residentCache.mu
}

// maxAsked bounds the request memory of the cache (see admit): past it, the
// keys of generations that are not resident are forgotten.
const maxAsked = 1 << 14

func (r *resident) id(i int) []byte {
	start := uint32(0)
	if i > 0 {
		start = r.idEnd[i-1]
	}
	return r.ids[start:r.idEnd[i]]
}

// residentBytes estimates what a generation of n vectors will occupy, with a
// pessimistic 64-byte chunk id, so a generation that cannot fit the budget is
// streamed without first being loaded and thrown away.
func residentBytes(n, dim int) int64 {
	return int64(n) * int64(dim+4+4+64)
}

// residentCache is a byte-budgeted LRU of resident generations. It is a
// latency dial, never a source of truth: an entry is used only when its rev
// equals the generation's rev read in the same snapshot as the scan, so a
// stale entry is unreachable, and a budget of zero turns the cache off.
//
// It must never be worse than no cache. Loading a generation costs about one
// streamed scan on top of the scan itself, so generations that do not all fit
// would, with plain LRU, evict each other on every search (measured: two
// 100,000-vector workspaces in a 64 MiB budget ran twice as slow as with the
// cache off). Admission prevents that: a generation that needs room is loaded
// only when every entry it would displace has not been used since the
// generation's own previous request (see admit); otherwise it is streamed,
// which is what it would do with the cache off.
type residentCache struct {
	budget int64

	mu      sync.Mutex
	used    int64
	tick    uint64
	entries map[cacheKey]*resident
	asked   map[cacheKey]uint64      // tick of each generation's latest request, resident or not
	loaders map[cacheKey]*sync.Mutex // one per key: concurrent first searches share one load
}

func newResidentCache(budgetBytes int64) *residentCache {
	return &residentCache{
		budget:  budgetBytes,
		entries: map[cacheKey]*resident{},
		asked:   map[cacheKey]uint64{},
		loaders: map[cacheKey]*sync.Mutex{},
	}
}

func (c *residentCache) enabled() bool { return c != nil && c.budget > 0 }

// fits reports whether a generation of n vectors of dim dimensions could be
// cached at all.
func (c *residentCache) fits(n, dim int) bool {
	return c.enabled() && residentBytes(n, dim) <= c.budget
}

// get returns the entry for k when it is current (rev equal), else nil.
func (c *residentCache) get(k cacheKey, rev int64) *resident {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hitLocked(k, rev)
}

func (c *residentCache) hitLocked(k cacheKey, rev int64) *resident {
	e := c.entries[k]
	if e == nil || e.rev != rev {
		return nil
	}
	c.tick++
	e.last = c.tick
	c.asked[k] = c.tick
	return e
}

// load returns the current entry, building it with build when there is none.
// build runs with the key's loader locked. It returns nil, nil when the cache
// declines to hold a generation of want bytes (see admit): the caller streams.
// The result is cached only when it fits the budget after evicting the least
// recently used others.
func (c *residentCache) load(k cacheKey, rev int64, want int64, build func() (*resident, error)) (*resident, error) {
	c.mu.Lock()
	l := c.loaders[k]
	if l == nil {
		l = &sync.Mutex{}
		c.loaders[k] = l
	}
	c.mu.Unlock()

	l.Lock()
	defer l.Unlock()
	if e := c.get(k, rev); e != nil {
		return e, nil
	}
	if !c.admit(k, want) {
		return nil, nil
	}
	e, err := build()
	if err != nil {
		return nil, err
	}
	e.rev = rev
	c.store(k, e)
	return e, nil
}

func (c *residentCache) store(k cacheKey, e *resident) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.entries[k]; old != nil {
		c.used -= old.size
		delete(c.entries, k)
	}
	if e.size > c.budget {
		return
	}
	for c.used+e.size > c.budget {
		victim, ok := c.lruLocked()
		if !ok {
			return
		}
		c.used -= c.entries[victim].size
		delete(c.entries, victim)
	}
	c.tick++
	e.last = c.tick
	c.asked[k] = c.tick
	c.entries[k] = e
	c.used += e.size
}

// admit reports whether a generation of want bytes may be loaded. It may when
// there is room, or when every entry it would have to evict (least recently
// used first) has not served a search since this generation last asked. So the
// first request that needs room is always refused and streamed; an incumbent
// that is still being searched keeps its place against any number of rivals;
// and a rival that keeps asking replaces an incumbent that has gone cold. With
// more hot generations than the budget holds, one stays resident and the rest
// stream, which is the cache-off speed, never worse.
func (c *residentCache) admit(k cacheKey, want int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tick++
	prev := c.asked[k] // 0 when it has never asked: nothing is older than that
	c.asked[k] = c.tick
	if len(c.asked) > maxAsked {
		kept := make(map[cacheKey]uint64, len(c.entries)+1)
		for other, e := range c.entries {
			kept[other] = e.last
		}
		kept[k] = c.tick
		c.asked = kept
	}
	free := c.budget - c.used
	if old := c.entries[k]; old != nil {
		free += old.size // a stale entry of the same generation is replaced, not displaced
	}
	if free >= want {
		return true
	}
	victims := make([]cacheKey, 0, len(c.entries))
	for other := range c.entries {
		if other != k {
			victims = append(victims, other)
		}
	}
	sort.Slice(victims, func(i, j int) bool { return c.entries[victims[i]].last < c.entries[victims[j]].last })
	for _, v := range victims {
		if free >= want {
			break
		}
		e := c.entries[v]
		if e.last >= prev { // used since this generation last asked: it keeps its place
			return false
		}
		free += e.size
	}
	return free >= want
}

// lruLocked picks the least recently used cached key.
func (c *residentCache) lruLocked() (cacheKey, bool) {
	var (
		best  cacheKey
		bestT uint64
		found bool
	)
	for k, e := range c.entries {
		if !found || e.last < bestT {
			best, bestT, found = k, e.last, true
		}
	}
	return best, found
}

// drop forgets one generation. Writers call it after a delete so the memory is
// released at once; correctness never depends on it (see rev).
func (c *residentCache) drop(k cacheKey) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[k]; e != nil {
		c.used -= e.size
		delete(c.entries, k)
	}
	delete(c.asked, k)
	delete(c.loaders, k)
}

// dropScope forgets every generation of one scope.
func (c *residentCache) dropScope(application, tenant, workspace string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if k.application == application && k.tenant == tenant && k.workspace == workspace {
			c.used -= e.size
			delete(c.entries, k)
		}
	}
	for k := range c.loaders {
		if k.application == application && k.tenant == tenant && k.workspace == workspace {
			delete(c.loaders, k)
		}
	}
	for k := range c.asked {
		if k.application == application && k.tenant == tenant && k.workspace == workspace {
			delete(c.asked, k)
		}
	}
}

// bytesFor reports the memory the cache holds for one scope.
func (c *residentCache) bytesFor(application, tenant, workspace string) int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for k, e := range c.entries {
		if k.application == application && k.tenant == tenant && k.workspace == workspace {
			n += e.size
		}
	}
	return n
}
