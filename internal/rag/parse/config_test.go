package parse

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestConfigValidate(t *testing.T) {
	good := DefaultConfig()
	good.WorkerPath = filepath.Join(os.TempDir(), "alixai-parser")
	if err := good.Validate(); err != nil {
		t.Fatalf("the default configuration with a path is invalid: %v", err)
	}
	if good.MaxOutputBytes > chunk.MaxDocumentBytes {
		t.Errorf("the default output cap %d is over the chunker's %d", good.MaxOutputBytes, chunk.MaxDocumentBytes)
	}
	cases := []struct {
		name string
		mod  func(*Config)
	}{
		{"no worker path", func(c *Config) { c.WorkerPath = "" }},
		{"relative worker path", func(c *Config) { c.WorkerPath = "alixai-parser" }},
		{"zero input cap", func(c *Config) { c.MaxInputBytes = 0 }},
		{"output over the chunker's limit", func(c *Config) { c.MaxOutputBytes = chunk.MaxDocumentBytes + 1 }},
		{"zero ratio", func(c *Config) { c.MaxExpansionRatio = 0 }},
		{"zero entries", func(c *Config) { c.MaxEntries = 0 }},
		{"shallow depth", func(c *Config) { c.MaxDepth = 2 }},
		{"no pages", func(c *Config) { c.MaxPages = 0 }},
		{"no blocks", func(c *Config) { c.MaxBlocks = 0 }},
		{"sub-second wall time", func(c *Config) { c.WallTimeout = time.Millisecond }},
		{"sub-second startup", func(c *Config) { c.StartupTimeout = time.Millisecond }},
		{"tiny memory", func(c *Config) { c.MemoryBytes = 1 << 20 }},
		{"negative cpu", func(c *Config) { c.CPUTime = -time.Second }},
		{"no concurrency", func(c *Config) { c.MaxConcurrent = 0 }},
		{"ocr switched on with no engine", func(c *Config) { c.OCR.Enabled = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := good
			c.mod(&cfg)
			err := cfg.Validate()
			if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Fatalf("err = %v, want validation_failed", err)
			}
		})
	}
}

func TestNewRunnerFailsClosed(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "missing-worker"))
	if _, err := NewRunner(cfg); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("a missing worker: %v", err)
	}
	cfg = testConfig(t.TempDir()) // a directory
	if _, err := NewRunner(cfg); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("a directory as the worker: %v", err)
	}
	// A file that is not a worker never gets a document: the handshake fails.
	notWorker := filepath.Join(t.TempDir(), "notaworker")
	if runtime.GOOS == "windows" {
		notWorker += ".exe"
		src, _ := os.ReadFile(prodWorker) // a real executable that is not speaking our protocol is the Go test binary
		_ = src
		notWorker = testExecutable(t)
	} else if err := os.WriteFile(notWorker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := testConfig(notWorker)
	c.StartupTimeout = 5 * time.Second
	r, err := NewRunner(c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.args = []string{"-hostile=silent"}
	if _, err := r.Parse(ctx(t), wire.FormatTXT, []byte("x")); err == nil {
		t.Error("a program that is not the worker parsed a document")
	}
}

// SEC-081: the directory a worker starts in is created owner-only.
func TestWorkerDirectoryOwnerOnly(t *testing.T) {
	r := newTestRunner(t)
	st, err := os.Stat(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsDir() {
		t.Fatal("not a directory")
	}
	if runtime.GOOS != "windows" {
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Errorf("worker directory mode = %o, want 700", perm)
		}
	}
	if !strings.Contains(filepath.Base(r.dir), "alixai-parse-") {
		t.Errorf("unexpected directory name %s", r.dir)
	}
	dir := r.dir
	r.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Close left the worker directory behind: %v", err)
	}
}

// Probe is the readiness check: it reports the confinement a real parse gets.
func TestProbeReportsConfinement(t *testing.T) {
	r := newTestRunner(t)
	rep, err := r.Probe(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Level != LevelL1 && rep.Level != LevelL2 {
		t.Errorf("level = %q", rep.Level)
	}
	if rep.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("platform = %q", rep.Platform)
	}
	if len(rep.Host) == 0 || len(rep.Worker) == 0 {
		t.Errorf("report names no mechanisms: %+v", rep)
	}
	for _, need := range wire.RequiredMechanisms(runtime.GOOS) {
		if !contains(rep.Worker, need) {
			t.Errorf("required mechanism %s missing from %v", need, rep.Worker)
		}
	}
	t.Logf("sandbox report: %+v", rep)
	if r.Spawned() < 1 {
		t.Error("Probe started no worker")
	}
}

func TestDocumentMapsOntoChunkDocument(t *testing.T) {
	r := newTestRunner(t)
	res := mustParse(t, r, wire.FormatCSV, []byte("a,b\n1,2\n3,4\n"))
	doc := res.Document("src_x")
	if doc.SourceID != "src_x" || doc.Text != res.Text || len(doc.Blocks) != len(res.Blocks) {
		t.Fatalf("doc = %+v", doc)
	}
	b := doc.Blocks[0]
	if b.Kind != chunk.BlockTable || !b.HasHeader || len(b.Rows) != 3 {
		t.Errorf("table block = %+v", b)
	}
	for i, row := range b.Rows {
		if row.Start != res.Blocks[0].Rows[i].Start || row.End != res.Blocks[0].Rows[i].End {
			t.Errorf("row %d span differs", i)
		}
	}
}
