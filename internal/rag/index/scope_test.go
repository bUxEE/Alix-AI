package index

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// queryPlan returns the EXPLAIN QUERY PLAN lines of a statement, on the reader
// pool, in plan order.
func (f *fixture) queryPlan(query string, args []any) []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		f.t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, detail)
	}
	return out
}

// SEC-020: the scope predicate is applied in SQL before ranking.
//
// The fixture is adversarial for a post-filter: another tenant holds hundreds of
// chunks that match the query far better than the caller's three, so a search
// that ranked across scopes and filtered afterwards would return none of the
// caller's chunks at limit 3 (it would be filling the limit with foreign rows
// that the filter then discards).
func TestScopeFilterAppliedInSQL(t *testing.T) {
	f := newFixture(t)
	mine := f.scope("app", "tenant-mine", "w1")
	foreign := f.scope("app", "tenant-foreign", "w1")

	// The caller's chunks mention the term once among many words.
	var own []spec
	for i := 0; i < 3; i++ {
		own = append(own, spec{fmt.Sprintf("mine-%d", i),
			fmt.Sprintf("Quarterly report %d covers budgets, hiring plans and one zebra sighting near the depot gate.", i)})
	}
	f.serving(mine, f.source(), own...)
	// The foreign tenant's chunks are the best BM25 matches in the whole index.
	var other []spec
	for i := 0; i < 300; i++ {
		other = append(other, spec{fmt.Sprintf("foreign-%d", i), "zebra zebra zebra zebra zebra"})
	}
	f.serving(foreign, f.source(), other...)

	// Control: an unscoped match really does see the foreign rows first, so the
	// fixture would catch a post-filter.
	var unscoped int
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM ragindex_fts WHERE ragindex_fts MATCH 'zebra'`).Scan(&unscoped); err != nil {
		t.Fatal(err)
	}
	if unscoped != 303 {
		t.Fatalf("control: unscoped MATCH sees %d rows, want 303", unscoped)
	}

	type stmt struct {
		sql  string
		args []any
	}
	var seen []stmt
	f.ix.trace = func(q string, args []any) { seen = append(seen, stmt{q, append([]any(nil), args...)}) }
	res := f.search(mine, Query{Text: "zebra", Limit: 3})
	f.ix.trace = nil

	if len(res.Hits) != 3 {
		t.Fatalf("limit 3 returned %d hits (%s); a scope filter applied after ranking would return none", len(res.Hits), joinHits(res))
	}
	for _, h := range res.Hits {
		if !strings.HasPrefix(h.DocumentID, "mine-") {
			t.Errorf("foreign hit %s", h.DocumentID)
		}
	}

	// Every statement of the search path carries the scope before ORDER BY and
	// LIMIT, and the ranking statement confines the full-text scan to a rowid
	// range.
	if len(seen) == 0 {
		t.Fatal("no statement was traced")
	}
	var ranking, reading *stmt
	for i := range seen {
		s := &seen[i]
		for _, col := range []string{"c.application_id = ?", "c.tenant_id = ?", "c.workspace_id = ?", "g.application_id = ?", "g.tenant_id = ?", "g.workspace_id = ?"} {
			if !strings.Contains(s.sql, col) {
				t.Errorf("statement lacks %q:\n%s", col, s.sql)
			}
		}
		if i := strings.Index(s.sql, "ORDER BY"); i >= 0 {
			if j := strings.Index(s.sql, "c.tenant_id = ?"); j < 0 || j > i {
				t.Errorf("scope predicate comes after ORDER BY:\n%s", s.sql)
			}
		}
		switch {
		case strings.Contains(s.sql, "ragindex_fts MATCH"):
			ranking = s
		case strings.Contains(s.sql, "c.text"):
			reading = s
		}
	}
	if ranking == nil || reading == nil {
		t.Fatalf("did not see both the ranking and the text-reading statement: %d statements", len(seen))
	}
	if !strings.Contains(ranking.sql, "ragindex_fts.rowid >= ?") || !strings.Contains(ranking.sql, "ragindex_fts.rowid <= ?") {
		t.Errorf("ranking statement does not constrain the FTS rowid range:\n%s", ranking.sql)
	}
	if strings.Contains(ranking.sql, "c.text") {
		t.Errorf("the ranking statement reads chunk text; text must only come from the second statement:\n%s", ranking.sql)
	}
	if !strings.Contains(reading.sql, "c.rid >= ?") || !strings.Contains(reading.sql, "g.state = 'active'") || !strings.Contains(reading.sql, "c.tombstoned_at IS NULL") {
		t.Errorf("text-reading statement lacks the range, generation or tombstone check:\n%s", reading.sql)
	}

	// The plan: the full-text index is the OUTER loop and the scan carries both
	// rowid bounds (idxStr flags '>' and '<'). A plain JOIN lets SQLite drive from
	// the chunk table and run one MATCH per chunk row (a query that never ends at
	// 100k rows); the CROSS JOIN in the statement forbids that.
	plan := f.queryPlan(ranking.sql, ranking.args)
	t.Logf("ranking plan: %v", plan)
	if len(plan) == 0 || !strings.Contains(plan[0], "ragindex_fts VIRTUAL TABLE INDEX") {
		t.Fatalf("the full-text scan is not the outer loop: %v", plan)
	}
	idx := plan[0][strings.Index(plan[0], "INDEX ")+len("INDEX "):]
	if !strings.Contains(idx, "M") || !strings.Contains(idx, ">") || !strings.Contains(idx, "<") || strings.Contains(idx, "=") {
		t.Errorf("full-text scan %q is not MATCH plus a pushed-down rowid range (want flags M, > and <, no =)", idx)
	}
	for _, line := range plan[1:] {
		if strings.Contains(line, "SCAN ragindex_chunk") || strings.Contains(line, "SCAN c") {
			t.Errorf("plan scans the chunk table: %v", plan)
		}
	}
}

// A scope that never indexed anything has no rowid range: it gets an empty
// result, never an unbounded one and never an error that names other scopes.
func TestUnknownScopeReturnsNothing(t *testing.T) {
	f := newFixture(t)
	f.serving(f.scope("app", "t1", "w1"), f.source(), spec{"d", "marker text about invoices"})
	res := f.search(f.scope("app", "never-indexed", "w1"), Query{Text: "invoices"})
	if len(res.Hits) != 0 {
		t.Fatalf("an unknown scope returned %s", joinHits(res))
	}
	if res.Version.Epoch != 0 || len(res.Version.Active) != 0 {
		t.Errorf("an unknown scope has version %+v", res.Version)
	}
}

// No scope, no query: the statement is never run.
func TestSearchRequiresScope(t *testing.T) {
	f := newFixture(t)
	f.serving(f.scope("app", "t1", "w1"), f.source(), spec{"d", "text about invoices"})
	ran := 0
	f.ix.trace = func(string, []any) { ran++ }
	for name, scope := range map[string]contracts.Scope{
		"zero":       {},
		"no tenant":  func() contracts.Scope { s := f.scope("app", "t1", "w1"); s.TenantID = ""; return s }(),
		"wildcard":   func() contracts.Scope { s := f.scope("app", "t1", "w1"); s.WorkspaceID = "*"; return s }(),
		"no session": func() contracts.Scope { s := f.scope("app", "t1", "w1"); s.SessionID = ""; return s }(),
	} {
		_, err := f.ix.Search(context.Background(), scope, Query{Text: "invoices"})
		if code := contracts.CodeOf(err); code != contracts.ErrScopeMissing && code != contracts.ErrScopeViolation {
			t.Errorf("%s: err = %v, want scope_missing or scope_violation", name, err)
		}
		if _, err := f.ix.Recheck(context.Background(), scope, []Ref{{ChunkID: "x", GenerationID: "y"}}, false); contracts.CodeOf(err) == "" {
			t.Errorf("%s: Recheck accepted an invalid scope", name)
		}
		if _, err := f.ix.BeginGeneration(context.Background(), scope, "src_x", f.comps()); contracts.CodeOf(err) == "" {
			t.Errorf("%s: BeginGeneration accepted an invalid scope", name)
		}
	}
	f.ix.trace = nil
	if ran != 0 {
		t.Errorf("%d statements ran for invalid scopes", ran)
	}
}

// SEC-020, SEC-018: a corpus of several applications, tenants and workspaces, with
// look-alike content (same titles, same text, same document and chunk ids, same
// identifiers) and one marker per scope, never returns a foreign row for any
// query, including exact-identifier queries and queries that try FTS5 syntax.
func TestCrossTenantNeverReturned(t *testing.T) {
	f := newFixture(t)
	scopes := []contracts.Scope{
		f.scope("app1", "t1", "w1"),
		f.scope("app1", "t1", "w2"),
		f.scope("app1", "t2", "w1"),
		f.scope("app2", "t1", "w1"), // same tenant id as app1/t1: tenant ids are unique only within an application
		f.scope("app2", "t2", "w2"),
	}
	marker := func(s contracts.Scope) string {
		return "mk" + strings.NewReplacer("-", "", "_", "").Replace(s.ApplicationID+s.TenantID+s.WorkspaceID)
	}
	uniqueSKU := func(s contracts.Scope) string {
		return "SKU-" + strings.ToUpper(marker(s)) + "-77"
	}
	// Every scope gets the same documents (same ids, so identical chunk ids),
	// differing only by the marker word and one scope-unique identifier.
	for _, s := range scopes {
		f.serving(s, "src_shared_01", // the same source id in every scope as well
			spec{"handbook", fmt.Sprintf("# Handbook\n\nThe warranty covers parts for two years. Reference %s applies to this scope only. Common code ZX-100 is valid everywhere.", marker(s))},
			spec{"faq", fmt.Sprintf("# FAQ\n\nHow do I return a product? Contact support quoting %s and the common code ZX-100.", uniqueSKU(s))},
			spec{"table", "# Prices\n\n| Item | Code |\n| --- | --- |\n| Widget | ZX-100 |\n| Gadget | ZX-200 |\n"},
		)
	}
	// The chunk ids really are shared across scopes: the look-alike case.
	var shared int
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM (SELECT chunk_id FROM ragindex_chunk WHERE document_id = 'table' GROUP BY chunk_id HAVING count(*) = 5)`).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if shared == 0 {
		t.Fatal("fixture: identical documents did not produce identical chunk ids across scopes")
	}

	vocab := []string{"warranty", "parts", "years", "reference", "applies", "scope", "common", "code", "valid", "everywhere",
		"return", "product", "contact", "support", "quoting", "widget", "gadget", "item", "prices", "handbook", "faq", "ZX-100", "ZX-200", "zx100"}
	for _, s := range scopes {
		vocab = append(vocab, marker(s), uniqueSKU(s), strings.ToLower(marker(s))[:6])
	}
	// FTS5 syntax and other awkward input, which must be inert.
	hostile := []string{
		`warranty OR mk`, `"warranty" NEAR(parts, scope)`, `text : warranty`, `application_id : app2`, `tenant_id:t2`,
		`mk*`, `* `, `^warranty`, `warranty NOT reference`, `"`, `""`, `)`, `(((`, `NEAR(`, `col:`, "\u0301", `-warranty`, `warranty AND`,
		`{text section} : warranty`, `rowid:1`, `ragindex_fts`, `'; DROP TABLE ragindex_chunk; --`,
	}
	rng := rand.New(rand.NewPCG(7, 11))
	queries := append([]string(nil), hostile...)
	for i := 0; i < 150; i++ {
		var q []string
		for j := 0; j < 1+rng.IntN(4); j++ {
			q = append(q, vocab[rng.IntN(len(vocab))])
		}
		queries = append(queries, strings.Join(q, " "))
	}
	// Also each scope's own marker and identifier as the whole query.
	for _, s := range scopes {
		queries = append(queries, marker(s), uniqueSKU(s), strings.ToLower(uniqueSKU(s)))
	}

	for _, s := range scopes {
		for _, q := range queries {
			for _, all := range []bool{false, true} {
				res, err := f.ix.Search(context.Background(), s, Query{Text: q, Limit: 50, MatchAll: all})
				if err != nil {
					t.Fatalf("scope %s/%s/%s query %q: %v", s.ApplicationID, s.TenantID, s.WorkspaceID, q, err)
				}
				for _, h := range res.Hits {
					blob := h.Text + " " + h.Section + " " + strings.Join(h.Path, " ") + " " + h.Header
					for _, other := range scopes {
						if other.ApplicationID == s.ApplicationID && other.TenantID == s.TenantID && other.WorkspaceID == s.WorkspaceID {
							continue
						}
						if strings.Contains(blob, marker(other)) || strings.Contains(strings.ToUpper(blob), strings.ToUpper(uniqueSKU(other))) {
							t.Fatalf("scope %s/%s/%s query %q returned another scope's content (%s): %q",
								s.ApplicationID, s.TenantID, s.WorkspaceID, q, marker(other), h.Text)
						}
					}
				}
			}
		}
		// Exact identifier that exists only in the other scopes: nothing.
		for _, other := range scopes {
			if other.ApplicationID == s.ApplicationID && other.TenantID == s.TenantID && other.WorkspaceID == s.WorkspaceID {
				continue
			}
			for _, q := range []string{uniqueSKU(other), strings.ToLower(uniqueSKU(other)), strings.ReplaceAll(uniqueSKU(other), "-", ""), marker(other)} {
				res := f.search(s, Query{Text: q, Limit: 50})
				for _, h := range res.Hits {
					if strings.Contains(strings.ToUpper(h.Text), strings.ToUpper(uniqueSKU(other))) || strings.Contains(h.Text, marker(other)) {
						t.Fatalf("scope %s/%s/%s found another scope's identifier %q: %q", s.ApplicationID, s.TenantID, s.WorkspaceID, q, h.Text)
					}
				}
			}
		}
		// The caller's own identifier is found, so the checks above are not
		// vacuous.
		own := f.search(s, Query{Text: uniqueSKU(s)})
		if len(own.Hits) == 0 || len(own.Hits[0].ExactIdentifiers) == 0 {
			t.Errorf("scope %s/%s/%s cannot find its own identifier %s", s.ApplicationID, s.TenantID, s.WorkspaceID, uniqueSKU(s))
		}
		// The shared identifier finds the scope's own documents, and only them,
		// even though other scopes hold identical chunks.
		shared := f.search(s, Query{Text: "ZX-100", Limit: 50})
		if len(shared.Hits) == 0 {
			t.Errorf("scope %s/%s/%s cannot find ZX-100", s.ApplicationID, s.TenantID, s.WorkspaceID)
		}
		for _, h := range shared.Hits {
			var app, tenant, ws string
			if err := f.db.QueryRowContext(context.Background(),
				`SELECT application_id, tenant_id, workspace_id FROM ragindex_chunk WHERE generation_id = ? AND chunk_id = ? AND document_id = ?`,
				h.GenerationID, h.ChunkID, h.DocumentID).Scan(&app, &tenant, &ws); err != nil {
				t.Fatal(err)
			}
			if app != s.ApplicationID || tenant != s.TenantID || ws != s.WorkspaceID {
				t.Fatalf("hit %s belongs to %s/%s/%s, not the caller's scope", h.ChunkID, app, tenant, ws)
			}
		}
	}
}

// The rowid ranges of different scopes are disjoint and every chunk row lies in
// its own scope's range.
func TestScopeRowidRangesDisjoint(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 4; i++ {
		f.serving(f.scope("app", fmt.Sprintf("t%d", i), "w1"), f.source(), spec{"d", "some text to index"}, spec{"e", "more text to index"})
	}
	rows, err := f.db.QueryContext(context.Background(), `SELECT scope_no, application_id, tenant_id, workspace_id FROM ragindex_scope`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var no int64
		var a, tn, w string
		if err := rows.Scan(&no, &a, &tn, &w); err != nil {
			t.Fatal(err)
		}
		lo, hi := ridRange(no)
		var inside, total int
		if err := f.db.QueryRowContext(context.Background(),
			`SELECT count(*), sum(rid >= ? AND rid <= ?) FROM ragindex_chunk WHERE application_id = ? AND tenant_id = ? AND workspace_id = ?`,
			lo, hi, a, tn, w).Scan(&total, &inside); err != nil {
			t.Fatal(err)
		}
		if total == 0 || inside != total {
			t.Errorf("scope %d: %d of %d chunk rows inside its range", no, inside, total)
		}
		n++
	}
	if n != 4 {
		t.Fatalf("saw %d scopes", n)
	}
}
