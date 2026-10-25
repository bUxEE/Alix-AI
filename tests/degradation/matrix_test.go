package degradation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// section18 is the degradation table (failure, required behaviour, and the test that asserts it; summarised in
// docs/architecture/overview.md section 6) as the tests below were written against it. The table is a contract (task
// M5-06): every row has its test, and in a checkout that holds the planning document the table is compared with it
// row by row, so a changed, added or removed row fails until the tests are reviewed against the new text.
var section18 = []struct{ failure, behaviour, test string }{
	{"LLM unavailable", "Application and search remain usable; return a clear local failure; never execute from partial model output", "TestDegradeLLMUnavailable"},
	{"Embedding model unavailable", "Use authorized FTS/exact search and label reduced semantic recall", "TestDegradeEmbeddingUnavailable"},
	{"Reranker unavailable", "Use fused retrieval order", "TestDegradeRerankerUnavailable"},
	{"STT unavailable", "Text input remains available", "TestDegradeSTTUnavailable"},
	{"TTS unavailable", "Return verified text", "TestDegradeTTSUnavailable"},
	{"Source unavailable", "Mark affected source stale/unavailable; do not pretend it was checked", "TestDegradeSourceUnavailable"},
	{"MCP/API adapter unavailable", "Preserve session and prepared-state safety; report no commit", "TestDegradeAdapterUnavailable"},
	{"Commit response uncertain", "Query idempotency/status before retrying", "TestDegradeCommitUncertain"},
	{"SQLite/index corruption", "Isolate damage, restore metadata backup or rebuild derived index", "TestDegradeIndexCorruption"},
	{"LocalAI crash", "Supervisor restarts it with backoff; in-flight action remains uncommitted unless receipt proves otherwise", "TestDegradeLocalAICrash"},
	{"Disk pressure", "Pause ingestion before interactive or transactional storage is endangered", "TestDegradeDiskPressure"},
}

func TestDegradationMatrixCoversSection18(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "AlixAI_Standalone_Master_Plan.md"))
	havePlan := err == nil
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var rows [][2]string
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "## 18."):
			in = true
		case in && strings.HasPrefix(line, "## "):
			in = false
		case in && strings.HasPrefix(line, "|"):
			cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
			if len(cells) != 2 {
				continue
			}
			f, b := strings.TrimSpace(cells[0]), strings.TrimSpace(cells[1])
			if f == "Failure" || strings.HasPrefix(f, "---") {
				continue
			}
			rows = append(rows, [2]string{f, b})
		}
	}
	if havePlan && len(rows) != len(section18) {
		t.Fatalf("section 18 has %d rows, the matrix tests were written for %d: review the tests against the plan", len(rows), len(section18))
	}

	declared := map[string]bool{}
	files, _ := filepath.Glob("*_test.go")
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				declared[fn.Name.Name] = true
			}
		}
	}
	for i, want := range section18 {
		if havePlan && (rows[i][0] != want.failure || strings.TrimSuffix(rows[i][1], ".") != want.behaviour) {
			t.Errorf("row %d of section 18 is %q / %q, the tests assert %q / %q", i+1, rows[i][0], rows[i][1], want.failure, want.behaviour)
		}
		if !declared[want.test] {
			t.Errorf("row %q has no test %s", want.failure, want.test)
		}
	}
}
