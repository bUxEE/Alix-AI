package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// testSection is a module-owned section exercising every override kind.
type testSection struct {
	ChunkSize int           `yaml:"chunk_size"`
	Enabled   bool          `yaml:"enabled"`
	Timeout   time.Duration `yaml:"timeout"`
	Ratio     float64       `yaml:"ratio"`
	Tags      []string      `yaml:"tags"`
	Store     struct {
		Path string `yaml:"path"`
	} `yaml:"store"`
	Skipped string `yaml:"-"`
}

func (s testSection) Validate() error {
	if s.ChunkSize <= 0 {
		return fmt.Errorf("chunk_size must be positive, got %d", s.ChunkSize)
	}
	return nil
}

func newTestSection() *testSection {
	return &testSection{ChunkSize: 800, Timeout: 5 * time.Second, Tags: []string{"a"}}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "alixai.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func noEnv(string) (string, bool) { return "", false }

func envMap(m map[string]string) EnvLookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestConfigLoadDefaults(t *testing.T) {
	want := CoreConfig{
		DataDir: "data",
		Server:  ServerConfig{Listen: "127.0.0.1:7470", UnixSocket: ""},
		Log:     LogConfig{Level: "info"},
	}

	t.Run("no file", func(t *testing.T) {
		r := NewRegistry()
		if err := r.LoadFrom("", noEnv); err != nil {
			t.Fatalf("LoadFrom(\"\") = %v", err)
		}
		if diff := cmp.Diff(want, r.Core()); diff != "" {
			t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("registry before any load already holds defaults", func(t *testing.T) {
		if diff := cmp.Diff(want, NewRegistry().Core()); diff != "" {
			t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("empty and comment-only files", func(t *testing.T) {
		for _, content := range []string{"", "# nothing\n", "---\n", "null\n"} {
			r := NewRegistry()
			if err := r.LoadFrom(writeFile(t, content), noEnv); err != nil {
				t.Fatalf("LoadFrom(%q) = %v", content, err)
			}
			if diff := cmp.Diff(want, r.Core()); diff != "" {
				t.Fatalf("content %q: defaults mismatch (-want +got):\n%s", content, diff)
			}
		}
	})

	t.Run("partial file keeps other defaults", func(t *testing.T) {
		r := NewRegistry()
		p := writeFile(t, "server:\n  unix_socket: /run/alixai.sock\nlog:\n  level: debug\n")
		if err := r.LoadFrom(p, noEnv); err != nil {
			t.Fatal(err)
		}
		got := want
		got.Server.UnixSocket = "/run/alixai.sock"
		got.Log.Level = "debug"
		if diff := cmp.Diff(got, r.Core()); diff != "" {
			t.Fatalf("mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a second load resets to defaults instead of accumulating", func(t *testing.T) {
		r := NewRegistry()
		if err := r.LoadFrom(writeFile(t, "data_dir: /var/lib/alixai\n"), noEnv); err != nil {
			t.Fatal(err)
		}
		if err := r.LoadFrom("", noEnv); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, r.Core()); diff != "" {
			t.Fatalf("mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("explicit missing file is an error", func(t *testing.T) {
		r := NewRegistry()
		if err := r.LoadFrom(filepath.Join(t.TempDir(), "absent.yaml"), noEnv); err == nil {
			t.Fatal("LoadFrom(missing) = nil, want error")
		}
	})
}

func TestConfigEnvOverride(t *testing.T) {
	r := NewRegistry()
	sec := newTestSection()
	r.Register("rag", sec)

	p := writeFile(t, `
server:
  listen: 127.0.0.1:9000
log:
  level: warn
rag:
  chunk_size: 100
  enabled: false
  store:
    path: /from/file
`)
	env := envMap(map[string]string{
		"ALIXAI_SERVER_LISTEN":      "127.0.0.1:9100",
		"ALIXAI_SERVER_UNIX_SOCKET": "/run/x.sock",
		"ALIXAI_DATA_DIR":           "/srv/alixai",
		"ALIXAI_LOG_LEVEL":          "DEBUG",
		"ALIXAI_LOG_REDACT_KEYS":    "ssn, card_number",
		"ALIXAI_RAG_CHUNK_SIZE":     "256",
		"ALIXAI_RAG_ENABLED":        "true",
		"ALIXAI_RAG_TIMEOUT":        "1m30s",
		"ALIXAI_RAG_RATIO":          "0.25",
		"ALIXAI_RAG_TAGS":           "x,y , z",
		"ALIXAI_RAG_STORE_PATH":     "/from/env",
		"ALIXAI_RAG_SKIPPED":        "must be ignored (yaml:\"-\")",
		"ALIXAI_UNRELATED_THING":    "ignored, not a registered field",
	})
	if err := r.LoadFrom(p, env); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	wantCore := CoreConfig{
		DataDir: "/srv/alixai",
		Server:  ServerConfig{Listen: "127.0.0.1:9100", UnixSocket: "/run/x.sock"},
		Log:     LogConfig{Level: "DEBUG", RedactKeys: []string{"ssn", "card_number"}},
	}
	if diff := cmp.Diff(wantCore, r.Core()); diff != "" {
		t.Errorf("core mismatch (env must beat file) (-want +got):\n%s", diff)
	}

	want := testSection{ChunkSize: 256, Enabled: true, Timeout: 90 * time.Second, Ratio: 0.25, Tags: []string{"x", "y", "z"}}
	want.Store.Path = "/from/env"
	if diff := cmp.Diff(want, *sec); diff != "" {
		t.Errorf("module section mismatch (-want +got):\n%s", diff)
	}

	t.Run("file value survives when env is absent", func(t *testing.T) {
		if err := r.LoadFrom(p, noEnv); err != nil {
			t.Fatal(err)
		}
		if r.Core().Server.Listen != "127.0.0.1:9000" || sec.ChunkSize != 100 || sec.Store.Path != "/from/file" {
			t.Fatalf("file values lost: listen=%q chunk=%d path=%q", r.Core().Server.Listen, sec.ChunkSize, sec.Store.Path)
		}
	})

	t.Run("real process environment", func(t *testing.T) {
		t.Setenv("ALIXAI_SERVER_LISTEN", "127.0.0.1:9333")
		if err := r.Load(""); err != nil {
			t.Fatal(err)
		}
		if got := r.Core().Server.Listen; got != "127.0.0.1:9333" {
			t.Fatalf("Listen = %q, want the value from the process environment", got)
		}
	})

	t.Run("env value is validated like any other", func(t *testing.T) {
		err := r.LoadFrom("", envMap(map[string]string{"ALIXAI_SERVER_LISTEN": "not-an-address"}))
		if err == nil || !strings.Contains(err.Error(), "server") {
			t.Fatalf("err = %v, want a server validation error", err)
		}
	})

	t.Run("unparseable env value is an error that does not echo the value", func(t *testing.T) {
		secret := "s3cr3t-not-a-number"
		err := r.LoadFrom("", envMap(map[string]string{"ALIXAI_RAG_CHUNK_SIZE": secret}))
		if err == nil {
			t.Fatal("LoadFrom = nil, want error")
		}
		if !strings.Contains(err.Error(), "ALIXAI_RAG_CHUNK_SIZE") {
			t.Errorf("error %q does not name the variable", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q leaks the env value", err)
		}
	})
}

func TestConfigRejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown top-level section", "bogus:\n  a: 1\n", `unknown section "bogus"`},
		{"unknown nested key in core section", "server:\n  listn: 127.0.0.1:1\n", "listn"},
		{"unknown nested key in module section", "rag:\n  chunk_sizes: 3\n", "chunk_sizes"},
		{"unknown deeply nested key", "rag:\n  store:\n    pth: x\n", "pth"},
		{"unknown scalar-adjacent key", "data_dir: x\ndata-dir: y\n", `unknown section "data-dir"`},
		{"duplicate section", "log:\n  level: info\nlog:\n  level: warn\n", "already defined"},
		{"top level is a list", "- server\n- log\n", "mapping"},
		{"top level is a scalar", "just-a-string\n", "mapping"},
		{"merge key is not a section", "<<: {a: 1}\n", "unknown section"},
		{"wrong type", "server:\n  listen: [1, 2]\n", "cannot unmarshal"},
		{"two documents", "log:\n  level: info\n---\nlog:\n  level: warn\n", "multiple"},
		{"not yaml", "server: [unclosed\n", "line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			r.Register("rag", newTestSection())
			before := r.Core()
			err := r.LoadFrom(writeFile(t, tc.yaml), noEnv)
			if err == nil {
				t.Fatalf("LoadFrom accepted:\n%s", tc.yaml)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if diff := cmp.Diff(before, r.Core()); diff != "" {
				t.Errorf("a failed Load changed the registered sections (-before +after):\n%s", diff)
			}
		})
	}

	t.Run("the offending line is reported", func(t *testing.T) {
		r := NewRegistry()
		err := r.LoadFrom(writeFile(t, "log:\n  level: info\nbogus: 1\n"), noEnv)
		if err == nil || !strings.Contains(err.Error(), "line 3") {
			t.Fatalf("err = %v, want it to mention line 3", err)
		}
	})
}

func TestConfigModuleSectionRegistration(t *testing.T) {
	t.Run("defaults come from the registered value and the file fills the same pointer", func(t *testing.T) {
		r := NewRegistry()
		sec := newTestSection()
		r.Register("rag", sec)

		if err := r.LoadFrom("", noEnv); err != nil {
			t.Fatal(err)
		}
		if sec.ChunkSize != 800 || sec.Timeout != 5*time.Second {
			t.Fatalf("defaults not applied: %+v", *sec)
		}

		p := writeFile(t, "rag:\n  chunk_size: 42\n  tags: [p, q]\n  timeout: 2s\n")
		if err := r.LoadFrom(p, noEnv); err != nil {
			t.Fatal(err)
		}
		want := testSection{ChunkSize: 42, Timeout: 2 * time.Second, Tags: []string{"p", "q"}}
		if diff := cmp.Diff(want, *sec); diff != "" {
			t.Fatalf("section mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"data_dir", "log", "rag", "server"}, r.Sections()); diff != "" {
			t.Fatalf("Sections mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("Validate runs on module sections and names the section", func(t *testing.T) {
		r := NewRegistry()
		sec := newTestSection()
		r.Register("rag", sec)
		err := r.LoadFrom(writeFile(t, "rag:\n  chunk_size: -1\n"), noEnv)
		if err == nil || !strings.Contains(err.Error(), "section rag") || !strings.Contains(err.Error(), "chunk_size") {
			t.Fatalf("err = %v, want a validation error naming section rag", err)
		}
		if sec.ChunkSize != 800 {
			t.Fatalf("failed Load published an invalid section: %+v", *sec)
		}
	})

	t.Run("every failing section is reported", func(t *testing.T) {
		r := NewRegistry()
		r.Register("rag", newTestSection())
		err := r.LoadFrom(writeFile(t, "rag:\n  chunk_size: 0\nserver:\n  listen: nope\nlog:\n  level: loud\n"), noEnv)
		if err == nil {
			t.Fatal("want error")
		}
		for _, s := range []string{"section rag", "section server", "section log"} {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("error %q does not mention %q", err, s)
			}
		}
	})

	t.Run("maps and pointers are never shared between defaults, working copies and live sections", func(t *testing.T) {
		wantDefaults := *newRefsSection()

		// 1. A load that decodes into maps and pointers but then fails
		// validation must leave the live section, the references its owner
		// holds, and the defaults untouched.
		r := NewRegistry()
		live := newRefsSection()
		ownerLabels, ownerSub := live.Labels, live.Sub
		r.Register("refs", live)
		r.Register("gate", &gateSection{})
		rejected := writeFile(t, "refs:\n  labels:\n    b: '2'\n  sub:\n    x: 9\ngate:\n  reject: true\n")
		if err := r.LoadFrom(rejected, noEnv); err == nil {
			t.Fatal("LoadFrom accepted a file the gate rejects")
		}
		if diff := cmp.Diff(wantDefaults, *live); diff != "" {
			t.Errorf("failed Load changed the live section (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(map[string]string{"a": "1"}, ownerLabels); diff != "" {
			t.Errorf("failed Load wrote through the owner's map (-want +got):\n%s", diff)
		}
		if ownerSub.X != 1 {
			t.Errorf("failed Load wrote through the owner's pointer: X = %d", ownerSub.X)
		}

		// 2. A successful load, then a load with no file: back to the defaults.
		accepted := writeFile(t, "refs:\n  labels:\n    b: '2'\n  sub:\n    x: 9\n  names: [m]\n")
		if err := r.LoadFrom(accepted, envMap(map[string]string{"ALIXAI_REFS_SUB_X": "10"})); err != nil {
			t.Fatal(err)
		}
		if live.Sub.X != 10 || live.Labels["b"] != "2" || live.Labels["a"] != "1" || live.Names[0] != "m" {
			t.Fatalf("file and env not applied: %+v sub=%+v", *live, *live.Sub)
		}
		if err := r.LoadFrom("", noEnv); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(wantDefaults, *live); diff != "" {
			t.Errorf("defaults were contaminated by an earlier load (-want +got):\n%s", diff)
		}
	})

	t.Run("a scalar section can be registered", func(t *testing.T) {
		r := NewRegistry()
		n := new(int)
		*n = 7
		r.Register("workers", n)
		if err := r.LoadFrom("", envMap(map[string]string{"ALIXAI_WORKERS": "12"})); err != nil {
			t.Fatal(err)
		}
		if *n != 12 {
			t.Fatalf("workers = %d, want 12 from ALIXAI_WORKERS", *n)
		}
		if err := r.LoadFrom(writeFile(t, "workers: 3\n"), noEnv); err != nil {
			t.Fatal(err)
		}
		if *n != 3 {
			t.Fatalf("workers = %d, want 3 from the file", *n)
		}
	})

	t.Run("package-level Register and Load use the Default registry", func(t *testing.T) {
		sec := newTestSection()
		Register("default_registry_probe", sec)
		t.Cleanup(func() { Default.mu.Lock(); defer Default.mu.Unlock(); removeForTest(Default, "default_registry_probe") })
		t.Setenv("ALIXAI_DEFAULT_REGISTRY_PROBE_CHUNK_SIZE", "9")
		if err := Load(""); err != nil {
			t.Fatal(err)
		}
		if sec.ChunkSize != 9 {
			t.Fatalf("ChunkSize = %d, want 9", sec.ChunkSize)
		}
		if got := Core().Server.Listen; got != DefaultListen {
			t.Fatalf("Core().Server.Listen = %q, want %q", got, DefaultListen)
		}
	})

	t.Run("programming errors panic", func(t *testing.T) {
		r := NewRegistry()
		r.Register("ok", newTestSection())
		type claimsEnv struct {
			Dir string `yaml:"dir"`
		}
		for name, fn := range map[string]func(){
			"duplicate name":       func() { r.Register("ok", newTestSection()) },
			"core name reused":     func() { r.Register("server", newTestSection()) },
			"empty name":           func() { r.Register("", newTestSection()) },
			"uppercase name":       func() { r.Register("Rag", newTestSection()) },
			"name with dash":       func() { r.Register("my-mod", newTestSection()) },
			"not a pointer":        func() { r.Register("val", testSection{}) },
			"nil pointer":          func() { r.Register("nilp", (*testSection)(nil)) },
			"nil interface":        func() { r.Register("nili", nil) },
			"unsupported map type": func() { r.Register("m", &map[string]string{}) },
			"env name collision":   func() { r.Register("data", &claimsEnv{}) }, // ALIXAI_DATA_DIR is data_dir's
		} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%s: Register did not panic", name)
					}
				}()
				fn()
			}()
		}
	})
}

// removeForTest unregisters a section from the shared Default registry so the
// test leaves no trace. The caller holds r.mu.
func removeForTest(r *Registry, name string) {
	delete(r.byName, name)
	kept := r.entries[:0]
	for _, e := range r.entries {
		if e.name != name {
			kept = append(kept, e)
		}
	}
	r.entries = kept
	for env, owner := range r.envOwner {
		if owner == name {
			delete(r.envOwner, env)
		}
	}
}

func TestCoreValidation(t *testing.T) {
	bad := map[string]string{
		"listen without port":       "server:\n  listen: 127.0.0.1\n",
		"listen port not numeric":   "server:\n  listen: 127.0.0.1:http\n",
		"listen port out of range":  "server:\n  listen: 127.0.0.1:70000\n",
		"listen empty":              "server:\n  listen: \"\"\n",
		"listen bad host":           "server:\n  listen: 'bad host:80'\n",
		"blank unix socket":         "server:\n  unix_socket: '   '\n",
		"empty data_dir":            "data_dir: ''\n",
		"unknown log level":         "log:\n  level: verbose\n",
		"blank redact key":          "log:\n  redact_keys: ['ssn', ' ']\n",
		"nested redact key is junk": "log:\n  redact_keys: x\n",
	}
	for name, content := range bad {
		t.Run("rejects "+name, func(t *testing.T) {
			if err := NewRegistry().LoadFrom(writeFile(t, content), noEnv); err == nil {
				t.Fatalf("accepted:\n%s", content)
			}
		})
	}
	good := map[string]string{
		"ipv4":       "server:\n  listen: 127.0.0.1:0\n",
		"ipv6":       "server:\n  listen: '[::1]:7470'\n",
		"hostname":   "server:\n  listen: localhost:7470\n",
		"any iface":  "server:\n  listen: ':7470'\n",
		"mixed case": "log:\n  level: WARN\n",
		"relative":   "data_dir: ./state/alixai\n",
	}
	for name, content := range good {
		t.Run("accepts "+name, func(t *testing.T) {
			if err := NewRegistry().LoadFrom(writeFile(t, content), noEnv); err != nil {
				t.Fatalf("rejected %q: %v", content, err)
			}
		})
	}
}

// refsSection has map, pointer and slice fields: the kinds a shallow copy of
// the defaults would share with the live section.
type refsSection struct {
	Labels map[string]string `yaml:"labels"`
	Sub    *refsSub          `yaml:"sub"`
	Names  []string          `yaml:"names"`
}

type refsSub struct {
	X int `yaml:"x"`
}

func newRefsSection() *refsSection {
	return &refsSection{Labels: map[string]string{"a": "1"}, Sub: &refsSub{X: 1}, Names: []string{"n"}}
}

// gateSection fails validation on demand, after every section has decoded.
type gateSection struct {
	Reject bool `yaml:"reject"`
}

func (g gateSection) Validate() error {
	if g.Reject {
		return fmt.Errorf("rejected")
	}
	return nil
}
