package isolationsec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse"
)

// parserWorker is the production parser worker every test runs documents
// through. TestMain builds it once; ALIXAI_PARSER_BIN points at a prebuilt one
// for a cross-compiled test binary.
var parserWorker string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	parserWorker = os.Getenv("ALIXAI_PARSER_BIN")
	if parserWorker == "" {
		dir, err := os.MkdirTemp("", "alixai-isolationsec-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		name := "alixai-parser"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		parserWorker = filepath.Join(dir, name)
		if err := buildWorker(parserWorker); err != nil {
			fmt.Fprintln(os.Stderr, "building the parser worker:", err)
			return 1
		}
	}
	if err := warmWorker(parserWorker); err != nil {
		fmt.Fprintln(os.Stderr, "starting the parser worker:", err)
		return 1
	}
	return m.Run()
}

// warmWorker starts the worker once and ends it, so that the first start of a
// freshly built executable (which an antivirus scans, taking many seconds on a
// dev box) is not charged to the first test. Limits are enforced the same either
// way.
func warmWorker(path string) error {
	cfg := parse.DefaultConfig()
	cfg.WorkerPath = path
	cfg.StartupTimeout = 3 * time.Minute
	r, err := parse.NewRunner(cfg)
	if err != nil {
		return err
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	start := time.Now()
	if _, err := r.Probe(ctx); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "parser worker ready in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}

// buildWorker builds the worker the way a release does: stripped and
// path-trimmed, pure Go.
func buildWorker(out string) error {
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	cmd := exec.Command(goTool(), "build", "-ldflags=-s -w", "-trimpath", "-o", out, "./cmd/alixai-parser")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	start := time.Now()
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, b)
	}
	fmt.Fprintf(os.Stderr, "built %s in %s\n", filepath.Base(out), time.Since(start).Round(time.Millisecond))
	return nil
}

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(runtime.GOROOT(), "bin", name)
}

// moduleRoot finds the directory with go.mod above the test's directory.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
