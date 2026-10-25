package sdktest

import (
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// The RAG area of the fake: a test declares what the application's package declares (DeclareSource), the
// caller registers it, and searches and answers are grounded on the declared passage: a question that shares a
// word with it is answered with a citation, any other is the typed no_supported_evidence result.

const citationRefTTL = 15 * time.Minute

type knowledge struct{ title, passage string }

type ragSource struct {
	id          string
	scope       string
	declaration string
	generation  string
	state       string // the generation state: active or invalidated
	created     time.Time
}

type ragRef struct {
	scope     string
	sourceID  string
	preview   string
	expiresAt time.Time
	principal string
}

type ragState struct {
	declared map[string]knowledge
	sources  map[string]*ragSource
	refs     map[string]*ragRef
}

func newRagState() ragState {
	return ragState{declared: map[string]knowledge{}, sources: map[string]*ragSource{}, refs: map[string]*ragRef{}}
}

// DeclareSource says what the application's active package declares as a RAG source: a name the caller may
// register, the display name of the source and the passage that answers are grounded on.
func (f *Fake) DeclareSource(declaration, title, passage string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rag.declared[declaration] = knowledge{title: title, passage: passage}
}

func ragScope(c Claims) string { return c.ApplicationID + "|" + c.TenantID + "|" + c.WorkspaceID }

func (c *call) notFound() {
	c.fail(http.StatusNotFound, contracts.ErrValidationFailed, "no such source in this scope", "reason", "not_found")
}

// source returns the source the path names in the caller's scope.
func (c *call) source() (*ragSource, bool) {
	s := c.f.rag.sources[c.param("source_id")]
	if s == nil || s.scope != ragScope(c.claims) {
		c.notFound()
		return nil, false
	}
	return s, true
}

func (f *Fake) sourceDoc(s *ragSource) map[string]any {
	k := f.rag.declared[s.declaration]
	at := f.stamp(s.created)
	return map[string]any{
		"declaration": s.declaration, "package_version": "1.0.0", "serving": true,
		"status": map[string]any{
			"source_id": s.id, "display_name": k.title, "state": "ready", "availability": "available", "staleness": "current",
			"last_attempt_at": at, "last_success_at": at,
			"counts":               map[string]any{"discovered": 1, "indexed": 1, "skipped": 0, "failed": 0, "tombstoned": 0},
			"active_generation_id": s.generation,
		},
	}
}

func (f *Fake) generationDoc(s *ragSource) map[string]any {
	component := func(name, version string) map[string]any { return map[string]any{"name": name, "version": version} }
	doc := map[string]any{
		"generation_id": s.generation, "source_id": s.id, "state": s.state,
		"components": map[string]any{
			"parser": component("alixai-parser", "1.0.0"), "ocr": nil,
			"chunker": component("structure-chunker", "2"), "embedder": component("bge-small-en", "v1.5"),
		},
		"counts":     map[string]any{"documents": 1, "chunks": 1, "embedded_chunks": 1},
		"created_at": f.stamp(s.created), "activated_at": f.stamp(s.created), "retired_at": nil,
	}
	if s.state == "invalidated" {
		doc["retired_at"] = f.stamp(f.now())
	}
	return doc
}

type declarationBody struct {
	Declaration string `json:"declaration"`
}

func (c *call) ragRegister() {
	raw, ok := c.readBody()
	if !ok {
		return
	}
	var body declarationBody
	if !c.closedObject(raw, &body) {
		return
	}
	if body.Declaration == "" || body.Declaration != strings.TrimSpace(body.Declaration) || len(body.Declaration) > 128 {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the declaration is malformed")
		return
	}
	if _, declared := c.f.rag.declared[body.Declaration]; !declared {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the active package does not declare this source", "reason", "not_declared")
		return
	}
	scope := ragScope(c.claims)
	for _, s := range c.f.rag.sources {
		if s.scope == scope && s.declaration == body.Declaration {
			c.json(http.StatusOK, c.f.sourceDoc(s))
			return
		}
	}
	s := &ragSource{
		id: c.f.typedID("src"), scope: scope, declaration: body.Declaration, generation: c.f.typedID("gen"),
		state: "active", created: c.f.now(),
	}
	c.f.rag.sources[s.id] = s
	c.w.Header().Set("Location", "/v1/rag/sources/"+s.id)
	c.json(http.StatusOK, c.f.sourceDoc(s))
}

func (c *call) scopedSources() []*ragSource {
	var out []*ragSource
	for _, s := range c.f.rag.sources {
		if s.scope == ragScope(c.claims) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (c *call) ragList() {
	docs := []any{}
	for _, s := range c.scopedSources() {
		docs = append(docs, c.f.sourceDoc(s))
	}
	c.json(http.StatusOK, map[string]any{"sources": docs})
}

func (c *call) ragReaudit() {
	if !c.emptyBody() {
		return
	}
	n := len(c.scopedSources())
	c.json(http.StatusOK, map[string]any{"checked": n, "rebound": n, "queued": 0, "removed": 0, "failed": 0})
}

func (c *call) ragGet() {
	if s, ok := c.source(); ok {
		c.json(http.StatusOK, c.f.sourceDoc(s))
	}
}

func (c *call) ragRemove() {
	if s, ok := c.source(); ok {
		delete(c.f.rag.sources, s.id)
		c.w.WriteHeader(http.StatusNoContent)
	}
}

// emptyBody accepts no body or {}.
func (c *call) emptyBody() bool {
	raw, ok := c.readBody()
	if !ok {
		return false
	}
	if len(raw) == 0 {
		return true
	}
	var empty struct{}
	return c.closedObject(raw, &empty)
}

func (c *call) ragAccepted() {
	s, ok := c.source()
	if !ok || !c.emptyBody() {
		return
	}
	c.w.Header().Set("Location", "/v1/rag/sources/"+s.id)
	c.json(http.StatusAccepted, map[string]any{"source_id": s.id, "job_id": c.f.typedID("job")})
}

func (c *call) ragSync()    { c.ragAccepted() }
func (c *call) ragRebuild() { c.ragAccepted() }

func (c *call) ragGenerations() {
	if s, ok := c.source(); ok {
		c.json(http.StatusOK, map[string]any{"generations": []any{c.f.generationDoc(s)}})
	}
}

func (c *call) ragInvalidate() {
	s, ok := c.source()
	if !ok || !c.emptyBody() {
		return
	}
	if c.param("generation_id") != s.generation {
		c.fail(http.StatusNotFound, contracts.ErrValidationFailed, "no such generation", "reason", "not_found")
		return
	}
	s.state = "invalidated"
	c.json(http.StatusOK, c.f.generationDoc(s))
}

// words are the lower-case words of a question that can match a passage.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// matches returns the serving sources of the scope whose passage shares a word of four letters or more with
// the question, narrowed to source_ids when given.
func (c *call) matches(question string, only []string) []*ragSource {
	var out []*ragSource
	for _, s := range c.scopedSources() {
		if s.state != "active" || (len(only) > 0 && !contains(only, s.id)) {
			continue
		}
		passage := strings.ToLower(c.f.rag.declared[s.declaration].passage)
		for _, w := range words(question) {
			if len(w) >= 4 && strings.Contains(passage, w) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// reference registers the open reference of a citation and returns its file id.
func (c *call) reference(s *ragSource) string {
	id := c.f.typedID("fil")
	c.f.rag.refs[id] = &ragRef{
		scope: ragScope(c.claims), principal: c.claims.PrincipalID, sourceID: s.id,
		preview: c.f.rag.declared[s.declaration].passage, expiresAt: c.f.now().Add(citationRefTTL),
	}
	return id
}

type searchBody struct {
	Query     string   `json:"query"`
	SourceIDs []string `json:"source_ids"`
	Limit     *int     `json:"limit"`
}

func (c *call) ragSearch() {
	raw, ok := c.readBody()
	if !ok {
		return
	}
	var body searchBody
	if !c.closedObject(raw, &body) {
		return
	}
	if strings.TrimSpace(body.Query) == "" || len(body.SourceIDs) > 32 || (body.Limit != nil && (*body.Limit < 1 || *body.Limit > 50)) {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the search request is not valid")
		return
	}
	hits := []any{}
	for i, s := range c.matches(body.Query, body.SourceIDs) {
		k := c.f.rag.declared[s.declaration]
		hits = append(hits, map[string]any{
			"evidence_id": "e" + string(rune('1'+i)), "rank": i + 1, "source_id": s.id, "display_name": k.title,
			"version": "rev-1", "availability": "available", "staleness": "current",
			"open_ref": map[string]any{"kind": "file", "file_id": c.reference(s)}, "excerpt": k.passage,
		})
	}
	c.json(http.StatusOK, map[string]any{"results": hits, "notices": []any{}})
}

type answerBody struct {
	Question  string   `json:"question"`
	SourceIDs []string `json:"source_ids"`
}

func (c *call) ragAnswer() {
	raw, ok := c.readBody()
	if !ok {
		return
	}
	var body answerBody
	if !c.closedObject(raw, &body) {
		return
	}
	if strings.TrimSpace(body.Question) == "" || len(body.SourceIDs) > 32 {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the answer request is not valid")
		return
	}
	found := c.matches(body.Question, body.SourceIDs)
	if len(found) == 0 {
		c.json(http.StatusOK, map[string]any{"outcome": "no_supported_evidence", "reason": "no_match", "notices": []any{}})
		return
	}
	claims, citations := []any{}, []any{}
	for i, s := range found {
		k := c.f.rag.declared[s.declaration]
		claims = append(claims, map[string]any{"text": k.passage, "citation_indexes": []any{i}})
		citations = append(citations, map[string]any{
			"source_id": s.id, "display_name": k.title, "version": "rev-1", "availability": "available", "staleness": "current",
			"open_ref": map[string]any{"kind": "file", "file_id": c.reference(s)}, "claims": []any{k.passage},
		})
	}
	c.json(http.StatusOK, map[string]any{"outcome": "answered", "claims": claims, "citations": citations, "notices": []any{}})
}

func (c *call) ragResolve() {
	ref := c.f.rag.refs[c.param("file_id")]
	if ref == nil || ref.scope != ragScope(c.claims) || ref.principal != c.claims.PrincipalID {
		// Every reference that does not resolve for the caller is the same permission_denied: no oracle.
		c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "the reference does not resolve for this caller")
		return
	}
	if !c.f.now().Before(ref.expiresAt) {
		c.fail(http.StatusGone, contracts.ErrExpired, "the reference has expired; ask the question again")
		return
	}
	s := c.f.rag.sources[ref.sourceID]
	if s == nil || s.state != "active" {
		c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "the reference does not resolve for this caller")
		return
	}
	k := c.f.rag.declared[s.declaration]
	c.json(http.StatusOK, map[string]any{
		"source_id": s.id, "display_name": k.title, "version": "rev-1", "availability": "available", "staleness": "current",
		"open_ref": map[string]any{"kind": "source"}, "preview": ref.preview,
		"expires_at": c.f.stamp(ref.expiresAt),
	})
}
