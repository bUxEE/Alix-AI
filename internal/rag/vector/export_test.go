package vector

// Hooks for the external test package (flat_test.go).

// RowsStmt exposes the statement every scan reads with.
var RowsStmt = rowsStmt

// CacheStats reports what the resident cache holds: entries and bytes (0, 0
// when the cache is off).
func CacheStats(f *Flat) (entries int, used int64) {
	if f.cache == nil {
		return 0, 0
	}
	f.cache.mu.Lock()
	defer f.cache.mu.Unlock()
	return len(f.cache.entries), f.cache.used
}

// CacheBytesFor reports the bytes the resident cache holds for one workspace.
func CacheBytesFor(f *Flat, application, tenant, workspace string) int64 {
	return f.cache.bytesFor(application, tenant, workspace)
}

// ResolveConfig applies the defaults NewFlat applies.
func ResolveConfig(c Config) Config { return c.withDefaults() }

// MaxCacheMiB and MaxVectors are the validation bounds.
const (
	MaxCacheMiB = maxCacheMiB
	MaxVectors  = maxVectors
)
