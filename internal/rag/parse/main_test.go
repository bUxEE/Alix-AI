package parse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The tests run real worker processes. TestMain builds the production worker
// and a second worker with the fault-injection hooks (-tags parsertest) once
// into a temporary directory. A cross-compiled test binary run elsewhere (for
// example in a Linux container) points at prebuilt workers instead with
// ALIXAI_PARSER_BIN and ALIXAI_PARSER_FAULT_BIN.
var (
	prodWorker  string
	faultWorker string
)

func TestMain(m *testing.M) {
	// The test binary doubles as a hostile stand-in for the worker (hostile_test.go):
	// the host is started with -hostile=<mode> and must survive what it does.
	for _, a := range os.Args[1:] {
		if mode, ok := strings.CutPrefix(a, "-hostile="); ok {
			os.Exit(hostileMain(mode))
		}
	}
	os.Exit(run(m))
}

func testExecutable(t testing.TB) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func run(m *testing.M) int {
	prodWorker, faultWorker = os.Getenv("ALIXAI_PARSER_BIN"), os.Getenv("ALIXAI_PARSER_FAULT_BIN")
	if prodWorker == "" || faultWorker == "" {
		dir, err := os.MkdirTemp("", "alixai-parser-test-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		ext := ""
		if runtime.GOOS == "windows" {
			ext = ".exe"
		}
		prodWorker = filepath.Join(dir, "alixai-parser"+ext)
		faultWorker = filepath.Join(dir, "alixai-parser-fault"+ext)
		if err := goBuild(prodWorker); err != nil {
			fmt.Fprintln(os.Stderr, "building the worker:", err)
			return 1
		}
		if err := goBuild(faultWorker, "-tags", "parsertest"); err != nil {
			fmt.Fprintln(os.Stderr, "building the fault-injection worker:", err)
			return 1
		}
	}
	// A new executable's first start can take tens of seconds under a real-time virus scanner (26.5 s was
	// measured on the dev box), and that delay would otherwise land inside a test's own wall-time limit
	// (TestParserMemoryLimitKilled). Pay it here, once, before any test starts its clock. Nothing is retried:
	// each worker is started a single time and the wait is bounded.
	for _, w := range []string{prodWorker, faultWorker} {
		if err := warmStart(w); err != nil {
			fmt.Fprintln(os.Stderr, "warm start:", err)
			return 1
		}
	}
	return m.Run()
}

// warmStartLimit bounds the warm start of one worker. It is far above the slowest first start seen.
const warmStartLimit = 3 * time.Minute

// warmStart starts the worker executable once with an empty environment and an empty stdin, as the host
// does. The worker reads no Init frame and exits with its protocol-error code at once; the exit status is
// not the point, the first start of the file is. It fails only when the process cannot be started or does
// not end within warmStartLimit.
func warmStart(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), warmStartLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Env = []string{}
	cmd.Stdin = strings.NewReader("")
	start := time.Now()
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return fmt.Errorf("%s did not start: %w", filepath.Base(path), err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s did not end within %s", filepath.Base(path), warmStartLimit)
	}
	fmt.Fprintf(os.Stderr, "warm start of %s took %s\n", filepath.Base(path), time.Since(start).Round(time.Millisecond))
	return nil
}

func goBuild(out string, extra ...string) error {
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	args := append([]string{"build"}, extra...)
	args = append(args, "-o", out, "./cmd/alixai-parser")
	cmd := exec.Command(goTool(), args...)
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
	return filepath.Join(runtime.GOROOT(), "bin", "go")
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
