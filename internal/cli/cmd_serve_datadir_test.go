package cli

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/fsperm/fspermtest"
	"github.com/buxee/alix-ai/internal/store"
)

// hardeningInfo is a fs.FileInfo with a chosen mode, so the mode check is tested
// the same way on every OS (Windows reports 0777 for every directory).
type hardeningInfo struct {
	mode fs.FileMode
}

func (hardeningInfo) Name() string        { return "x" }
func (hardeningInfo) Size() int64         { return 0 }
func (i hardeningInfo) Mode() fs.FileMode { return i.mode }
func (hardeningInfo) ModTime() time.Time  { return time.Time{} }
func (i hardeningInfo) IsDir() bool       { return i.mode.IsDir() }
func (hardeningInfo) Sys() any            { return nil }

// SEC-081: serve warns about an existing data directory or database that other
// users can access: by mode bits, and on Windows by the ACL (M2).
func TestServeWarnsOnALooseDataDir(t *testing.T) {
	dirMode := func(p fs.FileMode) fs.FileInfo { return hardeningInfo{mode: fs.ModeDir | p} }
	fileMode := func(p fs.FileMode) fs.FileInfo { return hardeningInfo{mode: p} }

	t.Run("findings by mode", func(t *testing.T) {
		cases := []struct {
			name     string
			dir, db  fs.FileInfo
			wantWarn []string
		}{
			{"owner-only is silent", dirMode(0o700), fileMode(0o600), nil},
			{"no database yet", dirMode(0o700), nil, nil},
			{"world-readable directory", dirMode(0o755), fileMode(0o600), []string{"accessible by other users (mode 0755); chmod 700"}},
			{"group-readable directory", dirMode(0o750), nil, []string{"mode 0750"}},
			{"group-writable directory", dirMode(0o770), nil, []string{"mode 0770"}},
			{"loose database in a tight directory", dirMode(0o700), fileMode(0o644), []string{"database file in /d/data is accessible by other users (mode 0644)"}},
			{"both loose", dirMode(0o755), fileMode(0o644), []string{"mode 0755", "mode 0644"}},
		}
		for _, c := range cases {
			got := hardeningDataDirFindings("/d/data", c.dir, c.db)
			if len(got) != len(c.wantWarn) {
				t.Errorf("%s: %d findings %q, want %d", c.name, len(got), got, len(c.wantWarn))
				continue
			}
			for i, want := range c.wantWarn {
				if !strings.Contains(got[i], want) {
					t.Errorf("%s: finding %d = %q, want it to contain %q", c.name, i, got[i], want)
				}
			}
		}
	})

	t.Run("windows checks the ACL", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("Windows ACLs")
		}
		root := t.TempDir()
		// Open to Authenticated Users, as a directory under E:\ is by default: a
		// finding for the directory and for the database that inherits it.
		loose := filepath.Join(root, "loose")
		if err := os.Mkdir(loose, 0o700); err != nil {
			t.Fatal(err)
		}
		fspermtest.Loosen(t, loose)
		if err := os.WriteFile(filepath.Join(loose, store.DefaultFileName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		got := hardeningWindowsFindings(loose)
		if len(got) != 2 || !strings.Contains(got[0], "data directory is not owner only") || !strings.Contains(got[1], "database file") {
			t.Fatalf("findings for an inheriting directory = %q", got)
		}
		// Made by the store: owner only, silent.
		tight := filepath.Join(root, "tight")
		db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(tight, store.DefaultFileName), Migrations: store.NewRegistry()})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		if got := hardeningWindowsFindings(tight); len(got) != 0 {
			t.Fatalf("a data directory the store made: %q", got)
		}
		if got := hardeningWindowsFindings(filepath.Join(root, "absent")); len(got) != 0 {
			t.Fatalf("an absent directory: %q", got)
		}
	})

	t.Run("a real directory", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("unix mode bits")
		}
		root := t.TempDir()
		data := filepath.Join(root, "data")
		if err := os.MkdirAll(data, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(data, 0o755); err != nil {
			t.Fatal(err)
		}
		var buf syncBuf
		hardeningWarnDataDir(hardeningTestLogger(&buf), "linux", data)
		if !strings.Contains(buf.String(), `"level":"WARN"`) || !strings.Contains(buf.String(), "chmod 700") {
			t.Errorf("no warning for a 0755 directory:\n%s", buf.String())
		}
		if err := os.Chmod(data, 0o700); err != nil {
			t.Fatal(err)
		}
		var quiet syncBuf
		hardeningWarnDataDir(hardeningTestLogger(&quiet), "linux", data)
		hardeningWarnDataDir(hardeningTestLogger(&quiet), "linux", filepath.Join(root, "absent"))
		if quiet.String() != "" {
			t.Errorf("a 0700 or absent directory warned:\n%s", quiet.String())
		}
	})

	t.Run("serve itself warns", func(t *testing.T) {
		f := newFixture(t)
		cfg, data := serveConfig(t, f, "")
		if err := os.MkdirAll(data, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(data, 0o755); err != nil {
			t.Fatal(err)
		}
		fspermtest.Loosen(t, data) // the ACL on Windows
		if err := os.WriteFile(filepath.Join(data, store.DefaultFileName), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		old := serveProfileNewEngine
		serveProfileNewEngine = func(dialogue.Deps) (*dialogue.Engine, error) { return nil, serveProfileStopped }
		t.Cleanup(func() { serveProfileNewEngine = old })

		var stderr syncBuf
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := runServe(ctx, serveEnv(t, &syncBuf{}, &stderr), []string{"-config", cfg})
		if !errors.Is(err, serveProfileStopped) {
			t.Fatalf("serve did not reach the engine: %v", err)
		}
		out := stderr.String()
		switch runtime.GOOS {
		case "windows":
			if !strings.Contains(out, "not owner only") {
				t.Errorf("serve started over an inheriting data directory without a warning:\n%s", out)
			}
		default:
			if !strings.Contains(out, "accessible by other users (mode 0755)") {
				t.Errorf("serve started over a 0755 data directory without a warning:\n%s", out)
			}
		}
	})
}

func hardeningTestLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }
