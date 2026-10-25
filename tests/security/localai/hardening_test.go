package localaisec

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/tests/security/secutil"
)

const checklistPath = "docs/security/localai-hardening-checklist.md"

// shippedAnchors are the launchers the repository ships today. Discovery walks the whole checkout and does not take
// this list as its input; the list is the positive control that the walk and the parsers do reach every form.
var shippedAnchors = []string{
	"deploy/container/localai/compose.yaml",
	"deploy/container/localai/.env.example",
	"deploy/container/stack/compose.yaml",
	"deploy/container/quadlet/alixai-localai.container",
	"deploy/container/quadlet/localai.env.example",
	"deploy/native/systemd/localai.service",
	"deploy/native/systemd/localai.yaml",
}

// scanned fails a discovery that found nothing, or that could not read what it found: a scan that sees nothing must
// never pass.
func scanned(d discovery) error {
	if len(d.Launchers) == 0 {
		return fmt.Errorf("no file that launches LocalAI was found")
	}
	if len(d.Problems) > 0 {
		return fmt.Errorf("%d problem(s): %s", len(d.Problems), strings.Join(d.Problems, "; "))
	}
	return nil
}

// SEC-068
func TestSecLocalAIHardeningFlags(t *testing.T) {
	doc := string(secutil.ReadFile(t, checklistPath))

	t.Run("exceptions_table_is_complete", func(t *testing.T) {
		ex, problems := parseExceptions(doc)
		for _, p := range problems {
			t.Error(p)
		}
		for id, row := range map[string]string{"EX-01": "HC-14", "EX-02": "HC-18", "EX-03": "HC-20", "EX-04": "HC-21"} {
			e, ok := ex[row]
			if !ok || e.ID != id {
				t.Errorf("the checklist's exceptions table has no %s entry for %s: the static check reads that table", id, row)
			}
		}
		if len(ex) != 4 {
			t.Errorf("the exceptions table holds %d entries, want exactly the four recorded in M0-18: a new exception is a decision to review here", len(ex))
		}
	})

	t.Run("shipped_files", func(t *testing.T) {
		ex, problems := parseExceptions(doc)
		for _, p := range problems {
			t.Error(p)
		}
		tr := repoTree(t)
		d := discover(tr)
		if err := scanned(d); err != nil {
			t.Fatalf("discovery: %v", err)
		}
		files := map[string]bool{}
		kinds := map[string]int{}
		for _, l := range d.Launchers {
			kinds[l.Kind]++
			for _, f := range l.Files {
				files[f] = true
			}
		}
		for _, a := range shippedAnchors {
			if !files[a] {
				t.Errorf("discovery did not reach %s: the walk or the parser for its form is blind", a)
			}
		}
		for _, k := range []string{"compose", "quadlet", "systemd", "envfile"} {
			if kinds[k] == 0 {
				t.Errorf("no %s launcher was found: that file form is not being read", k)
			}
		}
		c := checkCtx{ex: exceptions{byRow: ex, now: time.Now()}, tr: tr}
		var all []finding
		for _, l := range d.Launchers {
			all = append(all, c.check(l)...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].String() < all[j].String() })
		for _, f := range all {
			t.Error(f)
		}
		var names []string
		for _, l := range d.Launchers {
			names = append(names, l.Name)
		}
		t.Logf("%d launchers judged: %v", len(d.Launchers), kinds)
	})

	t.Run("breaking_a_shipped_file_is_flagged", func(t *testing.T) {
		shippedFilesMutated(t, doc)
	})

	t.Run("an_empty_scan_fails", func(t *testing.T) {
		if err := scanned(discover(tree{})); err == nil {
			t.Error("a scan of an empty tree passed")
		}
		if err := scanned(discover(tree{"deploy/x/compose.yaml": "services:\n  web:\n    image: nginx\n"})); err == nil {
			t.Error("a scan that found no LocalAI launcher passed")
		}
		stray := composeTree(cleanSample())
		moved := tree{}
		for p, s := range stray {
			moved[strings.Replace(p, "deploy/x/", "scripts/", 1)] = s
		}
		if d := discover(moved); len(d.Problems) == 0 {
			t.Error("a LocalAI launcher outside deploy/ was not reported")
		}
		if d := discover(tree{"deploy/x/compose.yaml": "services: [", "deploy/q/a.container": "garbage"}); len(d.Problems) < 2 {
			t.Errorf("a file the parser cannot read was skipped silently: %v", d.Problems)
		}
	})

	t.Run("clean_sample_passes_in_every_form", func(t *testing.T) {
		for _, form := range sampleForms {
			fs, n, problems := checkTree(form.tree(cleanSample()), exceptionsDoc("2027-04-03", allExceptions...))
			if n == 0 {
				t.Errorf("%s: the clean sample was not recognised as a LocalAI launcher", form.name)
			}
			for _, p := range problems {
				t.Errorf("%s: %s", form.name, p)
			}
			for _, f := range fs {
				t.Errorf("%s: the clean sample is flagged: %s", form.name, f)
			}
		}
	})

	t.Run("planted_violations_are_flagged", func(t *testing.T) {
		cases := plantedViolations()
		if len(cases) < 60 {
			t.Fatalf("only %d planted violations are defined", len(cases))
		}
		covered := map[string]bool{}
		for _, pc := range cases {
			s := cleanSample()
			pc.apply(&s)
			docText := exceptionsDoc("2027-04-03", allExceptions...)
			if pc.noException != "" {
				var keep []string
				for _, id := range allExceptions {
					if id != pc.noException {
						keep = append(keep, id)
					}
				}
				docText = exceptionsDoc("2027-04-03", keep...)
			}
			forms := pc.forms
			if len(forms) == 0 {
				forms = defaultForms
			}
			for _, name := range forms {
				form := formByName(name)
				fs, n, problems := checkTree(form.tree(pc.build(s, form)), docText)
				if n == 0 {
					t.Errorf("%s/%s: the planted sample was not recognised as a launcher", pc.name, name)
					continue
				}
				for _, p := range problems {
					t.Errorf("%s/%s: %s", pc.name, name, p)
				}
				if !hasRule(fs, pc.rule) {
					t.Errorf("%s/%s: the planted violation was not flagged as %s (findings: %v)", pc.name, name, pc.rule, fs)
					continue
				}
				covered[pc.rule] = true
			}
		}
		for _, r := range allRules() {
			if !covered[r] {
				t.Errorf("no planted violation proves the detector flags %s", r)
			}
		}
	})

	t.Run("flags_on_the_command_line_count", func(t *testing.T) {
		// A setting given as --flag instead of an environment variable is judged the same, and a flag wins over the
		// environment: a clean environment does not hide a flag that switches a surface on.
		s := cleanSample()
		s.flags = []string{"--disable-webui=false"}
		fs, _, _ := checkTree(composeTree(s), exceptionsDoc("2027-04-03", allExceptions...))
		if !hasRule(fs, "HC-12") {
			t.Errorf("--disable-webui=false was not flagged: %v", fs)
		}
		s = cleanSample()
		s.flags = []string{"--p2p"}
		fs, _, _ = checkTree(composeTree(s), exceptionsDoc("2027-04-03", allExceptions...))
		if !hasRule(fs, "HC-17") {
			t.Errorf("a bare --p2p (true) was not flagged: %v", fs)
		}
		s = cleanSample()
		s.flags = []string{"--disable-webui=true", "--log-level", "info", "--address", ":8080"}
		fs, _, _ = checkTree(composeTree(s), exceptionsDoc("2027-04-03", allExceptions...))
		if len(fs) != 0 {
			t.Errorf("clean flags are flagged: %v", fs)
		}
	})

	t.Run("exceptions_relax_exactly_their_row", func(t *testing.T) {
		exceptionBoundaries(t)
	})
}

// exceptionBoundaries proves that an exception relaxes the row it names and no other: each of the four entries is
// accepted where it applies and rejected beside it, and an expired or missing entry relaxes nothing.
func exceptionBoundaries(t *testing.T) {
	all := exceptionsDoc("2027-04-03", allExceptions...)
	judge := func(s sample, doc string) []finding {
		t.Helper()
		fs, n, problems := checkTree(composeTree(s), doc)
		if n == 0 || len(problems) > 0 {
			t.Fatalf("the sample was not read: %d launchers, %v", n, problems)
		}
		return fs
	}
	without := func(ids ...string) string {
		var keep []string
		for _, id := range allExceptions {
			drop := false
			for _, d := range ids {
				drop = drop || d == id
			}
			if !drop {
				keep = append(keep, id)
			}
		}
		return exceptionsDoc("2027-04-03", keep...)
	}

	// EX-03 and EX-04 let OPAQUE_ERRORS and the METRICS switch be unset, and nothing else.
	s := cleanSample()
	s.absent["LOCALAI_OPAQUE_ERRORS"], s.absent["LOCALAI_DISABLE_METRICS_ENDPOINT"] = true, true
	if fs := judge(s, all); len(fs) != 0 {
		t.Errorf("EX-03 and EX-04 do not relax their rows: %v", fs)
	}
	if fs := judge(s, without("EX-03")); !hasRule(fs, "HC-20") || hasRule(fs, "HC-21") {
		t.Errorf("without EX-03 only HC-20 may be flagged: %v", fs)
	}
	if fs := judge(s, without("EX-04")); !hasRule(fs, "HC-21") || hasRule(fs, "HC-20") {
		t.Errorf("without EX-04 only HC-21 may be flagged: %v", fs)
	}
	s.env["LOCALAI_DISABLE_WEBUI"] = "false"
	if fs := judge(s, all); !hasRule(fs, "HC-12") {
		t.Errorf("an exception for another row relaxed the web UI rule: %v", fs)
	}

	// EX-01: one local catalogue, nothing else.
	for name, bad := range map[string]string{
		"a remote catalogue":      `[{"name":"u","url":"https://example.invalid/gallery.yaml"}]`,
		"a second catalogue":      `[{"name":"a","url":"file:///backends/a.yaml"},{"name":"b","url":"file:///backends/b.yaml"}]`,
		"a file outside backends": `[{"name":"a","url":"file:///etc/gallery.yaml"}]`,
		"not a list":              `https://example.invalid/gallery.yaml`,
	} {
		s := cleanSample()
		s.env["LOCALAI_BACKEND_GALLERIES"] = bad
		if fs := judge(s, all); !hasRule(fs, "HC-14") {
			t.Errorf("EX-01 accepted %s: %v", name, fs)
		}
	}
	s = cleanSample()
	s.galleryRO = false
	if fs := judge(s, all); !hasRule(fs, "HC-14") {
		t.Errorf("EX-01 accepted a writable catalogue mount: %v", fs)
	}
	s = cleanSample()
	s.env["LOCALAI_GALLERIES"] = `[{"name":"u","url":"https://example.invalid/gallery.yaml"}]`
	if fs := judge(s, all); !hasRule(fs, "HC-14") {
		t.Errorf("EX-01 relaxed the model gallery, which it does not name: %v", fs)
	}
	if fs := judge(cleanSample(), without("EX-01")); !hasRule(fs, "HC-14") {
		t.Errorf("without EX-01 the local catalogue was accepted: %v", fs)
	}
	s = cleanSample()
	s.env["LOCALAI_AUTOLOAD_BACKEND_GALLERIES"] = "true"
	if fs := judge(s, all); !hasRule(fs, "HC-14") {
		t.Errorf("EX-01 relaxed the autoload switch: %v", fs)
	}

	// EX-02: catalogue names, resolved in the EX-01 catalogue; never a location, never the gRPC variant.
	for name, bad := range map[string]string{
		"a URL":                   "https://example.invalid/backend.tar",
		"an OCI ref":              "oci://example.invalid/backend:latest",
		"a path":                  "/opt/backend",
		"an unknown one":          "no-such-backend",
		"a list with one unknown": "cpu-llama-cpp,no-such-backend",
	} {
		s := cleanSample()
		s.env["LOCALAI_EXTERNAL_BACKENDS"] = bad
		if fs := judge(s, all); !hasRule(fs, "HC-18") {
			t.Errorf("EX-02 accepted %s: %v", name, fs)
		}
	}
	s = cleanSample()
	s.env["LOCALAI_EXTERNAL_GRPC_BACKENDS"] = "x:127.0.0.1:9000"
	if fs := judge(s, all); !hasRule(fs, "HC-18") {
		t.Errorf("EX-02 relaxed LOCALAI_EXTERNAL_GRPC_BACKENDS: %v", fs)
	}
	if fs := judge(cleanSample(), without("EX-02")); !hasRule(fs, "HC-18") {
		t.Errorf("without EX-02 the backend names were accepted: %v", fs)
	}
	if fs := judge(cleanSample(), without("EX-01")); !hasRule(fs, "HC-18") {
		t.Errorf("EX-02 stands without EX-01, whose catalogue resolves its names: %v", fs)
	}

	// Expired: the date is the day the entry must be re-examined, and an expired entry relaxes nothing.
	expired := exceptionsDoc("2026-10-03", allExceptions...)
	s = cleanSample()
	s.absent["LOCALAI_OPAQUE_ERRORS"] = true
	if fs := judge(s, expired); !hasRule(fs, "HC-20") {
		t.Errorf("an expired exception still relaxes its row: %v", fs)
	}
	// A row that does not read as an exception (no owner, no reason, no date) is reported and relaxes nothing.
	broken := "## 6. Exceptions\n\n| EX-03 | HC-20 | short | | no date | none |\n"
	if _, problems := parseExceptions(broken); len(problems) == 0 {
		t.Error("an exception without owner, reason and date was accepted")
	}
	// Rows outside section 6 are not exceptions.
	if ex, _ := parseExceptions(exceptionsDoc("2027-04-03")); len(ex) != 0 {
		t.Errorf("rows outside the exceptions table were read as exceptions: %v", ex)
	}
}

// shippedMutations breaks one line of a real shipped file, in memory, and names the checklist row that must then be
// reported. It proves that the parser of each file form reads the real files and not only the samples of this suite.
var shippedMutations = []struct {
	file, old, new, rule string
}{
	{"deploy/container/stack/compose.yaml", `LOCALAI_DISABLE_MCP: "true"`, `LOCALAI_DISABLE_MCP: "false"`, "HC-16"},
	{"deploy/container/stack/compose.yaml", `LOCALAI_LOG_LEVEL: info`, `LOCALAI_LOG_LEVEL: debug`, "HC-34"},
	{"deploy/container/stack/compose.yaml", `${LOCALAI_API_KEY:?set LOCALAI_API_KEY in .env (cp .env.example .env, then openssl rand -hex 32)}`, `0123456789abcdef0123456789abcdef`, "HC-06"},
	{"deploy/container/stack/compose.yaml", `@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b`, ``, "HC-27"},
	{"deploy/container/stack/compose.yaml", `${LOCALAI_EXTERNAL_BACKENDS:-cpu-llama-cpp}`, `https://example.invalid/b.tar`, "HC-18"},
	{"deploy/container/stack/compose.lite.yaml", `memswap_limit: 8g`, `memswap_limit: 16g`, "HC-33"},
	{"deploy/container/stack/compose.balanced.yaml", `${LOCALAI_MAX_ACTIVE_BACKENDS:-4}`, `${LOCALAI_MAX_ACTIVE_BACKENDS:-3}`, "HC-29"},
	{"deploy/container/localai/compose.yaml", `- "127.0.0.1:8080:8080"`, `- "8080:8080"`, "HC-02"},
	{"deploy/container/localai/compose.yaml", `LOCALAI_P2P: "false"`, `LOCALAI_P2P: "true"`, "HC-17"},
	{"deploy/container/localai/compose.yaml", `./data/models:/models`, `models:/models`, "HC-24"},
	{"deploy/container/localai/.env.example", `LOCALAI_EXTERNAL_BACKENDS=cpu-llama-cpp`, `LOCALAI_EXTERNAL_BACKENDS=cpu-llama-cpp
LOCALAI_CORS=true`, "HC-19"},
	{"deploy/container/quadlet/localai.env.example", `LOCALAI_P2P=false`, `LOCALAI_P2P=true`, "HC-17"},
	{"deploy/container/quadlet/localai.env.example", `LOCALAI_DISABLE_WEBUI=true`, `LOCALAI_DISABLE_WEBUI=false`, "HC-12"},
	{"deploy/container/quadlet/alixai-localai.container", `DropCapability=ALL`, ``, "HC-33"},
	{"deploy/container/quadlet/alixai-localai.container", `Image=docker.io/localai/localai:v4.10.0@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b`, `Image=docker.io/localai/localai:v4.10.0`, "HC-27"},
	{"deploy/container/quadlet/profiles/4g/alixai-localai.container.d/10-profile.conf", `--memory-swap=4g`, `--memory-swap=8g`, "HC-33"},
	{"deploy/container/quadlet/profiles/balanced/alixai-localai.container.d/10-profile.conf", `LOCALAI_MAX_ACTIVE_BACKENDS=4`, `LOCALAI_MAX_ACTIVE_BACKENDS=2`, "HC-29"},
	{"deploy/native/systemd/localai.yaml", `- LOCALAI_DISABLE_AGENTS=true`, `- LOCALAI_DISABLE_AGENTS=false`, "HC-15"},
	{"deploy/native/systemd/localai.yaml", `- LOCALAI_ADDRESS=127.0.0.1:8080`, `- LOCALAI_ADDRESS=0.0.0.0:8080`, "HC-01"},
	{"deploy/native/systemd/localai.service", `MemorySwapMax=0`, `MemorySwapMax=infinity`, "HC-33"},
	{"deploy/native/systemd/localai.service", `CapabilityBoundingSet=
`, ``, "HC-33"},
}

func shippedFilesMutated(t *testing.T, doc string) {
	ex, _ := parseExceptions(doc)
	real := repoTree(t)
	for _, m := range shippedMutations {
		src, ok := real[m.file]
		if !ok {
			t.Errorf("%s is not in the scanned tree: the mutation list is stale", m.file)
			continue
		}
		if !strings.Contains(src, m.old) {
			t.Errorf("%s no longer contains %q: update the mutation", m.file, m.old)
			continue
		}
		mut := tree{}
		for p, s := range real {
			mut[p] = s
		}
		mut[m.file] = strings.ReplaceAll(src, m.old, m.new)
		d := discover(mut)
		if err := scanned(d); err != nil {
			t.Errorf("%s: %v", m.file, err)
			continue
		}
		c := checkCtx{ex: exceptions{byRow: ex, now: time.Now()}, tr: mut}
		flagged := false
		for _, l := range d.Launchers {
			for _, f := range c.check(l) {
				if f.Rule == m.rule && strings.Contains(strings.Join(l.Files, " "), m.file) {
					flagged = true
				}
			}
		}
		if !flagged {
			t.Errorf("breaking %s (%q -> %q) was not flagged as %s", m.file, m.old, m.new, m.rule)
		}
	}
}
