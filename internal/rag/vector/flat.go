package vector

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"log/slog"
	"runtime/debug"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

//go:embed migrations/*.sql
var migrations embed.FS

// MigrationsModule is the name the flat index's migrations are registered
// under.
const MigrationsModule = "ragvector"

func init() { store.RegisterMigrations(MigrationsModule, migrations) }

// FlatName is Info.Name of the flat provider.
const FlatName = "flat"

// FlatOptions is everything NewFlat needs besides the database.
type FlatOptions struct {
	// Config sizes the cache and the ceilings; see DefaultConfig. A zero
	// Config means the defaults.
	Config Config
	// Clock stamps generations. Nil means clock.Real.
	Clock clock.Clock
	// Logger receives index events (counts and ids, never vectors or text).
	// Nil discards.
	Logger *slog.Logger
}

// Flat is the embedded VectorIndexProvider: an exact flat scan over int8 codes
// held in two SQLite tables of the core store (ADR 0011, option A). A search
// reads exactly the key range of the requested generations of the caller's
// scope: the scope is the leading part of the primary key, so there is no
// global candidate set to post-filter. An optional resident cache keeps hot
// generations in memory.
//
// A Flat is safe for concurrent use. Its migrations ("ragvector") must be in
// the registry the database was opened with; importing this package registers
// them in store.Default.
type Flat struct {
	db    *store.DB
	cfg   Config
	clk   clock.Clock
	log   *slog.Logger
	cache *residentCache
}

var _ VectorIndexProvider = (*Flat)(nil)

// NewFlat returns the flat provider over db.
func NewFlat(db *store.DB, opts FlatOptions) (*Flat, error) {
	if db == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "vector: nil store")
	}
	if opts.Config == (Config{}) {
		opts.Config = DefaultConfig()
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "vector: invalid configuration")
	}
	f := &Flat{db: db, cfg: opts.Config.withDefaults(), clk: opts.Clock, log: opts.Logger}
	if f.clk == nil {
		f.clk = clock.Real{}
	}
	if f.log == nil {
		f.log = slog.New(slog.DiscardHandler)
	}
	if f.cfg.CacheMiB > 0 {
		f.cache = newResidentCache(int64(f.cfg.CacheMiB) << 20)
	}
	return f, nil
}

// Info implements VectorIndexProvider.
func (f *Flat) Info() Info {
	return Info{Name: FlatName, Exact: true, Metric: "cosine", Storage: "int8", Reuses: true, StableTies: true}
}

// genInfo is a vector_generations row.
type genInfo struct {
	source  string
	model   string
	dim     int
	vectors int
	rev     int64
	// dropping: the generation is being deleted and no longer exists for any
	// caller; only the delete itself still touches its rows.
	dropping bool
}

const genColumns = "source_id, model_id, dim, vectors, rev, state"

// loadGen reads one generation of the scope. ok is false when there is none.
func loadGen(ctx context.Context, q store.Reader, scope contracts.Scope, generation string) (g genInfo, ok bool, err error) {
	row, err := store.QueryRowScoped(ctx, q, scope, store.ScopedStmt{
		SQL:       "SELECT " + genColumns + " FROM vector_generations",
		Where:     "generation_id = ?",
		WhereArgs: []any{generation},
	})
	if err != nil {
		return genInfo{}, false, err
	}
	var state string
	switch err := row.Scan(&g.source, &g.model, &g.dim, &g.vectors, &g.rev, &state); {
	case errors.Is(err, sql.ErrNoRows):
		return genInfo{}, false, nil
	case err != nil:
		return genInfo{}, false, err
	}
	g.dropping = state == stateDropping
	return g, true, nil
}

const stateDropping = "dropping"

const (
	// idHint is the chunk id length the resident load reserves room for; real
	// ids (chk_ plus a hash) are longer, and the buffer grows when they are.
	idHint = 48
	// freeAfterLoad is the resident entry size from which a load returns its
	// garbage to the OS.
	freeAfterLoad = 8 << 20
)

// Search implements VectorIndexProvider.
func (f *Flat) Search(ctx context.Context, scope contracts.Scope, q Query) (SearchResult, error) {
	if err := scope.Validate(); err != nil {
		return SearchResult{}, err
	}
	if err := q.Validate(); err != nil {
		return SearchResult{}, err
	}
	uq, err := UnitQuery(q.Vector)
	if err != nil {
		return SearchResult{}, err
	}
	gens := Dedupe(q.Generations)
	if len(gens) == 0 {
		return SearchResult{}, nil // nothing authorised: nothing returned
	}

	var (
		res   SearchResult
		infos []genInfo
		tk    *topK
	)
	err = f.db.View(ctx, func(tx *store.Tx) error {
		// View may run fn again after a busy error: start clean.
		res, infos, tk = SearchResult{}, infos[:0], newTopK(q.K)
		for gi, g := range gens {
			info, ok, err := loadGen(ctx, tx, scope, g)
			if err != nil {
				return err
			}
			infos = append(infos, info)
			switch {
			case !ok || info.dropping:
				res.Skipped = append(res.Skipped, Skipped{g, SkipUnknownGeneration})
				continue
			case info.model != q.Model:
				res.Skipped = append(res.Skipped, Skipped{g, SkipModelMismatch})
				continue
			case info.dim != len(uq):
				res.Skipped = append(res.Skipped, Skipped{g, SkipDimMismatch})
				continue
			}
			n, err := f.scanGeneration(ctx, tx, scope, g, gi, info, uq, tk)
			if err != nil {
				return err
			}
			res.Scanned += n
		}
		return nil
	})
	if err != nil {
		return SearchResult{}, err
	}
	for _, c := range tk.sorted() {
		res.Hits = append(res.Hits, Hit{
			ChunkID:      c.id,
			GenerationID: gens[c.gen],
			SourceID:     infos[c.gen].source,
			Score:        c.score,
		})
	}
	return res, nil
}

// scanGeneration scores one generation into tk, from the resident cache when
// it is enabled, fits and current, otherwise by streaming the rows.
func (f *Flat) scanGeneration(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation string, gi int, info genInfo, uq []float32, tk *topK) (int, error) {
	if f.cache.fits(info.vectors, info.dim) {
		key := cacheKey{scope.ApplicationID, scope.TenantID, scope.WorkspaceID, generation}
		e, err := f.cache.load(key, info.rev, residentBytes(info.vectors, info.dim), func() (*resident, error) {
			return f.loadResident(ctx, tx, scope, generation, info)
		})
		if err != nil {
			return 0, err
		}
		if e != nil {
			return scanResident(e, uq, gi, tk), nil
		}
		// The cache declined (its entries are still earning their place): stream.
	}
	return f.streamGeneration(ctx, tx, scope, generation, gi, info, uq, tk)
}

func scanResident(e *resident, uq []float32, gi int, tk *topK) int {
	dim := e.dim
	for i := 0; i < e.n; i++ {
		score := dot(uq, e.codes[i*dim:(i+1)*dim]) * e.inv[i]
		if len(tk.items) == tk.k && score < tk.items[0].score {
			continue
		}
		tk.offer(score, e.id(i), gi)
	}
	return e.n
}

func (f *Flat) streamGeneration(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation string, gi int, info genInfo, uq []float32, tk *topK) (int, error) {
	rows, err := queryRows(ctx, tx, scope, generation)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var (
		id, code sql.RawBytes
		inv      float64
		n        int
	)
	for rows.Next() {
		if err := rows.Scan(&id, &inv, &code); err != nil {
			return 0, err
		}
		if len(code) != info.dim {
			return 0, corrupt(generation, "a stored vector has the wrong length")
		}
		score := dot(uq, code) * float32(inv)
		if !(len(tk.items) == tk.k && score < tk.items[0].score) {
			tk.offer(score, id, gi)
		}
		n++
	}
	return n, rows.Err()
}

// loadResident reads a whole generation into memory, in the caller's snapshot,
// so the entry is consistent with the rev it is cached under.
func (f *Flat) loadResident(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation string, info genInfo) (*resident, error) {
	rows, err := queryRows(ctx, tx, scope, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	e := &resident{
		dim:   info.dim,
		codes: make([]byte, 0, info.vectors*info.dim),
		inv:   make([]float32, 0, info.vectors),
		idEnd: make([]uint32, 0, info.vectors),
		ids:   make([]byte, 0, info.vectors*idHint),
	}
	var (
		id, code sql.RawBytes
		inv      float64
	)
	for rows.Next() {
		if err := rows.Scan(&id, &inv, &code); err != nil {
			return nil, err
		}
		if len(code) != info.dim {
			return nil, corrupt(generation, "a stored vector has the wrong length")
		}
		e.codes = append(e.codes, code...)
		e.inv = append(e.inv, float32(inv))
		e.ids = append(e.ids, id...)
		e.idEnd = append(e.idEnd, uint32(len(e.ids)))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	e.n = len(e.inv)
	e.size = int64(cap(e.codes)) + int64(cap(e.inv))*4 + int64(cap(e.idEnd))*4 + int64(cap(e.ids))
	if e.size >= freeAfterLoad {
		// Streaming a large generation through database/sql leaves about as
		// much garbage as the entry itself, and the Go heap keeps it resident
		// until the next collection cycle. Give it back once, now: a load is a
		// rare, already slow event, and the steady RSS is the cost the 4 GB
		// profile counts (docs/efficiency/M2-07.md).
		debug.FreeOSMemory()
	}
	return e, nil
}

// rowsStmt is the statement every scan reads vectors with: the scope predicate
// first, then the generation, so SQLite reads one contiguous range of the
// primary key (TestScanReadsOnlyTheScopeRange asserts the plan).
func rowsStmt(scope contracts.Scope, generation string) (string, []any, error) {
	return store.ScopedStmt{
		SQL:       "SELECT chunk_id, inv_norm, code FROM vector_rows",
		Where:     "generation_id = ?",
		WhereArgs: []any{generation},
	}.Build(scope)
}

func queryRows(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation string) (*sql.Rows, error) {
	query, args, err := rowsStmt(scope, generation)
	if err != nil {
		return nil, err
	}
	return tx.QueryContext(ctx, query, args...)
}

func corrupt(generation, what string) error {
	return contracts.Newf(contracts.ErrInternal, "vector index: generation %q is inconsistent: %s", generation, what)
}
