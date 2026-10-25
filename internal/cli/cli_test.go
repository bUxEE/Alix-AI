package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/version"
)

// result captures one invocation.
type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, root *Root, env map[string]string, args ...string) (result, *config.Registry) {
	t.Helper()
	var out, errb bytes.Buffer
	reg := config.NewRegistry()
	code := root.Run(context.Background(), args, &Env{
		Stdin:     strings.NewReader(""),
		Stdout:    &out,
		Stderr:    &errb,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Config:    reg,
	})
	return result{code, out.String(), errb.String()}, reg
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "alixai.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCLIVersion(t *testing.T) {
	oldV, oldC, oldD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = oldV, oldC, oldD })
	version.Version, version.Commit, version.Date = "9.8.7-test", "0123456789abcdef", "2026-01-02T03:04:05Z"

	res, _ := run(t, Default, nil, "version")
	if res.code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", res.code, res.stderr)
	}
	if want := version.String() + "\n"; res.stdout != want {
		t.Fatalf("stdout = %q, want %q", res.stdout, want)
	}
	for _, want := range []string{"alixai", "9.8.7-test", "0123456789ab", "2026-01-02T03:04:05Z"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout %q does not contain %q", res.stdout, want)
		}
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q, want empty", res.stderr)
	}

	t.Run("extra argument is a usage error", func(t *testing.T) {
		res, _ := run(t, Default, nil, "version", "now")
		if res.code != ExitUsage || res.stdout != "" || !strings.Contains(res.stderr, "unexpected argument") {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("unknown flag is a usage error", func(t *testing.T) {
		res, _ := run(t, Default, nil, "version", "-nope")
		if res.code != ExitUsage || !strings.Contains(res.stderr, "nope") {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("-h prints usage and succeeds", func(t *testing.T) {
		res, _ := run(t, Default, nil, "version", "-h")
		if res.code != ExitOK || !strings.Contains(res.stdout, "Usage: alixai version") {
			t.Fatalf("got %+v", res)
		}
	})
}

func TestCLIConfigCheckRejectsInvalid(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	tests := []struct {
		name     string
		args     func(t *testing.T) []string
		env      map[string]string
		wantCode int
		wantErr  string
	}{
		{
			name:     "unknown nested key",
			args:     fileArgs("server:\n  listn: 127.0.0.1:7470\n"),
			wantCode: ExitError, wantErr: "listn",
		},
		{
			name:     "unknown top-level section",
			args:     fileArgs("bogus: 1\n"),
			wantCode: ExitError, wantErr: `unknown section "bogus"`,
		},
		{
			name:     "invalid listen address",
			args:     fileArgs("server:\n  listen: nowhere\n"),
			wantCode: ExitError, wantErr: "section server",
		},
		{
			name:     "invalid log level",
			args:     fileArgs("log:\n  level: loud\n"),
			wantCode: ExitError, wantErr: "section log",
		},
		{
			name:     "malformed YAML",
			args:     fileArgs("server: [unclosed\n"),
			wantCode: ExitError, wantErr: "config:",
		},
		{
			name:     "missing file",
			args:     func(*testing.T) []string { return []string{"config", "check", "-config", missing} },
			wantCode: ExitError, wantErr: "absent.yaml",
		},
		{
			name:     "invalid environment override",
			args:     func(*testing.T) []string { return []string{"config", "check"} },
			env:      map[string]string{"ALIXAI_SERVER_LISTEN": "nowhere"},
			wantCode: ExitError, wantErr: "section server",
		},
		{
			name: "invalid file named by ALIXAI_CONFIG",
			args: func(*testing.T) []string { return []string{"config", "check"} },
			env: map[string]string{
				"ALIXAI_CONFIG": writeConfig(t, "log:\n  level: loud\n"),
			},
			wantCode: ExitError, wantErr: "section log",
		},
		{
			name:     "positional argument",
			args:     func(*testing.T) []string { return []string{"config", "check", "extra"} },
			wantCode: ExitUsage, wantErr: "unexpected argument",
		},
		{
			name:     "unknown flag",
			args:     func(*testing.T) []string { return []string{"config", "check", "-frobnicate"} },
			wantCode: ExitUsage, wantErr: "frobnicate",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, reg := run(t, Default, tc.env, tc.args(t)...)
			if res.code != tc.wantCode {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", res.code, tc.wantCode, res.stdout, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want empty on failure", res.stdout)
			}
			if !strings.Contains(res.stderr, tc.wantErr) {
				t.Errorf("stderr %q does not contain %q", res.stderr, tc.wantErr)
			}
			if diff := cmp.Diff(config.NewRegistry().Core(), reg.Core()); diff != "" {
				t.Errorf("a rejected config altered the registry (-defaults +got):\n%s", diff)
			}
		})
	}

	t.Run("a valid file passes and prints no values", func(t *testing.T) {
		p := writeConfig(t, "server:\n  listen: 127.0.0.1:9000\ndata_dir: /srv/alixai-private-location\n")
		res, reg := run(t, Default, nil, "config", "check", "-config", p)
		if res.code != ExitOK {
			t.Fatalf("exit = %d, want 0\nstderr: %s", res.code, res.stderr)
		}
		if !strings.Contains(res.stdout, "config ok") {
			t.Errorf("stdout %q does not say ok", res.stdout)
		}
		for _, leak := range []string{"127.0.0.1:9000", "alixai-private-location"} {
			if strings.Contains(res.stdout, leak) {
				t.Errorf("stdout %q echoes configuration value %q", res.stdout, leak)
			}
		}
		if got := reg.Core().Server.Listen; got != "127.0.0.1:9000" {
			t.Errorf("valid file was not loaded: listen = %q", got)
		}
	})

	t.Run("defaults alone are valid", func(t *testing.T) {
		res, _ := run(t, Default, nil, "config", "check")
		if res.code != ExitOK {
			t.Fatalf("exit = %d, want 0\nstderr: %s", res.code, res.stderr)
		}
	})

	t.Run("flag beats ALIXAI_CONFIG", func(t *testing.T) {
		bad := writeConfig(t, "log:\n  level: loud\n")
		good := writeConfig(t, "log:\n  level: debug\n")
		res, _ := run(t, Default, map[string]string{"ALIXAI_CONFIG": bad}, "config", "check", "-config", good)
		if res.code != ExitOK {
			t.Fatalf("exit = %d, want 0\nstderr: %s", res.code, res.stderr)
		}
	})
}

func fileArgs(content string) func(t *testing.T) []string {
	return func(t *testing.T) []string {
		return []string{"config", "check", "-config", writeConfig(t, content)}
	}
}

func TestDefaultRootHasSelfRegisteredCommands(t *testing.T) {
	got := Default.Names()
	for _, want := range []string{"version", "config check"} {
		found := false
		for _, n := range got {
			found = found || n == want
		}
		if !found {
			t.Errorf("command %q is not registered (have %v)", want, got)
		}
	}
}

func TestRootDispatch(t *testing.T) {
	var calls []string
	root := NewRoot("prog")
	for _, name := range []string{"alpha", "group one", "group two", "group"} {
		root.Register(Command{
			Name:    name,
			Summary: "does " + name,
			Run: func(_ context.Context, _ *Env, args []string) error {
				calls = append(calls, name+"|"+strings.Join(args, ","))
				return nil
			},
		})
	}
	root.Register(Command{Name: "fails", Summary: "fails", Run: func(context.Context, *Env, []string) error {
		return errors.New("boom")
	}})
	root.Register(Command{Name: "misuse", Summary: "misuse", Run: func(context.Context, *Env, []string) error {
		return Usagef("bad %s", "input")
	}})

	t.Run("longest name wins and the rest are arguments", func(t *testing.T) {
		calls = nil
		for _, args := range [][]string{{"alpha", "x", "y"}, {"group", "one", "z"}, {"group", "other"}, {"group"}} {
			if res, _ := run(t, root, nil, args...); res.code != ExitOK {
				t.Fatalf("%v: exit %d, stderr %s", args, res.code, res.stderr)
			}
		}
		want := []string{"alpha|x,y", "group one|z", "group|other", "group|"}
		if diff := cmp.Diff(want, calls); diff != "" {
			t.Fatalf("calls (-want +got):\n%s", diff)
		}
	})
	t.Run("a failing command exits 1 with the message", func(t *testing.T) {
		res, _ := run(t, root, nil, "fails")
		if res.code != ExitError || res.stderr != "prog: boom\n" {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("a usage error exits 2", func(t *testing.T) {
		res, _ := run(t, root, nil, "misuse")
		if res.code != ExitUsage || res.stderr != "prog: misuse: bad input\n" {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("unknown command exits 2 and lists commands", func(t *testing.T) {
		res, _ := run(t, root, nil, "nonesuch")
		if res.code != ExitUsage || !strings.Contains(res.stderr, `unknown command "nonesuch"`) || !strings.Contains(res.stderr, "alpha") {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("no arguments prints usage to stderr and exits 2", func(t *testing.T) {
		res, _ := run(t, root, nil)
		if res.code != ExitUsage || res.stdout != "" || !strings.Contains(res.stderr, "Usage: prog") {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("help succeeds and lists commands on stdout", func(t *testing.T) {
		for _, arg := range []string{"help", "-h", "--help"} {
			res, _ := run(t, root, nil, arg)
			if res.code != ExitOK || !strings.Contains(res.stdout, "group one") || !strings.Contains(res.stdout, "does alpha") {
				t.Fatalf("%s: got %+v", arg, res)
			}
		}
	})
}

func TestRootDispatchGroupWithoutCommand(t *testing.T) {
	root := NewRoot("prog")
	root.Register(Command{Name: "db up", Summary: "migrate up", Run: func(context.Context, *Env, []string) error { return nil }})
	root.Register(Command{Name: "db down", Summary: "migrate down", Run: func(context.Context, *Env, []string) error { return nil }})
	res, _ := run(t, root, nil, "db")
	if res.code != ExitUsage || !strings.Contains(res.stderr, "db up") || !strings.Contains(res.stderr, "db down") {
		t.Fatalf("got %+v", res)
	}
}

func TestRegisterPanicsOnProgrammingErrors(t *testing.T) {
	ok := func(context.Context, *Env, []string) error { return nil }
	for name, c := range map[string]Command{
		"empty name":      {Name: "", Run: ok},
		"upper case":      {Name: "Version", Run: ok},
		"double space":    {Name: "config  check", Run: ok},
		"trailing space":  {Name: "config ", Run: ok},
		"nil run":         {Name: "nilrun"},
		"duplicate":       {Name: "dup", Run: ok},
		"leading hyphen":  {Name: "-x", Run: ok},
		"punctuation":     {Name: "a/b", Run: ok},
		"leading digit":   {Name: "1st", Run: ok},
		"underscore name": {Name: "a_b", Run: ok},
	} {
		root := NewRoot("prog")
		root.Register(Command{Name: "dup", Run: ok})
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: Register(%+v) did not panic", name, c.Name)
				}
			}()
			root.Register(c)
		}()
	}
}
