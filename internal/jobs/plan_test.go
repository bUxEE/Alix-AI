package jobs

import (
	"strings"
	"testing"
)

// The queue's cost claims (an idle poll is an index probe, not a scan; a
// lease touches one row) rest on SQLite using the partial indexes. A partial
// index is only used when the query repeats its WHERE terms literally, so a
// harmless-looking rewrite of a query can silently turn it into a table scan.
// This test pins the plans of the hot statements.
func TestQueryPlansUseTheIndexes(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	// Plans depend on table statistics only marginally; a few rows keep the
	// planner honest without making it prefer a scan of a tiny table.
	for i := 0; i < 50; i++ {
		h.enqueue(a, typeOf(ClassIngest), func(r *EnqueueRequest) {
			if i%2 == 0 {
				r.IdempotencyKey = string(rune('a' + i%26))
			}
		})
	}
	if _, err := h.db.ExecContext(bg, `ANALYZE`); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		query string
		args  []any
		want  []string // every fragment must appear in the plan
		deny  []string // none may appear
	}{
		{"lease head", `SELECT seq FROM jobs WHERE state = 'pending' AND class = ? AND run_at <= ? ORDER BY seq LIMIT 1`,
			[]any{5, 1}, []string{"jobs_ready"}, []string{"SCAN jobs", "TEMP B-TREE"}},
		{"ready probe with types", `SELECT 1 FROM jobs WHERE state = 'pending' AND class = ? AND run_at <= ? AND type IN (?, ?) LIMIT 1`,
			[]any{5, 1, "a", "b"}, []string{"jobs_ready"}, []string{"SCAN jobs"}},
		{"yield probe", `SELECT 1 FROM jobs WHERE state = 'pending' AND class <= ? AND run_at <= ? LIMIT 1`,
			[]any{2, 1}, []string{"jobs_ready"}, []string{"SCAN jobs"}},
		{"depth count", `SELECT count(*) FROM jobs WHERE state = 'pending' AND class = ?`,
			[]any{5}, []string{"COVERING INDEX jobs_ready"}, []string{"SCAN jobs"}},
		{"idempotency lookup", `SELECT id FROM jobs WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND idempotency_key = ?`,
			[]any{"app", "tenantA", "ws1", "a"}, []string{"jobs_idempotency"}, []string{"SCAN jobs"}},
		{"tenant counts", `SELECT state, count(*) FROM jobs WHERE state IN ('pending', 'running') AND application_id = ? AND tenant_id = ? AND class = ? GROUP BY state`,
			[]any{"app", "tenantA", 5}, []string{"jobs_tenant_active"}, []string{"SCAN jobs"}},
		{"expired leases", `SELECT 1 FROM jobs WHERE state = 'running' AND lease_until <= ?`,
			[]any{1}, []string{"jobs_lease"}, []string{"SCAN jobs"}},
		{"expired deadlines", `UPDATE jobs SET state = 'failed' WHERE state = 'pending' AND deadline_at IS NOT NULL AND deadline_at <= ?`,
			[]any{1}, []string{"jobs_deadline"}, []string{"SCAN jobs"}},
		{"retention", `SELECT seq FROM jobs WHERE state IN ('succeeded', 'failed', 'cancelled') AND finished_at <= ? ORDER BY finished_at LIMIT 10`,
			[]any{1}, []string{"jobs_finished"}, []string{"SCAN jobs"}},
		{"job by id and scope", `SELECT id FROM jobs WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND id = ?`,
			[]any{"app", "tenantA", "ws1", "x"}, []string{"sqlite_autoindex_jobs_1"}, []string{"SCAN jobs"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := h.db.QueryContext(bg, `EXPLAIN QUERY PLAN `+tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			joined := strings.Join(plan, "\n")
			t.Logf("plan:\n%s", joined)
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("plan lacks %q:\n%s", w, joined)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(joined, d) {
					t.Errorf("plan contains %q:\n%s", d, joined)
				}
			}
		})
	}
}
