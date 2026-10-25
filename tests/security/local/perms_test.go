package localsec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/cli"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/pkg/bridge"
)

// modeProblem judges the mode of one entry created under a data directory: directories 0700, secret files 0600. A public
// key file (.pub) holds nothing secret and is 0644 by design: it only must not be writable by anyone else.
func modeProblem(rel string, mode fs.FileMode) string {
	perm := mode.Perm()
	switch {
	case mode.IsDir():
		if perm != 0o700 {
			return fmt.Sprintf("%s: directory mode %04o, want 0700", rel, perm)
		}
	case mode&fs.ModeSymlink != 0:
		return fmt.Sprintf("%s: a symbolic link in the data directory", rel)
	case strings.HasSuffix(rel, ".pub"):
		if perm&0o022 != 0 {
			return fmt.Sprintf("%s: public key file mode %04o is writable by others", rel, perm)
		}
	default:
		if perm != 0o600 {
			return fmt.Sprintf("%s: file mode %04o, want 0600", rel, perm)
		}
	}
	return ""
}

// walkModes lists every entry under root, relative and slash-separated, and the mode problems among them.
func walkModes(root string) (entries []string, problems []string, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = "(root)"
		}
		info, ierr := os.Lstat(p)
		if ierr != nil {
			return ierr
		}
		entries = append(entries, rel)
		if msg := modeProblem(rel, info.Mode()); msg != "" {
			problems = append(problems, msg)
		}
		return nil
	})
	sort.Strings(entries)
	return entries, problems, err
}

// posix reports whether file mode bits mean anything on this platform.
func posix() bool { return runtime.GOOS != "windows" }

// needPOSIX skips a subtest that is about mode bits on a platform that has none.
func needPOSIX(t *testing.T) {
	t.Helper()
	if !posix() {
		t.Skip("Windows file modes carry no owner-only bits: this assertion runs on Linux and macOS")
	}
}

// syncBuf is a buffer a goroutine may write while the test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newEnv(stdout, stderr *syncBuf) *cli.Env {
	return &cli.Env{
		Stdin:     strings.NewReader(""),
		Stdout:    stdout,
		Stderr:    stderr,
		LookupEnv: func(string) (string, bool) { return "", false },
	}
}

// deployment is a throw-away configuration of the real service: a data directory that does not exist yet, a loopback
// listener on an ephemeral port, a LocalAI endpoint that nothing listens on, and a key file the test creates.
type deployment struct {
	dir, data, keyFile, cfg string
}

func newDeployment(t *testing.T) deployment {
	t.Helper()
	dir := t.TempDir()
	d := deployment{dir: dir, data: filepath.Join(dir, "data"), keyFile: filepath.Join(dir, "localai-api-key"), cfg: filepath.Join(dir, "alixai.yaml")}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.keyFile, []byte(hex.EncodeToString(raw)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d.keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf("data_dir: %q\nserver:\n  listen: 127.0.0.1:0\nlocalai:\n  endpoint: http://127.0.0.1:1\n  api_key_ref: %q\ninference:\n  models: []\n",
		filepath.ToSlash(d.data), "file:"+filepath.ToSlash(d.keyFile))
	if err := os.WriteFile(d.cfg, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// permissionWarningRE matches a warning or error line that is about who can access a file or directory.
var permissionWarningRE = regexp.MustCompile(`(?i)"level":"(WARN|ERROR)"[^\n]*(permissions?\b|accessible by|readable by|writable by|owner-only|chmod)`)

var listeningRE = regexp.MustCompile(`listening on (http://\S+)`)

// served is one run of `alixai serve`, in this process, through the real command line entry point.
type served struct {
	stdout, stderr *syncBuf
	done           chan int
	cancel         context.CancelFunc
	listening      bool
	exit           int
	exited         bool
}

// startServe runs `alixai serve -config cfg` and waits until it listens or exits.
func startServe(t *testing.T, cfg string) *served {
	t.Helper()
	s := &served{stdout: &syncBuf{}, stderr: &syncBuf{}, done: make(chan int, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() { s.done <- cli.Run(ctx, []string{"serve", "-config", cfg}, newEnv(s.stdout, s.stderr)) }()
	t.Cleanup(func() { s.stop(t) })
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		if listeningRE.MatchString(s.stdout.String()) {
			s.listening = true
			return s
		}
		select {
		case code := <-s.done:
			s.exit, s.exited = code, true
			return s
		case <-deadline.C:
			t.Fatalf("serve neither listened nor exited within 30 s\nstdout: %s\nstderr: %s", s.stdout.String(), s.stderr.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// stop cancels the run and waits for it to return.
func (s *served) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	if s.exited {
		return
	}
	select {
	case code := <-s.done:
		s.exit, s.exited = code, true
	case <-time.After(30 * time.Second):
		t.Error("serve did not stop within 30 s")
	}
}

// run executes one command line command and returns its exit code and output.
func run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := cli.Run(ctx, args, newEnv(&out, &errb))
	return code, out.String() + errb.String()
}

// SEC-081
func TestSecDataDirPermissions(t *testing.T) {
	t.Run("mode_detector_flags_planted_modes", func(t *testing.T) {
		for _, c := range []struct {
			rel  string
			mode fs.FileMode
			bad  bool
		}{
			{"data", fs.ModeDir | 0o700, false},
			{"data", fs.ModeDir | 0o755, true},
			{"data", fs.ModeDir | 0o750, true},
			{"data", fs.ModeDir | 0o777, true},
			{"alixai.db", 0o600, false},
			{"alixai.db", 0o644, true},
			{"alixai.db", 0o660, true},
			{"alixai.db", 0o666, true},
			{"keys/audit-checkpoint.key", 0o640, true},
			{"keys/audit-checkpoint.key", 0o400, true},
			{"keys/audit-checkpoint.pub", 0o644, false},
			{"keys/audit-checkpoint.pub", 0o600, false},
			{"keys/audit-checkpoint.pub", 0o666, true},
			{"keys/audit-checkpoint.pub", 0o664, true},
			{"link", fs.ModeSymlink | 0o777, true},
		} {
			if got := modeProblem(c.rel, c.mode) != ""; got != c.bad {
				t.Errorf("%s %v: flagged=%v, want %v", c.rel, c.mode, got, c.bad)
			}
		}
	})

	t.Run("first_start_creates_everything_owner_only", func(t *testing.T) {
		d := newDeployment(t)
		s := startServe(t, d.cfg)
		if !s.listening {
			t.Fatalf("serve did not start (exit %d)\n%s", s.exit, s.stderr.String())
		}
		// what the operator creates next, while the service runs: the signing keys
		if code, out := run(t, "keys", "init", "-config", d.cfg); code != cli.ExitOK {
			t.Fatalf("keys init = %d\n%s", code, out)
		}
		backupDir := filepath.Join(d.dir, "backup")
		if code, out := run(t, "backup", "create", "-config", d.cfg, "-dir", backupDir); code != cli.ExitOK {
			t.Fatalf("backup create = %d\n%s", code, out)
		}
		entries, problems, err := walkModes(d.data)
		if err != nil {
			t.Fatal(err)
		}
		// the scan must have seen the things that matter, or it proves nothing
		for _, want := range []string{"alixai.db", "admin-endpoint.json", "files", "keys", "keys/audit-checkpoint.key", "keys/backup.key"} {
			found := false
			for _, e := range entries {
				found = found || e == want
			}
			if !found {
				t.Errorf("first start did not create %s (entries: %v): the check cannot see it", want, entries)
			}
		}
		if posix() {
			for _, p := range problems {
				t.Error(p)
			}
		}
		bEntries, bProblems, err := walkModes(backupDir)
		if err != nil || len(bEntries) < 3 {
			t.Fatalf("the backup tree holds %v (%v)", bEntries, err)
		}
		if posix() {
			for _, p := range bProblems {
				t.Error("backup: " + p)
			}
		}
		// the endpoint file holds the operator credential: loosened, the reader refuses it
		if !posix() {
			s.stop(t)
			return
		}
		if err := os.Chmod(filepath.Join(d.data, "admin-endpoint.json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := gateway.ReadAdminEndpoint(d.data); err == nil {
			t.Error("a world-readable admin-endpoint.json was still used")
		}
		s.stop(t)
	})

	t.Run("the_walk_sees_a_planted_loose_file", func(t *testing.T) {
		needPOSIX(t)
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "keys"), 0o700); err != nil {
			t.Fatal(err)
		}
		loose := filepath.Join(root, "keys", "planted.key")
		if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(loose, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ps, err := walkModes(root); err != nil || len(ps) < 2 { // the root of a t.TempDir is 0700 or looser; the file is 0644
			t.Errorf("the walk missed the planted 0644 file: %v %v", ps, err)
		}
	})

	t.Run("loose_key_and_secret_files_are_refused", func(t *testing.T) {
		needPOSIX(t)
		// The control first: the same configuration with an owner-only key file starts.
		d := newDeployment(t)
		s := startServe(t, d.cfg)
		if !s.listening {
			t.Fatalf("the control run did not start (exit %d)\n%s", s.exit, s.stderr.String())
		}
		s.stop(t)
		if code, out := run(t, "keys", "init", "-config", d.cfg); code != cli.ExitOK {
			t.Fatalf("keys init = %d\n%s", code, out)
		}

		// LocalAI's API key file mode is checked by the adapter (adapters/localai secret tests); this suite may not import
		// the adapter (the M0 gate), and serve builds it only once a verified model is configured.

		// The checkpoint signing key readable by others: serve refuses, and so does keys init.
		key := filepath.Join(d.data, "keys", "audit-checkpoint.key")
		if err := os.Chmod(key, 0o640); err != nil {
			t.Fatal(err)
		}
		if s := startServe(t, d.cfg); s.listening || s.exit == cli.ExitOK {
			t.Errorf("serve started with a group-readable checkpoint signing key (listening=%v exit=%d)", s.listening, s.exit)
		}
		if code, _ := run(t, "keys", "init", "-config", d.cfg); code == cli.ExitOK {
			t.Error("keys init accepted a group-readable key file")
		}
		if err := os.Chmod(key, 0o600); err != nil {
			t.Fatal(err)
		}
		if s := startServe(t, d.cfg); !s.listening {
			t.Errorf("the control after restoring 0600 did not start (exit %d)\n%s", s.exit, s.stderr.String())
		}
	})

	t.Run("loose_data_directory_warns_or_refuses", func(t *testing.T) {
		needPOSIX(t)
		d := newDeployment(t)
		if err := os.MkdirAll(d.data, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d.data, 0o755); err != nil {
			t.Fatal(err)
		}
		s := startServe(t, d.cfg)
		// the detector first: it matches a warning about permissions and not the unrelated lines a start logs
		for line, want := range map[string]bool{
			`{"level":"WARN","msg":"/d/data is accessible by other users (mode 0755); chmod 700 it"}`:                  true,
			`{"level":"WARN","msg":"the data directory permissions are too open"}`:                                     true,
			`{"level":"ERROR","msg":"the inference path is off","reason":"no model is configured"}`:                    false,
			`{"level":"INFO","msg":"accessible by other users"}`:                                                       false,
			`{"level":"WARN","msg":"rag: ingestion starts without a ready parser worker","err_text":"not executable"}`: false,
		} {
			if permissionWarningRE.MatchString(line) != want {
				t.Fatalf("the warning detector is wrong for %s", line)
			}
		}
		warned := permissionWarningRE.MatchString(s.stderr.String())
		refused := !s.listening && s.exit != cli.ExitOK
		if !refused && !warned {
			t.Errorf("serve started over a 0755 data directory without a word (listening=%v)", s.listening)
		}
	})

	t.Run("native_socket_is_owner_only", func(t *testing.T) {
		needPOSIX(t)
		dir, err := os.MkdirTemp("", "as") // short: a socket path is limited to about 100 bytes
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sock := filepath.Join(dir, "run", "a.sock")
		l, err := bridge.ListenUnix(sock)
		if err != nil {
			t.Fatalf("ListenUnix: %v", err)
		}
		defer l.Close()
		si, err := os.Lstat(sock)
		if err != nil {
			t.Fatal(err)
		}
		if si.Mode()&fs.ModeSocket == 0 {
			t.Errorf("%s is not a socket: %v", sock, si.Mode())
		}
		if si.Mode().Perm() != 0o600 {
			t.Errorf("socket mode %04o, want 0600", si.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(sock))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("the socket directory the bridge created is %04o, want 0700", di.Mode().Perm())
		}
		// a directory other users can write to is refused: they could replace the socket
		loose := filepath.Join(dir, "loose")
		if err := os.Mkdir(loose, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(loose, 0o777); err != nil {
			t.Fatal(err)
		}
		if l2, err := bridge.ListenUnix(filepath.Join(loose, "b.sock")); err == nil {
			_ = l2.Close()
			t.Error("ListenUnix accepted a world-writable directory")
		}
		var _ net.Listener = l
	})

	if !posix() {
		t.Run("windows_acl_is_a_missing_control", func(t *testing.T) {
			t.Skip("DEFECT G5 (Windows half): nothing in the product creates or verifies a restrictive ACL on the data directory, the database, " +
				"the key files or the endpoint file; `alixai keys init` and `alixai service install` only print an instruction to do it by hand. " +
				"No stand-in is asserted (the backlog expects restrictive ACLs on Windows; internal/store, internal/trust and internal/cli need the control).")
		})
	}
}
