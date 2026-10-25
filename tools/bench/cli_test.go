package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/models"
)

// runCLI drives the command line in process and returns the exit status and
// both output streams.
func runCLI(t testing.TB, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// fakeRunArgs returns the arguments of a quick fake-provider run.
func fakeRunArgs(t testing.TB, out, profile string, extra ...string) []string {
	t.Helper()
	args := []string{"run", "--provider", "fake", "--model", "fake", "--profile", profile,
		"--corpus", repoPath(t, corpusRel), "--out", out, "--limit", "12", "--warmup", "0"}
	return append(args, extra...)
}

// onlyResultFile returns the one result file in dir.
func onlyResultFile(t testing.TB, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("want exactly one result in %s, found %v (%v)", dir, files, err)
	}
	return files[0]
}

func readResultMap(t testing.TB, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeResultMap(t testing.TB, dir, name string, m map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// dig walks a path of map keys and slice indexes in a decoded JSON value.
func dig(t testing.TB, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("not an object at %q", k)
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("not a list with index %d", k)
			}
			v = s[k]
		}
	}
	return v
}

// setPath sets a value at a path of map keys (the last element is the key).
func setPath(t testing.TB, root map[string]any, value any, path ...any) {
	t.Helper()
	parent := dig(t, root, path[:len(path)-1]...)
	switch k := path[len(path)-1].(type) {
	case string:
		parent.(map[string]any)[k] = value
	case int:
		parent.([]any)[k] = value
	}
}

// TestResultFileValidates runs the tool end to end, writes a real result file
// and has `bench check` accept it; then it damages the file in many ways and
// has check refuse each one for the right reason.
func TestResultFileValidates(t *testing.T) {
	dir := t.TempDir()
	if code, out, errs := runCLI(t, fakeRunArgs(t, dir, "lite")...); code != 0 {
		t.Fatalf("run exited %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	path := onlyResultFile(t, dir)

	t.Run("check accepts it", func(t *testing.T) {
		code, out, errs := runCLI(t, "check", dir)
		if code != 0 {
			t.Fatalf("check exited %d\n%s%s", code, out, errs)
		}
		if !strings.Contains(out, "PASS") || !strings.Contains(out, "ok - 1 result file") {
			t.Errorf("output = %q", out)
		}
	})

	t.Run("it validates against schemas/bench/result.schema.json", func(t *testing.T) {
		_, resultSchema, _ := realSchemas(t)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if problems := validateResultBytes(resultSchema, b); len(problems) != 0 {
			t.Fatalf("problems: %v", problems)
		}
	})

	t.Run("a single file can be checked", func(t *testing.T) {
		if code, out, _ := runCLI(t, "check", path); code != 0 {
			t.Errorf("exit %d: %s", code, out)
		}
	})

	t.Run("the file name is safe on every file system", func(t *testing.T) {
		name := filepath.Base(path)
		if strings.ContainsAny(name, `:*?"<>| /\`) || !strings.HasSuffix(name, ".json") {
			t.Errorf("file name %q", name)
		}
		if !strings.HasPrefix(name, "fake_fake_lite_run_") {
			t.Errorf("file name %q does not name provider, model, profile and run", name)
		}
	})

	t.Run("no key of the result can be redacted as a secret", func(t *testing.T) {
		// internal/log masks any key that contains "token" (and "secret", "password", ...).
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, e := range x {
					for _, bad := range []string{"token", "secret", "password", "authorization", "apikey", "cookie", "credential"} {
						if strings.Contains(strings.ToLower(k), bad) && path != "cases.output" {
							t.Errorf("key %q at %s would be masked by the log redactor", k, path)
						}
					}
					walk(path+"."+k, e)
				}
			case []any:
				for _, e := range x {
					walk(path, e)
				}
			}
		}
		walk("", readResultMap(t, path))
	})

	// Each case damages a copy of the file; check must refuse it.
	damage := []struct {
		name   string
		mutate func(t *testing.T, m map[string]any)
		want   string // substring of check's output
	}{
		{"a required key is missing", func(t *testing.T, m map[string]any) { delete(m, "host") }, "host"},
		{"an unknown key", func(t *testing.T, m map[string]any) { m["extra"] = 1 }, "extra"},
		{"an unknown nested key", func(t *testing.T, m map[string]any) { setPath(t, m, 1, "metrics", "surprise") }, "surprise"},
		{"a wrong format version", func(t *testing.T, m map[string]any) { m["format_version"] = 2 }, "format_version"},
		{"a malformed run id", func(t *testing.T, m map[string]any) { m["run_id"] = "run_1" }, "run_id"},
		{"a timestamp that is not a time", func(t *testing.T, m map[string]any) { m["timestamp"] = "yesterday" }, "timestamp"},
		{"a timestamp that is not UTC", func(t *testing.T, m map[string]any) { m["timestamp"] = "2026-10-02T12:00:00+02:00" }, "timestamp"},
		{"a profile that does not exist", func(t *testing.T, m map[string]any) { m["profile"] = "none" }, "profile"},
		{"the short profile name instead of the registry's", func(t *testing.T, m map[string]any) { m["profile"] = "4g" }, "profile"},
		{"a result that claims not to be emulated", func(t *testing.T, m map[string]any) { m["emulated"] = false }, "emulated"},
		{"emulated as a string", func(t *testing.T, m map[string]any) { m["emulated"] = "yes" }, "emulated"},
		{"a fake result that claims to be real", func(t *testing.T, m map[string]any) { m["synthetic"] = false }, "synthetic"},
		{"limits that are not the profile's", func(t *testing.T, m map[string]any) { setPath(t, m, 1.0, "container_limits", "mem_mb") }, "container_limits"},
		{"a corpus hash that is not a hash", func(t *testing.T, m map[string]any) { setPath(t, m, "abc", "corpus", "sha256") }, "sha256"},
		{"an artefact hash with no source", func(t *testing.T, m map[string]any) { setPath(t, m, "a", "model", "artifact_sha256") }, "artifact_sha256"},
		{"a git sha that is not one", func(t *testing.T, m map[string]any) { setPath(t, m, "main", "git", "sha") }, "sha"},
		{"zero cores", func(t *testing.T, m map[string]any) { setPath(t, m, 0, "host", "logical_cores") }, "logical_cores"},
		{"a rate above one", func(t *testing.T, m map[string]any) { setPath(t, m, 1.5, "metrics", "pass", "rate") }, "rate"},
		{"a negative count", func(t *testing.T, m map[string]any) { setPath(t, m, -1, "metrics", "pass", "passed") }, "passed"},
		{"a metric that disagrees with the cases", func(t *testing.T, m map[string]any) { setPath(t, m, 0, "metrics", "pass", "passed") }, "metrics do not match"},
		{"a per-category metric that disagrees", func(t *testing.T, m map[string]any) { setPath(t, m, 1, "by_category", "routing", "cases") }, "by_category"},
		{"a per-language metric that disagrees", func(t *testing.T, m map[string]any) { setPath(t, m, 1, "by_language", "en", "cases") }, "by_language"},
		{"a latency percentile edited by hand", func(t *testing.T, m map[string]any) { setPath(t, m, 1.0, "latency", "total_ms", "p95") }, "latency does not match"},
		{"an error count edited by hand", func(t *testing.T, m map[string]any) { setPath(t, m, 3, "errors", "count") }, "errors do not match"},
		{"a case flag flipped so the pass is no longer earned", func(t *testing.T, m map[string]any) { setPath(t, m, false, "cases", 0, "capability_ok") }, "pass is true but capability_ok is false"},
		{"a case that passes without a valid schema", func(t *testing.T, m map[string]any) {
			setPath(t, m, false, "cases", 0, "schema_valid")
			setPath(t, m, "x", "cases", 0, "schema_error")
		}, "cases"},
		{"a case with a bad category", func(t *testing.T, m map[string]any) { setPath(t, m, "chitchat", "cases", 0, "category") }, "category"},
		{"an error case without a code", func(t *testing.T, m map[string]any) { setPath(t, m, "error", "cases", 0, "status") }, "/cases/0"},
		{"no cases", func(t *testing.T, m map[string]any) { m["cases"] = []any{} }, "cases"},
		{"cases that are not the ones that ran", func(t *testing.T, m map[string]any) { setPath(t, m, 80, "corpus", "cases_run") }, "cases_run"},
		{"a limit that is not the number of cases", func(t *testing.T, m map[string]any) { setPath(t, m, nil, "config", "limit") }, "no --limit was used"},
		{"memory that says it sampled while nothing did", func(t *testing.T, m map[string]any) { setPath(t, m, true, "memory", "sampled") }, "memory"},
		{"memory numbers without sampling", func(t *testing.T, m map[string]any) { setPath(t, m, 100.0, "memory", "peak_rss_mb") }, "memory"},
		{"a percentile method nobody defined", func(t *testing.T, m map[string]any) { setPath(t, m, "interpolated", "config", "percentile_method") }, "percentile_method"},
	}
	for _, tt := range damage {
		t.Run("check refuses: "+tt.name, func(t *testing.T) {
			m := readResultMap(t, path)
			tt.mutate(t, m)
			bad := t.TempDir()
			writeResultMap(t, bad, "damaged.json", m)
			code, out, _ := runCLI(t, "check", bad)
			if code != 1 {
				t.Fatalf("exit %d, want 1; output:\n%s", code, out)
			}
			if !strings.Contains(out, "FAIL") || !strings.Contains(out, tt.want) {
				t.Errorf("output does not mention %q:\n%s", tt.want, out)
			}
		})
	}

	t.Run("check fails closed", func(t *testing.T) {
		empty := t.TempDir()
		if code, _, errs := runCLI(t, "check", empty); code != 1 || !strings.Contains(errs, "no result files") {
			t.Errorf("an empty directory must fail: exit %d, stderr %q", code, errs)
		}
		if code, _, _ := runCLI(t, "check", filepath.Join(empty, "missing")); code != 1 {
			t.Errorf("a missing directory must fail, exit %d", code)
		}
		notAResult := t.TempDir()
		if err := os.WriteFile(filepath.Join(notAResult, "other.json"), []byte(`{"hello":"world"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, out, _ := runCLI(t, "check", notAResult); code != 1 || !strings.Contains(out, "FAIL") {
			t.Errorf("a JSON file that is not a result must fail: exit %d\n%s", code, out)
		}
		garbage := t.TempDir()
		if err := os.WriteFile(filepath.Join(garbage, "x.json"), []byte(`{not json`), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, out, _ := runCLI(t, "check", garbage); code != 1 || !strings.Contains(out, "not valid JSON") {
			t.Errorf("malformed JSON must fail: exit %d\n%s", code, out)
		}
		if code, _, _ := runCLI(t, "check"); code != 2 {
			t.Errorf("no argument is a usage error, exit %d", code)
		}
	})

	t.Run("one bad file fails the whole directory", func(t *testing.T) {
		mixed := t.TempDir()
		good, _ := os.ReadFile(path)
		if err := os.WriteFile(filepath.Join(mixed, "good.json"), good, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mixed, "bad.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, _ := runCLI(t, "check", mixed)
		if code != 1 || !strings.Contains(out, "PASS") || !strings.Contains(out, "FAIL") || !strings.Contains(out, "1 of 2") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})

	t.Run("directories are searched recursively, hidden files are skipped", func(t *testing.T) {
		root := t.TempDir()
		sub := filepath.Join(root, "m0", "lite")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		good, _ := os.ReadFile(path)
		if err := os.WriteFile(filepath.Join(sub, "r.json"), good, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".bench-123.tmp"), []byte("half"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".hidden.json"), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, out, _ := runCLI(t, "check", root); code != 0 || !strings.Contains(out, "ok - 1 result file") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})

	t.Run("-reject-synthetic refuses fake results", func(t *testing.T) {
		code, out, _ := runCLI(t, "check", "-reject-synthetic", dir)
		if code != 1 || !strings.Contains(out, "synthetic") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
}

// TestEmulatedFlagSetForLimitedProfiles: lite, balanced and experimental-4g are
// resource-limited profiles, so every result under them is emulated, never
// certified; --profile 4g is the registry's experimental-4g; nothing else is
// accepted.
func TestEmulatedFlagSetForLimitedProfiles(t *testing.T) {
	tests := []struct {
		flag    string
		profile string
		memMB   int
		cpus    float64
	}{
		{"lite", "lite", 8192, 4},
		{"balanced", "balanced", 16384, 8},
		{"4g", "experimental-4g", 4096, 4},
		{"experimental-4g", "experimental-4g", 4096, 4},
		{"LITE", "lite", 8192, 4},
	}
	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			p, err := ParseProfile(tt.flag)
			if err != nil || string(p) != tt.profile {
				t.Fatalf("ParseProfile(%q) = %q, %v; want %q", tt.flag, p, err, tt.profile)
			}
			if !IsEmulated(p) {
				t.Errorf("profile %s must be emulated", p)
			}
			limits, ok := LimitsFor(p)
			if !ok || limits.MemMB != tt.memMB || limits.CPUs != tt.cpus {
				t.Errorf("limits = %+v, %v; want %d MiB and %v CPUs", limits, ok, tt.memMB, tt.cpus)
			}

			dir := t.TempDir()
			if code, out, errs := runCLI(t, fakeRunArgs(t, dir, tt.flag, "--limit", "3")...); code != 0 {
				t.Fatalf("run exited %d\n%s%s", code, out, errs)
			}
			m := readResultMap(t, onlyResultFile(t, dir))
			if m["profile"] != tt.profile {
				t.Errorf("profile in the result = %v, want %s (the registry's spelling)", m["profile"], tt.profile)
			}
			if m["emulated"] != true {
				t.Errorf("emulated = %v, want true", m["emulated"])
			}
			if dig(t, m, "container_limits", "mem_mb") != float64(tt.memMB) || dig(t, m, "container_limits", "cpus") != tt.cpus {
				t.Errorf("container_limits = %v", m["container_limits"])
			}
			if code, out, _ := runCLI(t, "check", dir); code != 0 {
				t.Errorf("check refused a genuine %s result:\n%s", tt.flag, out)
			}

			// Claiming otherwise is refused, even when the file is otherwise perfect.
			m["emulated"] = false
			lie := t.TempDir()
			writeResultMap(t, lie, "lie.json", m)
			if code, out, _ := runCLI(t, "check", lie); code != 1 || !strings.Contains(out, "emulated") {
				t.Errorf("a result claiming emulated=false under %s must fail check: exit %d\n%s", tt.profile, code, out)
			}
		})
	}

	t.Run("the profile names are the registry's", func(t *testing.T) {
		for _, p := range []models.Profile{models.ProfileLite, models.ProfileBalanced, models.ProfileExperimental4G} {
			if !IsEmulated(p) {
				t.Errorf("%s is a registry profile and must be emulated", p)
			}
		}
	})

	t.Run("a profile the tool does not know cannot be run or claimed", func(t *testing.T) {
		for _, bad := range []string{"", "none", "full", "certified", "8g", "lite2"} {
			code, _, errs := runCLI(t, fakeRunArgs(t, t.TempDir(), bad)...)
			if code != 2 || !strings.Contains(errs, "unknown profile") {
				t.Errorf("--profile %q: exit %d, stderr %q; want a usage error", bad, code, errs)
			}
			if _, err := ParseProfile(bad); err == nil {
				t.Errorf("ParseProfile(%q) accepted it", bad)
			}
		}
		if IsEmulated("certified") || IsEmulated("") {
			t.Error("an unknown profile is not a limited profile, and must not be treated as one")
		}
	})
}

func TestRunUsageErrors(t *testing.T) {
	corpus := repoPath(t, corpusRel)
	good := []string{"--provider", "fake", "--model", "fake", "--profile", "lite", "--corpus", corpus, "--out", t.TempDir()}
	without := func(flag string) []string {
		var out []string
		for i := 0; i < len(good); i++ {
			if good[i] == flag {
				i++
				continue
			}
			out = append(out, good[i])
		}
		return append([]string{"run"}, out...)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "usage"},
		{"unknown command", []string{"frobnicate"}, "unknown command"},
		{"missing provider", without("--provider"), "--provider"},
		{"missing model", without("--model"), "--model"},
		{"missing profile", without("--profile"), "profile"},
		{"missing corpus", without("--corpus"), "--corpus"},
		{"missing out", without("--out"), "--out"},
		{"bad provider", append([]string{"run"}, replaceFlag(good, "--provider", "openai")...), "--provider"},
		{"bad role", append(append([]string{"run"}, good...), "--role", "embedder"), "--role"},
		{"bad constraint", append(append([]string{"run"}, good...), "--constraint", "magic"), "--constraint"},
		{"negative limit", append(append([]string{"run"}, good...), "--limit", "-1"), "--limit"},
		{"zero max toks", append(append([]string{"run"}, good...), "--max-toks", "0"), "--max-toks"},
		{"stray argument", append(append([]string{"run"}, good...), "extra"), "unexpected argument"},
		{"unknown flag", append(append([]string{"run"}, good...), "--turbo"), "flag provided but not defined"},
		{"too fast sampling", append(append([]string{"run"}, good...), "--docker-container", "x", "--sample-interval", "1ms"), "--sample-interval"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errs := runCLI(t, tt.args...)
			if code != 2 || !strings.Contains(out+errs, tt.want) {
				t.Errorf("exit %d, want 2 and a message with %q\nstdout: %s\nstderr: %s", code, tt.want, out, errs)
			}
		})
	}
	t.Run("help succeeds", func(t *testing.T) {
		if code, out, _ := runCLI(t, "help"); code != 0 || !strings.Contains(out, "bench run") {
			t.Errorf("exit %d, %q", code, out)
		}
		if code, _, _ := runCLI(t, "run", "-h"); code != 0 {
			t.Errorf("run -h exit %d", code)
		}
	})
}

func replaceFlag(args []string, flag, value string) []string {
	out := append([]string(nil), args...)
	for i := range out {
		if out[i] == flag {
			out[i+1] = value
		}
	}
	return out
}

func TestRunFailures(t *testing.T) {
	t.Run("an unknown registry id with the fake provider", func(t *testing.T) {
		args := fakeRunArgs(t, t.TempDir(), "lite")
		args = replaceFlag(args, "--model", "no-such-model")
		code, _, errs := runCLI(t, args...)
		if code != 1 || !strings.Contains(errs, "not in the registry") {
			t.Errorf("exit %d, stderr %q", code, errs)
		}
		for _, e := range registryEntries(t) {
			if !strings.Contains(errs, e.ID) {
				t.Errorf("the error does not list the known model %s: %q", e.ID, errs)
			}
		}
	})
	t.Run("a corpus that does not load", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad-v0.jsonl")
		if err := os.WriteFile(bad, []byte("{\"id\": 1}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		args := replaceFlag(fakeRunArgs(t, t.TempDir(), "lite"), "--corpus", bad)
		if code, _, errs := runCLI(t, args...); code != 1 || !strings.Contains(errs, "line 1") {
			t.Errorf("exit %d, stderr %q", code, errs)
		}
	})
	t.Run("a missing corpus", func(t *testing.T) {
		args := replaceFlag(fakeRunArgs(t, t.TempDir(), "lite"), "--corpus", filepath.Join(t.TempDir(), "nope.jsonl"))
		if code, _, _ := runCLI(t, args...); code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
	})
	t.Run("nothing is written when the run fails", func(t *testing.T) {
		dir := t.TempDir()
		args := replaceFlag(fakeRunArgs(t, dir, "lite"), "--model", "no-such-model")
		runCLI(t, args...)
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("a failed run left %d file(s) behind", len(entries))
		}
	})
	t.Run("a result is never overwritten", func(t *testing.T) {
		dir := t.TempDir()
		res := &Result{RunID: "run_01J0000000000000000000000A", Profile: "lite", Model: ModelInfo{RegistryID: "m"}, Provider: ProviderInfo{Name: "fake"}}
		first, err := writeResultFile(dir, res, []byte("one"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writeResultFile(dir, res, []byte("two")); err == nil {
			t.Error("the second write over the same run id must fail")
		}
		if b, _ := os.ReadFile(first); string(b) != "one" {
			t.Errorf("the first result was changed to %q", b)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Errorf("%d files in the directory, want 1 (no temporary files left)", len(entries))
		}
	})
	t.Run("the output directory is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "b")
		if code, out, errs := runCLI(t, fakeRunArgs(t, dir, "lite")...); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, out, errs)
		}
		onlyResultFile(t, dir)
	})
}
