package index

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// keyAlgorithm names the shape of the cache key and the behaviour of the query
// engine behind it. Changing how queries are parsed or ranked changes it, so
// entries made by the older behaviour are never served.
const keyAlgorithm = "ragindex-cache-key/v1"

// Version is the state of a scope's index as one read snapshot saw it: the
// scope's epoch, which moves on every change that can alter what a query
// returns (a document put into the active generation, a deletion, a tombstone,
// an activation, an invalidation), and the generation serving each source.
// It is the "source version" of a cache key (master plan 10.2).
type Version struct {
	Epoch int64
	// Active lists "<source id>=<generation id>" for every source with a
	// serving generation, sorted.
	Active []string
}

func readVersion(ctx context.Context, r store.Reader, scope contracts.Scope, sc scopeRow) (Version, error) {
	v := Version{Epoch: sc.epoch}
	rows, err := store.QueryScoped(ctx, r, scope, store.ScopedStmt{
		SQL:   `SELECT source_id, generation_id FROM ragindex_generation`,
		Where: `state = 'active'`,
		Tail:  `ORDER BY source_id`,
	})
	if err != nil {
		return Version{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var src, gen string
		if err := rows.Scan(&src, &gen); err != nil {
			return Version{}, err
		}
		v.Active = append(v.Active, src+"="+gen)
	}
	return v, rows.Err()
}

// Version reads the scope's current version. A scope that never indexed
// anything has the zero Version.
func (ix *Index) Version(ctx context.Context, scope contracts.Scope) (Version, error) {
	if err := validateScope(scope); err != nil {
		return Version{}, err
	}
	var v Version
	err := ix.db.View(ctx, func(tx *store.Tx) error {
		sc, found, err := lookupScope(ctx, tx, scope)
		if err != nil || !found {
			return err
		}
		v, err = readVersion(ctx, tx, scope, sc)
		return err
	})
	return v, err
}

// CacheKey is the key a retrieval, evidence, query-embedding, citation-preview
// or prompt-assembly cache must use for an entry derived from this index
// (SEC-022, master plan 10.2). It binds the entry to:
//
//   - the namespace, so different kinds of entry never collide;
//   - the application, tenant and workspace, and the principal, its roles and
//     its capability scopes: the full authorisation scope, so an entry made for
//     one principal is never served to another, nor to the same principal
//     after a role changed;
//   - the index version, so any change to the indexed content or to the
//     serving generation of a source gives a new key and the old entry is never
//     hit again;
//   - the caller's material (the query text and options; Query.Material).
//
// The session is deliberately not part of it: it carries no authority. Every
// field is length-prefixed before hashing, so no two different inputs can
// produce the same byte string. Take the Version from the same snapshot as the
// result being cached (Result.Version), never from a separate read.
func CacheKey(scope contracts.Scope, namespace string, v Version, material ...string) (string, error) {
	if err := validateScope(scope); err != nil {
		return "", err
	}
	if namespace == "" {
		return "", invalidArg("cache namespace is empty")
	}
	h := sha256.New()
	put := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	putList := func(l []string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(l)))
		h.Write(n[:])
		for _, s := range l {
			put(s)
		}
	}
	sorted := func(l []string) []string {
		c := append([]string(nil), l...)
		sort.Strings(c)
		return c
	}
	put(keyAlgorithm)
	put(namespace)
	put(scope.ApplicationID)
	put(scope.TenantID)
	put(scope.WorkspaceID)
	put(scope.PrincipalID)
	putList(sorted(scope.Roles))
	putList(sorted(scope.CapabilityScopes))
	var e [8]byte
	binary.BigEndian.PutUint64(e[:], uint64(v.Epoch))
	h.Write(e[:])
	putList(sorted(v.Active))
	putList(material)
	return "rik1_" + hex.EncodeToString(h.Sum(nil)), nil
}
