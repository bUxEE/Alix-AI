package answer

import (
	"context"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// refPrefix is the ID prefix of a citation reference: it is a file-kind open_ref
// (conversation.CitationOpenRef), an opaque scoped reference that is not a path
// and never resolves to one.
const refPrefix = "fil"

// refRecord is what the resolver remembers about one issued reference.
type refRecord struct {
	application, tenant, workspace, principal string

	sourceID actions.SourceID
	version  actions.OpaqueVersion
	locator  *conversation.CitationLocator
	preview  string
	expires  time.Time
}

// RefSpec is what a reference is issued for.
type RefSpec struct {
	SourceID actions.SourceID
	Version  actions.OpaqueVersion
	Locator  *conversation.CitationLocator
	// Preview is the cited passage, shown when the reference is resolved. It is
	// bounded by Config.PreviewRunes.
	Preview string
}

// Resolution is what a reference resolves to, read at the moment it is resolved.
type Resolution struct {
	SourceID    actions.SourceID
	DisplayName string
	Version     actions.OpaqueVersion
	Locator     *conversation.CitationLocator
	// Availability and Staleness are the source's state now, not at retrieval.
	Availability conversation.CitationAvailability
	Staleness    conversation.CitationStaleness
	// Open is the way to open the source itself: the source catalog's reference
	// (a record of the host application, a file) or, when it has none, the source
	// resolved by its ID. It is an opaque reference, never a URL or a path.
	Open conversation.CitationOpenRef
	// Preview is the passage the citation was grounded on, as it was cited. It
	// may be older than the source: Staleness says so.
	Preview   string
	ExpiresAt time.Time
}

// Resolver issues and resolves the open/preview references of citations
// (master plan 12.5: a permission-checked open/preview reference). A reference is
// an opaque, random file-kind ID bound, when it is issued, to the application,
// tenant, workspace and principal the answer was written for, to the source and
// version cited, and to an expiry from the injected clock. Resolving it
//
//  1. looks the ID up: an ID never issued, or one issued to another scope, is
//     refused with the same permission_denied, so a caller learns nothing about
//     what exists (SEC-022);
//  2. refuses an expired reference with expired, only to the scope it belongs to;
//  3. asks the source catalog, fresh, whether the principal may still read the
//     source: a source revoked or removed since the answer is refused with
//     permission_denied, however recently the reference was issued.
//
// The catalog is the authority; a reference is a pointer to a decision, never a
// grant. A failing catalog is an unavailable error, never a pass.
//
// References live in memory only, bounded by Config.MaxRefs (oldest first) and
// Config.RefTTL: a restart invalidates them, which is the safe direction. The
// resolver is safe for concurrent use.
type Resolver struct {
	cat query.SourceCatalog
	clk clock.Clock
	gen *ids.Generator

	ttl     time.Duration
	maxRefs int
	preview int

	mu   sync.Mutex
	recs map[string]*refRecord
	fifo []string // IDs in issue order; the TTL is uniform, so it is also expiry order
	head int
}

// NewResolver returns a Resolver. cat is required: without it a reference
// could not be permission-checked.
func NewResolver(cat query.SourceCatalog, clk clock.Clock, cfg Config) (*Resolver, error) {
	if cat == nil {
		return nil, contracts.New(contracts.ErrInternal, "answer: no source catalog: citation references cannot be permission-checked")
	}
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "answer: invalid configuration")
	}
	if clk == nil {
		clk = clock.Real{}
	}
	return &Resolver{
		cat: cat, clk: clk, gen: ids.NewGenerator(clk),
		ttl: cfg.RefTTL, maxRefs: cfg.MaxRefs, preview: cfg.PreviewRunes,
		recs: make(map[string]*refRecord),
	}, nil
}

// errDenied is the one answer for every reference that does not resolve for the
// caller: unknown, issued to another scope, tampered with, or its source no longer
// readable. Its text is constant so the error cannot tell them apart.
func errDenied() error {
	return contracts.New(contracts.ErrPermissionDenied, "answer: this citation reference is not available to the caller")
}

// Issue records a reference for the scope and returns it with its expiry. The scope
// must be valid: no reference is ever issued without one.
func (r *Resolver) Issue(scope contracts.Scope, in RefSpec) (conversation.CitationOpenRef, time.Time, error) {
	if err := scope.Validate(); err != nil {
		return conversation.CitationOpenRef{}, time.Time{}, err
	}
	if in.SourceID == "" || in.Version == "" {
		return conversation.CitationOpenRef{}, time.Time{}, contracts.New(contracts.ErrValidationFailed, "answer: a reference needs a source and a version")
	}
	now := r.clk.Now()
	rec := &refRecord{
		application: scope.ApplicationID, tenant: scope.TenantID, workspace: scope.WorkspaceID, principal: scope.PrincipalID,
		sourceID: in.SourceID, version: in.Version, locator: cloneLocator(in.Locator),
		expires: now.Add(r.ttl),
	}
	if r.preview > 0 {
		rec.preview = clipRunes(in.Preview, r.preview)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked(now)
	id := r.gen.New(refPrefix)
	r.recs[id] = rec
	r.fifo = append(r.fifo, id)
	ref := conversation.CitationOpenRef{Value: conversation.CitationOpenRefFile{Kind: "file", FileID: actions.FileID(id)}}
	return ref, rec.expires, nil
}

// evictLocked drops the references that expired, then the oldest while the table is
// at its bound, so one more fits.
func (r *Resolver) evictLocked(now time.Time) {
	for r.head < len(r.fifo) {
		id := r.fifo[r.head]
		rec, ok := r.recs[id]
		if ok && now.Before(rec.expires) && len(r.recs) < r.maxRefs {
			break
		}
		delete(r.recs, id)
		r.head++
	}
	if r.head > 1024 && r.head*2 > len(r.fifo) { // compact the queue
		r.fifo = append([]string(nil), r.fifo[r.head:]...)
		r.head = 0
	}
}

// Len is the number of references currently remembered, expired ones included until
// they are evicted.
func (r *Resolver) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recs)
}

// Resolve resolves a reference for the caller. See Resolver for the order of the
// checks. The error never names the reference or the source.
func (r *Resolver) Resolve(ctx context.Context, scope contracts.Scope, fileID actions.FileID) (Resolution, error) {
	if err := scope.Validate(); err != nil {
		return Resolution{}, err
	}
	r.mu.Lock()
	rec, ok := r.recs[string(fileID)]
	var cp refRecord
	if ok {
		cp = *rec
	}
	r.mu.Unlock()
	if !ok || cp.application != scope.ApplicationID || cp.tenant != scope.TenantID ||
		cp.workspace != scope.WorkspaceID || cp.principal != scope.PrincipalID {
		return Resolution{}, errDenied()
	}
	if !r.clk.Now().Before(cp.expires) {
		return Resolution{}, contracts.New(contracts.ErrExpired, "answer: this citation reference has expired")
	}

	sid := string(cp.sourceID)
	states, err := r.cat.Sources(ctx, scope, []string{sid})
	if err != nil {
		return Resolution{}, contracts.Wrap(contracts.ErrUnavailable, err, "answer: source catalog unavailable: the reference was not resolved")
	}
	st, ok := states[sid]
	if !ok || !st.Authorised || st.Availability == conversation.CitationAvailabilityRemoved {
		return Resolution{}, errDenied()
	}
	open := conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}}
	if st.OpenRef != nil {
		open = *st.OpenRef
	}
	name := st.DisplayName
	if name == "" {
		name = sid
	}
	return Resolution{
		SourceID: cp.sourceID, DisplayName: name, Version: cp.version, Locator: cloneLocator(cp.locator),
		Availability: availabilityOf(st.Availability), Staleness: stalenessOf(st.Staleness),
		Open: open, Preview: cp.preview, ExpiresAt: cp.expires,
	}, nil
}

// availabilityOf reads a state's availability: the zero value, or anything not
// listed, is unavailable. Nothing is claimed as checked that was not.
func availabilityOf(a conversation.CitationAvailability) conversation.CitationAvailability {
	if a.Valid() {
		return a
	}
	return conversation.CitationAvailabilityUnavailable
}

// stalenessOf reads a state's staleness: the zero value, or anything not listed,
// is unknown.
func stalenessOf(s conversation.CitationStaleness) conversation.CitationStaleness {
	if s.Valid() {
		return s
	}
	return conversation.CitationStalenessUnknown
}

func cloneLocator(l *conversation.CitationLocator) *conversation.CitationLocator {
	if l == nil {
		return nil
	}
	c := *l
	if l.Page != nil {
		v := *l.Page
		c.Page = &v
	}
	if l.Section != nil {
		v := *l.Section
		c.Section = &v
	}
	if l.Record != nil {
		v := *l.Record
		c.Record = &v
	}
	if l.Span != nil {
		v := *l.Span
		c.Span = &v
	}
	return &c
}
