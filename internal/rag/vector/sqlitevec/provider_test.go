package sqlitevec_test

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/rag/vector/sqlitevec"
	"github.com/buxee/alix-ai/internal/rag/vector/vectortest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/store/vec"
)

var bg = context.Background()

// newProvider opens a store whose registry holds ONLY the sqlite-vec module: the
// default registry never contains it.
func newProvider(t testing.TB) (*sqlitevec.Provider, *store.DB) {
	t.Helper()
	reg := store.NewRegistry()
	if err := vec.Register(reg); err != nil {
		t.Fatalf("vec.Register: %v", err)
	}
	db := storetest.Open(t, reg)
	p, err := sqlitevec.New(bg, db, sqlitevec.Options{})
	if err != nil {
		t.Fatalf("sqlitevec.New: %v", err)
	}
	return p, db
}

// SEC-021, SEC-023: TestProviderContractSuite: the sqlite-vec candidate passes the same contract
// as the embedded flat provider, at the one dimension it serves.
func TestProviderContractSuite(t *testing.T) {
	vectortest.RunProviderSuite(t, func(t *testing.T) vector.VectorIndexProvider {
		p, _ := newProvider(t)
		return p
	}, vectortest.WithDim(vec.Dim), vectortest.WithCorpus(1000))
}

func TestExtensionIsLoadedAndSchemaScoped(t *testing.T) {
	_, db := newProvider(t)
	v, err := vec.Version(bg, db)
	if err != nil || v == "" {
		t.Fatalf("vec.Version = %q, %v", v, err)
	}
	t.Logf("sqlite-vec %s", v)
	// vec0's own shadow table has no scope columns; everything else must.
	storetest.AssertScopedTables(t, db, vec.ShadowTables()...)
}

// New fails closed when the schema is not there (the composition root forgot to
// register the module) rather than on the first query.
func TestNewRefusesAMissingSchema(t *testing.T) {
	db := storetest.Open(t, store.NewRegistry())
	if _, err := sqlitevec.New(bg, db, sqlitevec.Options{}); err == nil {
		t.Fatal("New accepted a store without the sqlite-vec schema")
	}
}

// vec0 zeroes a deleted vector in place, so the bytes do not linger in the
// extension's own chunk table.
func TestDeletedVectorIsErasedFromTheShadowTable(t *testing.T) {
	p, db := newProvider(t)
	r := vectortest.Rand(7)
	scope := vectortest.Scope("app", "tenA", "ws1")
	items := vectortest.Items(scope, "c", vectortest.Clustered(r, 6, vec.Dim, 2, 0.5))
	if _, err := p.Upsert(bg, scope, vector.UpsertBatch{Generation: "g1", SourceID: "s1", Model: vectortest.Model, Items: items}); err != nil {
		t.Fatal(err)
	}
	code, _, err := vector.Quantise(items[2].Vector)
	if err != nil {
		t.Fatal(err)
	}
	has := func() bool {
		var blob []byte
		if err := db.QueryRowContext(bg, `SELECT vectors FROM vecb_vectors_vector_chunks00 LIMIT 1`).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		return contains(blob, code)
	}
	if !has() {
		t.Fatal("setup: the stored vector is not in the shadow table")
	}
	if n, err := p.DeleteChunks(bg, scope, "g1", []string{items[2].ChunkID}); err != nil || n != 1 {
		t.Fatalf("DeleteChunks = %d, %v", n, err)
	}
	if has() {
		t.Fatal("the deleted vector's bytes are still in vec0's chunk table")
	}
}

func contains(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
