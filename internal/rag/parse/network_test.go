package parse

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// goList runs go list in the module root for a target platform.
func goList(t *testing.T, goos, goarch, format, pkg string, extra ...string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go tool here (a cross-compiled test run); the same checks run wherever the tests are built")
	}
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"list"}, extra...)
	args = append(args, "-f", format, pkg)
	cmd := exec.Command(goTool(), args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list (%s/%s): %v\n%s", goos, goarch, err, out)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// SEC-016: the parser has no network and inherits no secrets.
//
// What this test proves, by platform:
//
//   - Everywhere, as a property of the code: the production worker's dependency
//     graph contains no network package (net, net/http, crypto/tls, ...) and no
//     process-spawning package (os/exec), for every supported OS and CPU, and
//     none of the worker's own code calls a socket primitive. The worker cannot
//     dial because it has no code that dials. This is a code property, not an
//     operating system guarantee.
//   - On Linux with Landlock and seccomp available (sandbox level L2): the
//     kernel also refuses socket(2) in a worker that were to contain such code,
//     which the dynamic case demonstrates with a fault-injection worker that does
//     link net and tries to connect.
//   - On Windows and macOS: no OS enforcement of "no network" exists in this
//     version (ADR 0014). The dynamic case records that a net-linked worker can
//     connect there, and asserts the host does not claim otherwise: a level of L2
//     is only ever reported when the connection was in fact refused.
func TestParserHasNoNetwork(t *testing.T) {
	forbidden := func(p string) bool {
		switch {
		case p == "net" || strings.HasPrefix(p, "net/"):
			return true
		case p == "crypto/tls" || p == "crypto/x509" || p == "os/exec" || p == "plugin":
			return true
		case strings.HasPrefix(p, "golang.org/x/net/") && p != "golang.org/x/net/html" && p != "golang.org/x/net/html/atom":
			return true
		}
		return false
	}
	platforms := [][2]string{{"windows", "amd64"}, {"windows", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}}
	t.Run("import graph", func(t *testing.T) {
		for _, p := range platforms {
			deps := goList(t, p[0], p[1], "{{.ImportPath}}", "./cmd/alixai-parser", "-deps")
			if len(deps) < 50 {
				t.Fatalf("%s/%s: only %d packages listed; the listing is not seeing the worker", p[0], p[1], len(deps))
			}
			for _, d := range deps {
				if forbidden(d) {
					t.Errorf("%s/%s: the worker depends on %s", p[0], p[1], d)
				}
			}
			// The worker must not reach the launcher either.
			for _, d := range deps {
				if d == "github.com/buxee/alix-ai/internal/rag/parse" {
					t.Errorf("%s/%s: the worker imports the launcher package", p[0], p[1])
				}
			}
		}
	})

	t.Run("no socket primitive in the worker's own code", func(t *testing.T) {
		primitives := map[string]bool{
			"Socket": true, "Connect": true, "Bind": true, "Listen": true, "Accept": true, "Accept4": true,
			"Sendto": true, "Recvfrom": true, "Sendmsg": true, "Recvmsg": true, "Socketpair": true,
			"WSASocket": true, "WSAStartup": true, "WSAConnect": true, "ConnectEx": true, "AcceptEx": true,
			"GetAddrInfoW": true, "DnsQuery": true,
		}
		for _, p := range platforms {
			lines := goList(t, p[0], p[1], "{{.ImportPath}}|{{.Dir}}|{{join .GoFiles \",\"}}", "./cmd/alixai-parser", "-deps")
			checked := 0
			for _, l := range lines {
				importPath, rest, _ := strings.Cut(l, "|")
				dir, files, _ := strings.Cut(rest, "|")
				// Identify the project's own packages by IMPORT PATH, never by directory name: the checkout directory
				// may be spelled alixAI, alixai or a worktree name. The standard library and third-party code are covered
				// by the import graph above.
				if !strings.HasPrefix(importPath, "github.com/buxee/alix-ai/") {
					continue
				}
				for _, f := range strings.Split(files, ",") {
					if f == "" {
						continue
					}
					af, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, f), nil, 0)
					if err != nil {
						t.Fatal(err)
					}
					checked++
					ast.Inspect(af, func(n ast.Node) bool {
						sel, ok := n.(*ast.SelectorExpr)
						if !ok {
							return true
						}
						if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "syscall" || id.Name == "windows" || id.Name == "unix") && primitives[sel.Sel.Name] {
							t.Errorf("%s/%s: %s/%s calls %s.%s", p[0], p[1], filepath.Base(dir), f, id.Name, sel.Sel.Name)
						}
						return true
					})
				}
			}
			if checked < 15 {
				t.Errorf("%s/%s: scanned only %d of the worker's own files", p[0], p[1], checked)
			}
		}
	})

	t.Run("production worker is built without the fault hooks", func(t *testing.T) {
		r := newTestRunner(t)
		for _, f := range []string{"x-net", "x-hang", "x-crash", "x-env", "x-spawn", "x-file"} {
			_, err := r.parse(ctx(t), f, []byte("127.0.0.1:1"))
			if code(err) != "internal" && code(err) != "validation_failed" {
				t.Errorf("format %q: err = %v", f, err)
			}
			if err == nil {
				t.Errorf("the production worker accepted the fault format %q", f)
			}
		}
	})

	t.Run("what the operating system does here", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("no loopback listener: %v", err)
		}
		defer ln.Close()
		accepted := make(chan struct{}, 4)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				accepted <- struct{}{}
				c.Close()
			}
		}()

		r := newFaultRunner(t)
		res, err := r.parse(ctx(t), "x-net", []byte(ln.Addr().String()))
		if err != nil {
			t.Fatalf("the probe failed: %v", err)
		}
		outcome := res.Text
		connected := false
		select {
		case <-accepted:
			connected = true
		case <-time.After(300 * time.Millisecond):
		}
		t.Logf("platform %s, sandbox level %s, host %v, worker %v, missing %v", res.Sandbox.Platform, res.Sandbox.Level, res.Sandbox.Host, res.Sandbox.Worker, res.Sandbox.Missing)
		t.Logf("a worker that links net and dials 127.0.0.1: %q (listener saw a connection: %v)", outcome, connected)

		networkDenied := contains(res.Sandbox.Worker, wire.SeccompNet)
		switch {
		case networkDenied:
			if connected || !strings.Contains(outcome, "dial failed") {
				t.Errorf("seccomp was applied but the connection was not refused: %q", outcome)
			}
		case res.Sandbox.Level == LevelL2:
			t.Errorf("the host reports level L2 without a network-denying mechanism applied")
		default:
			if runtime.GOOS == "linux" {
				t.Logf("UNTESTED here: this kernel or container did not allow seccomp (%v)", res.Sandbox.Missing)
			} else {
				t.Logf("not OS-enforced on %s (ADR 0014): the guarantee is the import-graph check above", runtime.GOOS)
			}
			if res.Sandbox.Level != LevelL1 {
				t.Errorf("level = %q, want L1 where nothing denies the network", res.Sandbox.Level)
			}
		}
	})
}

// A worker inherits no secret from the core, and starts in an empty directory.
func TestParserInheritsNothing(t *testing.T) {
	secrets := map[string]string{
		"ALIXAI_TEST_SECRET":    "sentinel-secret-value-1",
		"AWS_SECRET_ACCESS_KEY": "sentinel-secret-value-2",
		"OPENAI_API_KEY":        "sentinel-secret-value-3",
		"HOME":                  "sentinel-home",
		"PATH":                  "sentinel-path",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	r := newFaultRunner(t)
	res, err := r.parse(ctx(t), "x-env", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range secrets {
		if strings.Contains(res.Text, v) || strings.Contains(res.Text, k+"=") {
			t.Errorf("the worker can see %s:\n%s", k, res.Text)
		}
	}
	// Windows adds SYSTEMROOT itself; nothing else may be there.
	var envLine string
	for _, l := range strings.Split(res.Text, "\n") {
		if strings.HasPrefix(l, "env=") {
			envLine = l
		}
	}
	if envLine == "" {
		t.Fatalf("no env line in %q", res.Text)
	}
	for _, name := range []string{"HOME=", "PATH=", "TMP=", "TEMP=", "USERPROFILE="} {
		if strings.Contains(strings.ToUpper(envLine), name) {
			t.Errorf("the worker's environment holds %s: %s", name, envLine)
		}
	}
	// It runs in the Runner's empty directory, not the core's working directory.
	cwd, _ := os.Getwd()
	for _, l := range strings.Split(res.Text, "\n") {
		if wd, ok := strings.CutPrefix(l, "wd="); ok {
			if strings.Contains(wd, filepath.Base(cwd)) && strings.Contains(wd, "parse") && !strings.Contains(wd, "alixai-parse-") {
				t.Errorf("the worker started in the core's directory: %s", wd)
			}
			if !strings.Contains(wd, "alixai-parse-") {
				t.Errorf("wd = %s, want the Runner's empty directory", wd)
			}
		}
		if args, ok := strings.CutPrefix(l, "args="); ok && strings.Count(args, ",")+strings.Count(args, " ") > 0 && !bytes.Contains([]byte(args), []byte(filepath.Base(faultWorker))) {
			t.Errorf("unexpected arguments: %s", args)
		}
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("the worker directory is not empty: %v %v", entries, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	_ = names
}
