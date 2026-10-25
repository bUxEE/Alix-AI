package vector

import (
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
)

// newFlatIndex opens a throwaway store and a flat provider over it.
func newFlatIndex(t testing.TB, cfg Config) (*Flat, *store.DB) {
	t.Helper()
	db := storetest.Open(t, nil)
	f, err := NewFlat(db, FlatOptions{Config: cfg})
	if err != nil {
		t.Fatalf("NewFlat: %v", err)
	}
	return f, db
}

// cfgNoCache is the default configuration with the resident cache off.
func cfgNoCache() Config {
	c := DefaultConfig()
	c.CacheMiB = 0
	return c
}

// cfgCache is the default configuration with a small cache.
func cfgCache() Config {
	c := DefaultConfig()
	c.CacheMiB = 8
	return c
}
