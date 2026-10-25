package vector_test

import (
	"testing"

	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/rag/vector/vectortest"
	"github.com/buxee/alix-ai/internal/store/storetest"
)

// SEC-021, SEC-023: TestProviderContractSuite runs the reusable contract against the embedded
// flat provider, streaming (cache off) and from the resident cache (cache on):
// the two read paths must be indistinguishable.
func TestProviderContractSuite(t *testing.T) {
	for name, mutate := range map[string]func(*vector.Config){
		"streamed": func(c *vector.Config) { c.CacheMiB = 0 },
		"resident": func(c *vector.Config) { c.CacheMiB = 16 },
	} {
		t.Run(name, func(t *testing.T) {
			vectortest.RunProviderSuite(t, func(t *testing.T) vector.VectorIndexProvider {
				cfg := vector.DefaultConfig()
				mutate(&cfg)
				f, err := vector.NewFlat(storetest.Open(t, nil), vector.FlatOptions{Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				return f
			})
		})
	}
}
