package localaisec

import (
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/tests/security/secutil"
)

// agentsMCPProblems judges one launcher against HC-15 and HC-16: server-side agents and MCP are switched off. An env
// file is judged on the values it sets; every other launcher must set both.
func agentsMCPProblems(l *launcher) []string {
	var ps []string
	for _, name := range []string{"LOCALAI_DISABLE_AGENTS", "LOCALAI_DISABLE_MCP"} {
		s := l.Env[name]
		switch {
		case s == nil:
			if !l.EnvOnly {
				ps = append(ps, fmt.Sprintf("%s: %s is not set (agents and MCP are on by default)", l.Name, name))
			}
		case s.External:
			ps = append(ps, fmt.Sprintf("%s: %s is taken from the host environment", l.Name, name))
		default:
			for _, v := range s.Values {
				if !strings.EqualFold(strings.TrimSpace(v), "true") {
					ps = append(ps, fmt.Sprintf("%s: %s=%q, want true", l.Name, name, v))
				}
			}
		}
	}
	return ps
}

// forbiddenModelKeys are the model-configuration keys that give a model its own agent loop or its own tool servers:
// LocalAI's `agent:` stanza and the `mcp:` stanza with its stdio (a command run by the LocalAI process) and remote
// servers. Compared without case, dashes and underscores.
var forbiddenModelKeys = map[string]string{
	"mcp":        "an MCP stanza (server-side MCP runs stdio commands and calls remote servers from the runtime)",
	"mcpservers": "an MCP server list",
	"agent":      "an agent stanza (the runtime would loop tool calls on its own)",
	"agents":     "an agent list",
	"stdio":      "a stdio command (the runtime would run it)",
}

var trustRemoteCodeRE = regexp.MustCompile(`(?i)trust[_-]?remote[_-]?code["']?\s*[:=]\s*["']?(true|yes|on|1)\b`)

func normKey(k string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(k))
}

// walkYAML visits every mapping key and every string scalar of a YAML value, with the path to it.
func walkYAML(v any, at string, key func(path, k string, v any), str func(path, s string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			key(at+"."+k, k, val)
			walkYAML(val, at+"."+k, key, str)
		}
	case []any:
		for i, e := range x {
			walkYAML(e, fmt.Sprintf("%s[%d]", at, i), key, str)
		}
	case string:
		str(at, x)
	}
}

// modelConfigProblems scans the YAML a runtime could load as a model definition: no agent or MCP stanza, no
// trust_remote_code. It returns the problems and the number of files it read.
func modelConfigProblems(tr tree, roots ...string) (problems []string, scanned int) {
	for _, rel := range tr.sorted() {
		if ext := path.Ext(rel); ext != ".yaml" && ext != ".yml" {
			continue
		}
		under := false
		for _, r := range roots {
			under = under || strings.HasPrefix(rel, r)
		}
		if !under {
			continue
		}
		var docs []any
		dec := yaml.NewDecoder(strings.NewReader(tr[rel]))
		for {
			var d any
			if err := dec.Decode(&d); err != nil {
				if !errors.Is(err, io.EOF) {
					problems = append(problems, fmt.Sprintf("%s: not valid YAML: %v", rel, err))
				}
				break
			}
			docs = append(docs, d)
		}
		scanned++
		for _, d := range docs {
			walkYAML(d, "", func(p, k string, val any) {
				if why, bad := forbiddenModelKeys[normKey(k)]; bad {
					problems = append(problems, fmt.Sprintf("%s: %s holds %s", rel, p, why))
				}
				if normKey(k) == "trustremotecode" && truthy(val) {
					problems = append(problems, fmt.Sprintf("%s: %s sets trust_remote_code: a model that runs code from its own repository", rel, p))
				}
			}, func(p, s string) {
				if trustRemoteCodeRE.MatchString(s) {
					problems = append(problems, fmt.Sprintf("%s: %s sets trust_remote_code to true", rel, p))
				}
			})
		}
		if m := trustRemoteCodeRE.FindString(tr[rel]); m != "" {
			problems = append(problems, fmt.Sprintf("%s: the text sets %q", rel, m))
		}
	}
	sort.Strings(problems)
	return dedupeStrings(problems), scanned
}

func dedupeStrings(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// ---- the Realtime session ----

const realtimeDir = "adapters/localai/realtime"

var (
	toolChoiceNoneRE    = regexp.MustCompile(`(?i)tool_?choice["']?\s*[:=,]?\s*[^\n]{0,60}["']none["']`)
	createResponseOffRE = regexp.MustCompile(`(?i)create_?response["']?\s*[:=,]?\s*[^\n]{0,30}\bfalse\b`)
	toolChoiceOnRE      = regexp.MustCompile(`(?i)tool_?choice["']?\s*[:=,]?\s*[^\n]{0,60}["'](auto|required)["']`)
	createResponseOnRE  = regexp.MustCompile(`(?i)create_?response["']?\s*[:=,]?\s*[^\n]{0,30}\btrue\b`)
)

// realtimeSessionProblems judges the Go source of the Realtime adapter: the session it configures must turn model-side
// tool calling off (tool_choice "none") and autonomous responses off (turn detection with create_response false), and
// must not anywhere set either of them the other way. It is a source check because the session configuration is a
// literal the adapter sends; the adapter's own behavioural test is TestRealtimeToolCallingDisabled.
func realtimeSessionProblems(src map[string]string) []string {
	var all []string
	names := make([]string, 0, len(src))
	for n := range src {
		names = append(names, n)
	}
	sort.Strings(names)
	var ps []string
	for _, n := range names {
		all = append(all, src[n])
		if toolChoiceOnRE.MatchString(src[n]) {
			ps = append(ps, n+": sets tool_choice to auto or required")
		}
		if createResponseOnRE.MatchString(src[n]) {
			ps = append(ps, n+": sets create_response to true (the runtime would answer on its own)")
		}
	}
	joined := strings.Join(all, "\n")
	if !toolChoiceNoneRE.MatchString(joined) {
		ps = append(ps, `no session configuration sets tool_choice to "none"`)
	}
	if !createResponseOffRE.MatchString(joined) {
		ps = append(ps, "no session configuration sets create_response to false")
	}
	return ps
}

// SEC-070
func TestSecLocalAIAgentsAndMCPDisabled(t *testing.T) {
	t.Run("deployments_disable_agents_and_mcp", func(t *testing.T) {
		tr := repoTree(t)
		d := discover(tr)
		if err := scanned(d); err != nil {
			t.Fatalf("discovery: %v", err)
		}
		for _, l := range d.Launchers {
			for _, p := range agentsMCPProblems(l) {
				t.Error(p)
			}
		}
		t.Logf("%d launchers judged", len(d.Launchers))
	})

	t.Run("agent_and_mcp_switch_detector_flags_planted_violations", func(t *testing.T) {
		judge := func(tr tree) (problems []string, n int) {
			d := discover(tr)
			for _, l := range d.Launchers {
				problems = append(problems, agentsMCPProblems(l)...)
			}
			return problems, len(d.Launchers)
		}
		for _, form := range sampleForms {
			if ps, n := judge(form.tree(cleanSample())); n == 0 || len(ps) != 0 {
				t.Errorf("%s: the clean sample gave %d launchers and problems %v", form.name, n, ps)
			}
		}
		for _, name := range []string{"LOCALAI_DISABLE_AGENTS", "LOCALAI_DISABLE_MCP"} {
			for _, mut := range []struct {
				what  string
				apply func(*sample)
				forms []string
			}{
				{"false", func(s *sample) { s.env[name] = "false" }, defaultForms},
				{"empty", func(s *sample) { s.env[name] = "" }, defaultForms},
				{"absent", func(s *sample) { s.absent[name] = true }, []string{"compose-map", "compose-list", "quadlet", "systemd"}},
			} {
				for _, f := range mut.forms {
					s := cleanSample()
					mut.apply(&s)
					if f == "compose-flags" {
						continue
					}
					if ps, n := judge(formByName(f).tree(s)); n == 0 || len(ps) == 0 {
						t.Errorf("%s=%s in %s form: %d launchers, problems %v: not flagged", name, mut.what, f, n, ps)
					}
				}
			}
		}
		// the same settings given as command-line flags
		s := cleanSample()
		s.flags = []string{"--disable-agents=false"}
		if ps, _ := judge(composeTree(s)); len(ps) == 0 {
			t.Error("--disable-agents=false on the command line was not flagged")
		}
	})

	t.Run("shipped_model_configs_carry_no_agent_or_mcp_stanza", func(t *testing.T) {
		tr := repoTree(t)
		ps, n := modelConfigProblems(tr, "models/", "deploy/")
		if n == 0 {
			t.Fatal("no YAML under models/ or deploy/ was scanned")
		}
		if _, ok := tr["deploy/container/localai/config/models.yaml"]; !ok {
			t.Fatal("the shipped LocalAI model definitions (deploy/container/localai/config/models.yaml) were not in the scan")
		}
		for _, p := range ps {
			t.Error(p)
		}
		// Control on the real files: the same scan, over the real tree with one planted stanza, finds it.
		for name, plant := range map[string]string{
			"an mcp stanza":          "\n- name: planted\n  backend: llama-cpp\n  mcp:\n    stdio: '{\"mcpServers\":{\"x\":{\"command\":\"sh\"}}}'\n",
			"an agent stanza":        "\n- name: planted\n  backend: llama-cpp\n  agent:\n    max_iterations: 10\n",
			"trust_remote_code":      "\n- name: planted\n  backend: transformers\n  trust_remote_code: true\n",
			"trust_remote_code text": "\n- name: planted\n  options:\n    - trust_remote_code:true\n",
		} {
			mut := tree{}
			for p, s := range tr {
				mut[p] = s
			}
			mut["deploy/container/localai/config/models.yaml"] += plant
			if got, _ := modelConfigProblems(mut, "models/", "deploy/"); len(got) == 0 {
				t.Errorf("%s planted in the real models.yaml was not found", name)
			}
		}
	})

	t.Run("model_config_detector_flags_planted_violations", func(t *testing.T) {
		clean := "- name: m\n  backend: llama-cpp\n  context_size: 2048\n  known_usecases:\n    - chat\n  options:\n    - use_jinja:true\n  parameters:\n    model: m.gguf\n  template:\n    use_tokenizer_template: true\n"
		if ps, n := modelConfigProblems(tree{"models/m.yaml": clean}, "models/"); n != 1 || len(ps) != 0 {
			t.Errorf("a clean model config: %d scanned, problems %v", n, ps)
		}
		for name, bad := range map[string]string{
			"mcp key":                  "- name: m\n  mcp:\n    remote: '{}'\n",
			"MCP upper case":           "- name: m\n  MCP: {}\n",
			"mcp_servers":              "name: m\nmcp_servers: []\n",
			"agent stanza":             "name: m\nagent:\n  max_attempts: 3\n",
			"nested agent":             "name: m\nfoo:\n  - bar:\n      agents: []\n",
			"stdio":                    "name: m\nfoo:\n  stdio: sh\n",
			"trust_remote_code true":   "name: m\ntrust_remote_code: true\n",
			"trust_remote_code string": "name: m\ntrust_remote_code: \"True\"\n",
			"in an option":             "name: m\noptions:\n  - trust_remote_code:true\n",
			"in a second document":     "name: a\n---\nname: b\ntrust_remote_code: true\n",
		} {
			if ps, _ := modelConfigProblems(tree{"models/m.yaml": bad}, "models/"); len(ps) == 0 {
				t.Errorf("%s was not flagged", name)
			}
		}
		if ps, _ := modelConfigProblems(tree{"models/m.yaml": "name: m\ntrust_remote_code: false\n"}, "models/"); len(ps) != 0 {
			t.Errorf("trust_remote_code: false was flagged: %v", ps)
		}
		if _, n := modelConfigProblems(tree{"elsewhere/m.yaml": "mcp: {}\n"}, "models/"); n != 0 {
			t.Error("a file outside the scanned roots was scanned")
		}
	})

	t.Run("realtime_session_detector_flags_planted_violations", func(t *testing.T) {
		clean := map[string]string{"session.go": "package realtime\n\nvar session = map[string]any{\n\t\"tool_choice\": \"none\",\n\t\"turn_detection\": map[string]any{\"create_response\": false},\n}\n"}
		if ps := realtimeSessionProblems(clean); len(ps) != 0 {
			t.Errorf("a clean session is flagged: %v", ps)
		}
		clean2 := map[string]string{"s.go": "type Session struct {\n\tToolChoice string `json:\"tool_choice\"`\n}\n\nfunc New() Session { return Session{ToolChoice: \"none\", TurnDetection: TurnDetection{CreateResponse: false}} }\n"}
		if ps := realtimeSessionProblems(clean2); len(ps) != 0 {
			t.Errorf("a clean session in struct form is flagged: %v", ps)
		}
		for name, bad := range map[string]map[string]string{
			"tool_choice auto":      {"s.go": "var x = map[string]any{\"tool_choice\": \"auto\", \"create_response\": false}\n"},
			"create_response true":  {"s.go": "var x = map[string]any{\"tool_choice\": \"none\", \"create_response\": true}\n"},
			"nothing configured":    {"s.go": "package realtime\n"},
			"only one switch":       {"s.go": "var x = map[string]any{\"tool_choice\": \"none\"}\n"},
			"one file sets it back": {"a.go": "var a = map[string]any{\"tool_choice\": \"none\", \"create_response\": false}\n", "b.go": "var b = map[string]any{\"tool_choice\": \"required\"}\n"},
		} {
			if ps := realtimeSessionProblems(bad); len(ps) == 0 {
				t.Errorf("%s was not flagged", name)
			}
		}
	})

	t.Run("realtime_session_disables_tool_calling_and_autonomous_responses", func(t *testing.T) {
		src := map[string]string{}
		for _, rel := range secutil.RepoFiles(t) {
			if strings.HasPrefix(rel, realtimeDir+"/") && strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") {
				src[rel] = string(secutil.ReadFile(t, rel))
			}
		}
		if len(src) == 0 {
			t.Skip("NEEDS: " + realtimeDir + " does not exist (milestone M4-03 is not built): there is no Realtime session configuration to read, so the " +
				"Realtime half of SEC-070 cannot be asserted yet. The detector above is proven against planted sources and runs on the real package as soon as it lands.")
		}
		for _, p := range realtimeSessionProblems(src) {
			t.Error(p)
		}
	})

	// The live half: a running stack refuses the agent and MCP surfaces. A no-op without the live tag.
	secLocalAILiveAgentsProbe(t)
}

// truthy reads a YAML scalar as a switch.
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int:
		return x != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "on", "1":
			return true
		}
	}
	return false
}
