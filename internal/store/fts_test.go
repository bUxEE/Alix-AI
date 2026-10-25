package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestFTS5Available(t *testing.T) {
	db := openTest(t, nil)
	ctx := context.Background()

	var compiled int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_compile_options WHERE compile_options = 'ENABLE_FTS5'`).Scan(&compiled); err != nil {
		t.Fatal(err)
	}
	if compiled != 1 {
		t.Fatalf("SQLite was built without ENABLE_FTS5; the plan depends on it (ADR 0002, 0009)")
	}

	if _, err := db.ExecContext(ctx, `
		CREATE VIRTUAL TABLE probe USING fts5 (body);
		INSERT INTO probe (body) VALUES ('the quick brown fox'), ('lazy dogs sleep');`); err != nil {
		t.Fatalf("FTS5 virtual table cannot be created with this driver: %v", err)
	}
	var hits int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM probe WHERE probe MATCH 'fox'`).Scan(&hits); err != nil {
		t.Fatalf("FTS5 MATCH failed: %v", err)
	}
	if hits != 1 {
		t.Errorf("MATCH 'fox' hit %d rows, want 1", hits)
	}
	var version string
	if err := db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("SQLite %s with FTS5 via modernc.org/sqlite", version)
}

// rowIDs runs a query returning one integer column and collects it in order.
func rowIDs(t *testing.T, db *DB, query string, args ...any) []int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFTS5BM25Ranking(t *testing.T) {
	db := openTest(t, nil)
	ctx := context.Background()
	filler := strings.Repeat("filler ", 40)
	docs := []struct {
		id          int
		title, body string
	}{
		{1, "intro", "alpha beta gamma"},
		{2, "repeat", "alpha alpha alpha delta"},
		{3, "long", "alpha " + filler},
		{4, "other", "completely unrelated words"},
		{5, "alpha", "beta beta beta"}, // term only in the title
	}
	if _, err := db.ExecContext(ctx, `CREATE VIRTUAL TABLE docs USING fts5 (title, body, tokenize = 'unicode61 remove_diacritics 2')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *Tx) error {
		for _, d := range docs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO docs (rowid, title, body) VALUES (?, ?, ?)`, d.id, d.title, d.body); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Equal column weights: more occurrences in a shorter document ranks first,
	// the long document with a single occurrence ranks last, non-matches are
	// absent. These are the relations that define BM25, not a magic ordering.
	got := rowIDs(t, db, `SELECT rowid FROM docs WHERE docs MATCH 'alpha' ORDER BY bm25(docs)`)
	pos := map[int]int{}
	for i, id := range got {
		pos[id] = i
	}
	for _, id := range []int{1, 2, 3, 5} {
		if _, ok := pos[id]; !ok {
			t.Fatalf("doc %d missing from the results %v", id, got)
		}
	}
	if _, ok := pos[4]; ok {
		t.Errorf("non-matching doc 4 returned: %v", got)
	}
	if !(pos[2] < pos[1] && pos[1] < pos[3]) {
		t.Errorf("BM25 order wrong: want 2 (tf=3) before 1 (tf=1, short) before 3 (tf=1, long), got %v", got)
	}

	// Scores are negative (SQLite negates BM25 so ascending is best-first) and
	// non-decreasing along the ranking; rank is the same number.
	srows, err := db.QueryContext(ctx, `SELECT rowid, bm25(docs), rank FROM docs WHERE docs MATCH 'alpha' ORDER BY rank`)
	if err != nil {
		t.Fatal(err)
	}
	defer srows.Close()
	prev := -1e18
	n := 0
	for srows.Next() {
		var id int
		var score, rank float64
		if err := srows.Scan(&id, &score, &rank); err != nil {
			t.Fatal(err)
		}
		if score >= 0 {
			t.Errorf("doc %d: bm25 = %v, want negative", id, score)
		}
		if score < prev {
			t.Errorf("doc %d: scores not ascending (%v after %v)", id, score, prev)
		}
		if score != rank {
			t.Errorf("doc %d: rank %v != bm25 %v", id, rank, score)
		}
		prev = score
		n++
	}
	if n != 4 {
		t.Errorf("scored %d docs, want 4", n)
	}

	// Column weights: title counts ten times the body, so the title-only hit
	// (doc 5) now beats every body-only hit.
	if first := rowIDs(t, db, `SELECT rowid FROM docs WHERE docs MATCH 'alpha' ORDER BY bm25(docs, 10.0, 1.0) LIMIT 1`); !reflect.DeepEqual(first, []int{5}) {
		t.Errorf("with title weight 10, first hit = %v, want [5]", first)
	}

	// Query forms the RAG layer relies on.
	for _, c := range []struct {
		name, match string
		want        []int
	}{
		{"prefix", `alph*`, []int{1, 2, 3, 5}},
		{"phrase", `"alpha beta"`, []int{1}},
		{"and", `alpha AND delta`, []int{2}},
		{"column filter", `title : alpha`, []int{5}},
		{"not", `alpha NOT filler`, []int{1, 2, 5}},
	} {
		if got := rowIDs(t, db, `SELECT rowid FROM docs WHERE docs MATCH ? ORDER BY rowid`, c.match); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s (%q) = %v, want %v", c.name, c.match, got, c.want)
		}
	}
}

// TestFTS5ExternalContentScoped exercises the pattern the RAG module will use:
// an external-content index with scope columns, kept in sync by triggers, under
// the connection's defensive mode and trusted_schema=OFF, queried with the
// scope predicate and BM25 together.
func TestFTS5ExternalContentScoped(t *testing.T) {
	db := openTest(t, nil)
	ctx := context.Background()
	a1, b1 := testScope("app", "tenantA", "w1"), testScope("app", "tenantB", "w1")
	insertNote(t, db, a1, "a-1", "Café menu", "espresso and croissant")
	insertNote(t, db, a1, "a-2", "Other", "espresso espresso espresso")
	insertNote(t, db, b1, "b-1", "Their note", "espresso for tenant B only")

	// The FTS table carries the scope columns, so the scope predicate applies
	// to the index itself, before any text is returned.
	search := func(tenant string) []string {
		t.Helper()
		rows, err := QueryScoped(ctx, db, testScope("app", tenant, "w1"), ScopedStmt{
			SQL:       `SELECT n.id FROM notes_fts JOIN notes n ON n.rowid = notes_fts.rowid`,
			Alias:     "notes_fts",
			Where:     `notes_fts MATCH ?`,
			WhereArgs: []any{"espresso"},
			Tail:      `ORDER BY bm25(notes_fts)`,
		})
		if err != nil {
			t.Fatalf("scoped FTS query: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	if got := search("tenantA"); !reflect.DeepEqual(got, []string{"a-2", "a-1"}) {
		t.Errorf("tenant A results = %v, want [a-2 a-1] (BM25 order, tenant B excluded)", got)
	}
	if got := search("tenantB"); !reflect.DeepEqual(got, []string{"b-1"}) {
		t.Errorf("tenant B results = %v, want [b-1]", got)
	}
	if got := search("tenantC"); len(got) != 0 {
		t.Errorf("unknown tenant results = %v, want none", got)
	}

	// Diacritics fold: 'cafe' finds 'Café'.
	if n := count(t, db, `SELECT count(*) FROM notes_fts WHERE notes_fts MATCH 'cafe'`); n != 1 {
		t.Errorf("remove_diacritics: 'cafe' matched %d rows, want 1", n)
	}

	// Update and delete keep the index in step (the 'delete' command on an
	// external-content table writes shadow tables; defensive mode must allow it).
	if _, err := ExecScoped(ctx, db, a1, ScopedStmt{
		SQL: `UPDATE notes SET body = ?`, Args: []any{"decaf only"},
		Where: `id = ?`, WhereArgs: []any{"a-2"},
	}); err != nil {
		t.Fatalf("update through trigger: %v", err)
	}
	if got := search("tenantA"); !reflect.DeepEqual(got, []string{"a-1"}) {
		t.Errorf("after update tenant A = %v, want [a-1]", got)
	}
	if _, err := ExecScoped(ctx, db, a1, ScopedStmt{SQL: `DELETE FROM notes`, Where: `id = ?`, WhereArgs: []any{"a-1"}}); err != nil {
		t.Fatalf("delete through trigger: %v", err)
	}
	if got := search("tenantA"); len(got) != 0 {
		t.Errorf("after delete tenant A = %v, want none", got)
	}
	// Administrative commands still work under defensive mode.
	for _, cmd := range []string{
		`INSERT INTO notes_fts (notes_fts) VALUES ('optimize')`,
		`INSERT INTO notes_fts (notes_fts) VALUES ('rebuild')`,
		`INSERT INTO notes_fts (notes_fts, rank) VALUES ('integrity-check', 1)`,
	} {
		if _, err := db.ExecContext(ctx, cmd); err != nil {
			t.Errorf("%s: %v", cmd, err)
		}
	}
	if err := db.IntegrityCheck(ctx); err != nil {
		t.Errorf("integrity after FTS churn: %v", err)
	}
}
