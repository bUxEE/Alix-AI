package localaisec

import (
	"strings"
)

// sampleForm renders a sample in one file form.
type sampleForm struct {
	name string
	tree func(sample) tree
}

var sampleForms = []sampleForm{
	{"compose-map", composeTree},
	{"compose-list", func(s sample) tree { s.envAsList = true; return composeTree(s) }},
	{"compose-flags", composeTree},
	{"quadlet", quadletTree},
	{"systemd", func(s sample) tree {
		// A native LocalAI listens on a loopback address; the container forms bind inside their namespace.
		if s.env["LOCALAI_ADDRESS"] == ":8080" && !s.keepAddr {
			s = s.clone()
			s.env["LOCALAI_ADDRESS"] = "127.0.0.1:8080"
		}
		return systemdTree(s)
	}},
	{"envfile", envFileTree},
}

var defaultForms = []string{"compose-map", "compose-list", "compose-flags", "quadlet", "systemd", "envfile"}

func formByName(name string) sampleForm {
	for _, f := range sampleForms {
		if f.name == name {
			return f
		}
	}
	panic("unknown form " + name)
}

// planted is one violation: what to break in the clean sample, which checklist row must report it, and in which file
// forms it can be expressed.
type planted struct {
	name        string
	rule        string
	apply       func(*sample)
	forms       []string
	noException string // the exception entry to leave out of the checklist table for this case
	flag        string // the --flag form of the same setting, for the compose-flags form
	envVar      string // the variable that flag replaces
}

// build returns the sample as the given form carries it: in the flags form the setting moves from the environment to
// the command line.
func (pc planted) build(s sample, f sampleForm) sample {
	if f.name == "compose-flags" && pc.flag != "" {
		s = s.clone()
		s.absent[pc.envVar] = true
		s.flags = append(s.flags, pc.flag)
	}
	return s
}

func flagFor(name, val string) string {
	if !strings.HasPrefix(name, "LOCALAI_") || val == "" || name == "LOCALAI_API_KEY" {
		return ""
	}
	f := "--" + strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, "LOCALAI_"), "_", "-"))
	switch name {
	case "LOCALAI_AUTH":
		f = "--auth-enabled"
	}
	return f + "=" + val
}

func dropForms(forms []string, drop ...string) []string {
	var out []string
	for _, f := range forms {
		keep := true
		for _, d := range drop {
			keep = keep && f != d
		}
		if keep {
			out = append(out, f)
		}
	}
	return out
}

// setVar plants a value for a setting. In the flags form the variable is removed from the environment so the flag is
// the only source.
func setVar(rule, name, val string) planted {
	pc := planted{
		name: rule + " " + name + "=" + val,
		rule: rule,
		apply: func(s *sample) {
			s.env[name] = val
			s.absent[name] = false
		},
		forms:  dropForms(defaultForms, "compose-flags"),
		flag:   flagFor(name, val),
		envVar: name,
	}
	if pc.flag != "" {
		pc.forms = defaultForms
	}
	return pc
}

// dropVar plants the absence of a setting. An env file carries only part of a configuration, so it is not asked to
// carry the setting.
func dropVar(rule, name string) planted {
	return planted{
		name:  rule + " " + name + " absent",
		rule:  rule,
		apply: func(s *sample) { s.absent[name] = true },
		forms: []string{"compose-map", "compose-list", "quadlet", "systemd"},
	}
}

func plantedViolations() []planted {
	var out []planted
	for _, r := range requireTrue {
		out = append(out, setVar(r.row, r.name, "false"), setVar(r.row, r.name, ""), dropVar(r.row, r.name))
	}
	for _, r := range requireFalse {
		out = append(out, setVar(r.row, r.name, "true"), dropVar(r.row, r.name))
	}
	for i, r := range requireTrueUnlessExcepted {
		ex := []string{"EX-03", "EX-04"}[i]
		a, b := setVar(r.row, r.name, "false"), dropVar(r.row, r.name)
		a.noException, b.noException = ex, ex
		a.name, b.name = a.name+" without "+ex, b.name+" without "+ex
		out = append(out, a, b)
	}
	for _, r := range forbidden {
		val := "true"
		if r.name == "DEBUG" {
			val = "1"
		}
		out = append(out, setVar(r.row, r.name, val))
	}
	out = append(out,
		setVar("HC-34", "LOCALAI_LOG_LEVEL", "debug"),
		setVar("HC-34", "LOCALAI_LOG_LEVEL", "TRACE"),
	)

	// HC-06: the key.
	for _, k := range []string{"none", "empty", "literal:0123456789abcdef0123456789abcdef", "default:0123456789abcdef0123456789abcdef"} {
		k := k
		forms := []string{"compose-map", "compose-list", "quadlet", "systemd"}
		if strings.HasPrefix(k, "literal:") {
			forms = append(forms, "envfile")
		}
		out = append(out, planted{name: "HC-06 key " + strings.SplitN(k, ":", 2)[0], rule: "HC-06", apply: func(s *sample) { s.key = k }, forms: forms})
	}

	// HC-14, HC-18: galleries and external backends.
	out = append(out,
		setVar("HC-14", "LOCALAI_GALLERIES", `[{"name":"u","url":"https://example.invalid/gallery.yaml"}]`),
		setVar("HC-14", "LOCALAI_BACKEND_GALLERIES", `[{"name":"u","url":"https://example.invalid/gallery.yaml"}]`),
		setVar("HC-18", "LOCALAI_EXTERNAL_BACKENDS", "https://example.invalid/backend.tar"),
		setVar("HC-18", "LOCALAI_EXTERNAL_BACKENDS", "no-such-backend"),
		setVar("HC-18", "LOCALAI_EXTERNAL_GRPC_BACKENDS", "x:127.0.0.1:9000"),
	)
	g := setVar("HC-14", "LOCALAI_BACKEND_GALLERIES", cleanGallery)
	g.noException, g.name = "EX-01", g.name+" without EX-01"
	e := setVar("HC-18", "LOCALAI_EXTERNAL_BACKENDS", "cpu-llama-cpp")
	e.noException, e.name = "EX-02", e.name+" without EX-02"
	out = append(out, g, e)

	// HC-29: the resident backends.
	out = append(out,
		dropVar("HC-29", "LOCALAI_MAX_ACTIVE_BACKENDS"),
		setVar("HC-29", "LOCALAI_MAX_ACTIVE_BACKENDS", "0"),
		setVar("HC-29", "LOCALAI_MAX_ACTIVE_BACKENDS", "many"),
		planted{name: "HC-29 lite with one slot", rule: "HC-29", forms: []string{"compose-map", "quadlet"},
			apply: func(s *sample) { s.profile, s.profileMax = "lite", "1" }},
	)

	// HC-01: the address of a native LocalAI.
	out = append(out,
		planted{name: "HC-01 native on all interfaces", rule: "HC-01", forms: []string{"systemd"},
			apply: func(s *sample) { s.env["LOCALAI_ADDRESS"] = "0.0.0.0:8080"; s.keepAddr = true }},
		planted{name: "HC-01 native with the default :8080", rule: "HC-01", forms: []string{"systemd"},
			apply: func(s *sample) { s.env["LOCALAI_ADDRESS"] = ":8080"; s.keepAddr = true }},
		planted{name: "HC-01 native without an address", rule: "HC-01", forms: []string{"systemd"},
			apply: func(s *sample) { s.absent["LOCALAI_ADDRESS"] = true }},
	)

	// Structure.
	containerForms := []string{"compose-map", "compose-list", "quadlet"}
	allBuilt := []string{"compose-map", "compose-list", "quadlet", "systemd"}
	structural := func(name, rule string, forms []string, f func(*sample)) planted {
		return planted{name: name, rule: rule, forms: forms, apply: f}
	}
	out = append(out,
		structural("image by tag", "HC-27", containerForms, func(s *sample) { s.image = "docker.io/localai/localai:v4.10.0" }),
		structural("image latest", "HC-27", containerForms, func(s *sample) { s.image = "docker.io/localai/localai:latest" }),
		structural("image with a short digest", "HC-27", containerForms, func(s *sample) { s.image = "docker.io/localai/localai@sha256:abc123" }),
		structural("port on all interfaces, short", "HC-02", containerForms, func(s *sample) { s.ports = []string{"8080:8080"} }),
		structural("port on 0.0.0.0", "HC-02", containerForms, func(s *sample) { s.ports = []string{"0.0.0.0:8080:8080"} }),
		structural("port on ::", "HC-02", containerForms, func(s *sample) { s.ports = []string{"[::]:8080:8080"} }),
		structural("one of two ports public", "HC-02", containerForms, func(s *sample) { s.ports = []string{"127.0.0.1:8080:8080", "9090:9090"} }),
		structural("host network", "HC-02", containerForms, func(s *sample) { s.netMode = "host" }),
		structural("named volume for models", "HC-24", containerForms, func(s *sample) {
			s.volumes[0], s.topVols = "models:/models", []string{"models"}
		}),
		structural("named volume for backends", "HC-24", containerForms, func(s *sample) {
			s.volumes[1], s.topVols = "backends:/backends", []string{"backends"}
		}),
		structural("anonymous volume for data", "HC-24", containerForms, func(s *sample) { s.volumes[2] = "/data" }),
		structural("swap above memory", "HC-33", allBuilt, func(s *sample) { s.memSwap = "16g" }),
		structural("no swap limit", "HC-33", allBuilt, func(s *sample) { s.memSwap = "" }),
		structural("capabilities not dropped", "HC-33", allBuilt, func(s *sample) { s.capDrop = nil }),
		structural("another capability dropped", "HC-33", containerForms, func(s *sample) { s.capDrop = []string{"NET_RAW"} }),
		structural("privileged", "HC-33", containerForms, func(s *sample) { s.privileged = true }),
		structural("init service", "STRUCT", allBuilt, func(s *sample) { s.service = "localai-init" }),
		structural("a second service named init", "STRUCT", containerForms, func(s *sample) { s.extra = []string{"model-init"} }),
	)
	return out
}
