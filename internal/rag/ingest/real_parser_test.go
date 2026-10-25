package ingest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse"
	"github.com/buxee/alix-ai/internal/rag/source"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// buildWorker builds the production parser worker the way a release does:
// stripped (-s -w) and path-trimmed (-trimpath), pure Go.
func buildWorker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "alixai-parser")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("no go.mod above the test directory")
		}
		root = parent
	}
	goTool := "go"
	if p, err := exec.LookPath("go"); err == nil {
		goTool = p
	} else {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(goTool, "build", "-ldflags=-s -w", "-trimpath", "-o", out, "./cmd/alixai-parser")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	begin := time.Now()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the worker: %v\n%s", err, b)
	}
	t.Logf("built the worker in %s", time.Since(begin).Round(time.Millisecond))
	return out
}

// TestIngestWithTheRealParserWorker runs the pipeline over the real sandboxed
// worker: the readiness probe at service start (which pays the first start of a
// freshly built executable, 26.5 s under Avast on the Windows dev box), then
// documents of several formats parsed one process each, chunked, indexed and
// searchable. Skipped with -short: it builds and starts a process.
func TestIngestWithTheRealParserWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts the parser worker")
	}
	worker := buildWorker(t)
	f := newFixture(t, func(o *fixtureOpts) {
		o.config = func(c *Config) {
			c.Parser.WorkerPath = worker
			c.Parser.StartupTimeout = 90 * time.Second
			c.Parser.ProbeTimeout = 3 * time.Minute
		}
	})
	runner, err := parse.NewRunner(f.cfg.ParseConfig())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() { runner.Close() })
	f.svc.parser = NewRunnerParser(runner)

	begin := time.Now()
	rd, err := f.svc.Start(bg)
	if err != nil {
		t.Fatalf("Start: %v (readiness %+v)", err, rd)
	}
	t.Logf("sandbox level %s on %s; the first start took %s (limit %s)", rd.Level, rd.Platform, time.Since(begin).Round(time.Millisecond), rd.StartupTimeout)
	for _, w := range rd.Warnings {
		t.Logf("readiness: %s", w)
	}
	if rd.Level != parse.LevelL1 && rd.Level != parse.LevelL2 {
		t.Fatalf("level = %q", rd.Level)
	}

	f.def = f.mkDef(f.srcID, "tenant-1", "ws-1", f.root, func(d *ap.RAGSource) {
		d.Formats = append(d.Formats, source.MIMEHTML, source.MIMECSV)
	})
	f.reg.set(f.def)
	f.write("guide.md", "# Guide\n\nThe harbour lights flicker at dusk and the ferry leaves at six.\n\n## Fares\n\nA single crossing costs four coins.")
	f.write("notes.txt", "Plain notes about lighthouses and their keepers.\n\nThe keeper wrote down every ship that passed.")
	f.write("page.html", "<html><body><h1>Tides</h1><p>High tide arrives at noon.</p><p style=\"display:none\">hiddenpayload</p></body></html>")
	f.write("table.csv", "port,depth\nnorthhaven,12\nsouthbay,9\n")

	begin = time.Now()
	j := f.sync()
	t.Logf("four documents parsed, chunked and indexed in %s", time.Since(begin).Round(time.Millisecond))
	mustEqual(t, "sync", j.State, "succeeded")
	st := f.versionStates()
	if st[vIndexed] != 4 {
		t.Fatalf("versions: %v\n%s", st, f.dumpJobs())
	}
	mustEqual(t, "state", f.status().State, rag.SourceStatusStateReady)
	for _, q := range []struct{ text, doc string }{
		{"ferry", "guide.md"}, {"lighthouses", "notes.txt"}, {"tide", "page.html"}, {"northhaven", "table.csv"},
	} {
		hits := f.search(q.text)
		if len(hits) == 0 {
			t.Fatalf("%q (in %s) is not searchable", q.text, q.doc)
		}
		mustEqual(t, fmt.Sprintf("%q is found in %s", q.text, q.doc), hits[0].DocumentID, documentID(f.srcID, q.doc))
	}
	// Hidden text is flagged by the worker and kept out of search and embeddings.
	mustEqual(t, "hidden text in the default search", len(f.search("hiddenpayload")), 0)
	for _, p := range f.emb.inner.Prompts() {
		for _, in := range p.Embed.Inputs {
			if contains(in, "hiddenpayload") {
				t.Fatal("hidden text reached the embedding model")
			}
		}
	}
	// The generation records the real parser's identity and version.
	g := f.activeGen()
	mustEqual(t, "parser component", g.Components.Parser, parse.Component())
	t.Logf("parser worker spawned %d processes for %d documents", runner.Spawned(), 4)
}
