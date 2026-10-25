package query

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// finalRecheck is the final authorisation and version recheck of master plan
// 12.4, run just before the package is built, in two independent reads:
//
//   - index.Recheck, in a fresh snapshot: the chunk is still in the caller's
//     scope, not tombstoned, in the generation that serves its source, hidden
//     text excluded, and its document is still at the version the query saw;
//   - the source catalog, read again: the principal may still read the source
//     and it was not removed.
//
// A candidate that fails either is gone from the package, as if it had never
// matched (SEC-028): a source deleted, replaced or revoked while the query ran
// vanishes. A failed read of either is an error, not a pass.
//
// Only candidates that can enter the package are rechecked: the head of the
// ordered list in batches of MaxItems, stopping as soon as MaxItems of them have
// passed both reads. The recheck costs one lookup per chunk (docs/efficiency/
// M2-09.md measured it as the largest stage of a small scope), so rechecking the
// whole shortlist for a package that holds a third of it would triple that cost
// for nothing. A candidate that is never rechecked never reaches the package.
// left is how many candidates were left unchecked because the package was full.
func (r *run) finalRecheck(total context.Context, ordered []*cand) (out []*cand, states map[string]SourceState, left int, err error) {
	t0 := r.p.clk.Now()
	defer func() { r.diag.Recheck = r.p.clk.Now().Sub(t0) }()
	var visible []*cand // passed the index recheck, in order
	pos := 0
	for pos < len(ordered) && len(out) < r.maxItems {
		end := min(pos+r.maxItems, len(ordered))
		batch, err := r.indexRecheck(total, ordered[pos:end])
		if err != nil {
			return nil, nil, 0, err
		}
		pos = end
		if len(batch) == 0 {
			continue
		}
		visible = append(visible, batch...)
		// One catalog read for every source of every candidate that passed so far:
		// it is cheap and keeps the states of one instant.
		if states, err = r.sourceStates(total, visible); err != nil {
			return nil, nil, 0, err
		}
		out = out[:0]
		for _, c := range visible {
			if st, ok := states[c.hit.SourceID]; ok && readable(st) {
				out = append(out, c)
			}
		}
	}
	r.diag.DroppedUnauthorised += len(visible) - len(out)
	return out, states, len(ordered) - pos, nil
}

// indexRecheck runs index.Recheck for a batch and returns the candidates that
// are still visible. It never trusts a short or mismatched answer.
func (r *run) indexRecheck(total context.Context, batch []*cand) ([]*cand, error) {
	refs := make([]index.Ref, len(batch))
	for i, c := range batch {
		refs[i] = index.Ref{ChunkID: c.hit.ChunkID, GenerationID: c.hit.GenerationID, DocumentVersion: c.hit.DocumentVersion}
	}
	verdicts, err := within(total, func(c context.Context) ([]index.Verdict, error) {
		return r.p.d.Lexical.Recheck(c, r.scope, refs, false)
	})
	if err != nil {
		return nil, r.stageError("recheck", total, total, err)
	}
	if len(verdicts) != len(refs) {
		return nil, contracts.Newf(contracts.ErrInternal, "query: the recheck answered %d of %d chunks; refusing to build a package", len(verdicts), len(refs))
	}
	live := make([]*cand, 0, len(batch))
	for i, c := range batch {
		v := verdicts[i]
		if !v.Visible || v.Ref.ChunkID != refs[i].ChunkID || v.Ref.GenerationID != refs[i].GenerationID {
			r.diag.DroppedRecheck++
			continue
		}
		live = append(live, c)
	}
	return live, nil
}

// sourceStates reads the catalog for the sources of the candidates.
func (r *run) sourceStates(total context.Context, cands []*cand) (map[string]SourceState, error) {
	seen := map[string]bool{}
	var srcs []string
	for _, c := range cands {
		if !seen[c.hit.SourceID] {
			seen[c.hit.SourceID] = true
			srcs = append(srcs, c.hit.SourceID)
		}
	}
	sort.Strings(srcs)
	states, err := r.p.d.Catalog.Sources(total, r.scope, srcs)
	if err != nil {
		return nil, r.catalogError(total, err)
	}
	return states, nil
}

// build assembles the bounded evidence package from the candidates that passed
// the final recheck, in order. truncated says authorised candidates were already
// left out by an earlier cap (the shortlist); the item and token caps add to it.
//
// Every item carries what a citation needs and the source's egress mode and
// classification, copied from the source's state so a later data-egress filter
// can drop or shorten the text without querying again; a source whose
// declaration states neither carries the most restrictive values. Text is the
// chunk, or the section it was expanded to when that still fits the token
// budget; it is never cut: an item that does not fit is left out and the
// package says it was truncated.
func (r *run) build(survivors []*cand, states map[string]SourceState, truncated bool) Result {
	items := make([]rag.EvidenceItem, 0, min(len(survivors), r.maxItems))
	used := 0
	for _, c := range survivors {
		if len(items) >= r.maxItems {
			r.diag.DroppedBudget++
			truncated = true
			continue
		}
		item, toks, ok := r.makeItem(c, states[c.hit.SourceID], len(items)+1, r.maxToks-used)
		switch {
		case ok:
			items = append(items, item)
			used += toks
		case toks > 0:
			r.diag.DroppedBudget++
			truncated = true
		default:
			r.diag.DroppedInvalid++
		}
	}

	methods := r.methods
	if len(methods) == 0 {
		methods = []rag.RetrievalMethod{rag.RetrievalMethodFts}
	}
	return Result{
		Package: rag.EvidencePackage{
			PackageID: rag.EvidencePackageID(r.p.ids.New("evp")),
			Scope: rag.EvidencePackageScope{
				ApplicationID: common.Identifier(r.scope.ApplicationID),
				TenantID:      common.Identifier(r.scope.TenantID),
				WorkspaceID:   common.Identifier(r.scope.WorkspaceID),
				PrincipalID:   common.Identifier(r.scope.PrincipalID),
			},
			Query:     rag.SearchRequestQuery(r.req.Text),
			CreatedAt: common.Timestamp(r.p.clk.Now().UTC().Format("2006-01-02T15:04:05.000Z")),
			Items:     items,
			Budget: rag.EvidencePackageBudget{
				MaxItems:  int64(r.maxItems),
				MaxToks:   int64(r.maxToks),
				EstToks:   rag.EstToks(used),
				Truncated: truncated,
			},
			Retrieval: rag.EvidencePackageRetrieval{
				Methods: append([]rag.RetrievalMethod(nil), methods...),
				Notices: append(rag.RetrievalNotices{}, r.notices...),
			},
		},
		Diagnostics: r.diag,
	}
}

// makeItem builds one evidence item at position pos with room for room
// estimated tokens. It returns ok false with toks > 0 when the item is valid but
// does not fit, and with toks == 0 when it is not a valid item at all.
func (r *run) makeItem(c *cand, st SourceState, pos, room int) (item rag.EvidenceItem, toks int, ok bool) {
	h := c.hit
	version := h.DocumentVersion
	if !validOpaqueVersion(version) || h.ChunkID == "" || h.GenerationID == "" || h.SourceID == "" {
		return item, 0, false
	}

	type option struct {
		text    string
		locator conversation.CitationLocator
	}
	var options []option
	if c.parent != nil {
		options = append(options, option{c.parent.Text, parentLocator(*c.parent)})
	}
	options = append(options, option{h.Text, h.Locator()})

	var chosen *option
	smallest := 0
	for i := range options {
		o := &options[i]
		if o.text == "" || utf8.RuneCountInString(o.text) > chunk.MaxChunkChars || strings.ContainsRune(o.text, 0) || !utf8.ValidString(o.text) {
			continue
		}
		t := chunk.EstimateTokens(o.text)
		if smallest == 0 || t < smallest {
			smallest = t
		}
		if t <= room {
			chosen, toks = o, t
			break
		}
	}
	if chosen == nil {
		return item, smallest, false // smallest is 0 when no option was valid
	}

	name := strings.TrimSpace(st.DisplayName)
	if name == "" {
		name = h.SourceID
	}
	if rs := []rune(name); len(rs) > 256 {
		name = string(rs[:256])
	}
	availability := st.Availability
	if availability == "" {
		availability = conversation.CitationAvailabilityUnavailable
	}
	staleness := st.Staleness
	if staleness == "" {
		staleness = conversation.CitationStalenessUnknown
	}
	openRef := conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}}
	if st.OpenRef != nil && st.OpenRef.Value != nil {
		openRef = *st.OpenRef
	}
	egress := rag.EffectiveSourceEgress(st.Egress)
	class := rag.EffectiveSourceClassification(st.Classification)
	loc := chosen.locator
	item = rag.EvidenceItem{
		EvidenceID:           rag.EvidenceID("e" + strconv.Itoa(pos)),
		Rank:                 int64(pos),
		ChunkID:              common.Identifier(h.ChunkID),
		GenerationID:         rag.GenerationID(h.GenerationID),
		SourceID:             actions.SourceID(h.SourceID),
		DisplayName:          name,
		Version:              actions.OpaqueVersion(version),
		Locator:              &loc,
		Availability:         availability,
		Staleness:            staleness,
		OpenRef:              openRef,
		Text:                 chosen.text,
		EstToks:              rag.EstToks(toks),
		SourceEgress:         &egress,
		SourceClassification: &class,
	}
	return item, toks, true
}

func parentLocator(p index.Parent) conversation.CitationLocator {
	loc := conversation.CitationLocator{Span: &conversation.CitationSpan{Start: int64(p.Span.Start), End: int64(p.Span.End)}}
	if p.Page > 0 {
		pg := int64(p.Page)
		loc.Page = &pg
	}
	if p.Section != "" {
		s := p.Section
		if rs := []rune(s); len(rs) > 256 {
			s = string(rs[:256])
		}
		loc.Section = &s
	}
	return loc
}

// validOpaqueVersion is the schema's opaqueVersion: 1 to 256 characters, none of
// them whitespace or a control character.
func validOpaqueVersion(v string) bool {
	if v == "" || len(v) > 256 || !utf8.ValidString(v) {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] <= 0x20 || v[i] == 0x7f {
			return false
		}
	}
	return true
}
