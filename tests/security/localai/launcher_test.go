package localaisec

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// setting is one LocalAI variable as a deployment file gives it.
type setting struct {
	// Values are every value the variable can have after substitution (requiredValue for a required substitution).
	Values []string
	// Ref marks a value delivered by reference (a credential file), never written in the file.
	Ref bool
	// External marks a variable named without a value, taken from the host environment at run time.
	External bool
	// FromFile marks a value read from an env file.
	FromFile bool
	From     string
}

type mount struct {
	Source, Target string
	ReadOnly       bool
	Named          bool // a named or anonymous volume, not a bind mount
	Raw            string
}

// launcher is one evaluated way of starting LocalAI: a compose service with its overrides merged, a Quadlet unit with
// its drop-ins, a systemd unit with the configuration it names, or an env file on its own.
type launcher struct {
	Name      string
	Kind      string // compose, quadlet, systemd, supervisor, envfile
	Dir       string
	Service   string
	Profile   string // lite, balanced, 4g or ""
	Files     []string
	Container bool
	EnvOnly   bool // an env file read by something else: only the values it sets are judged

	Env map[string]*setting

	Images       []string
	Ports        []string
	Mounts       []mount
	NamedVolumes map[string]bool
	NetworkMode  string
	Privileged   bool
	CapDrop      []string
	CapAdd       []string
	MemLimit     string
	MemSwap      string
	// Names are every service or unit name of the evaluation, for the "no init container" rule.
	Names []string

	// Native: the systemd equivalents of the container limits.
	MemoryMax         string
	MemorySwapMax     string
	CapBoundingSet    []string
	CapBoundingSetSet bool
}

func newLauncher(name, kind string) *launcher {
	return &launcher{Name: name, Kind: kind, Env: map[string]*setting{}, NamedVolumes: map[string]bool{}}
}

// set records a variable's values; a later source of higher precedence replaces an earlier one.
func (l *launcher) set(name string, s *setting) { l.Env[name] = s }

var (
	localAIImageRE = regexp.MustCompile(`(^|/)localai/localai([:@]|$)|(^|/)local-ai([:@]|$)`)
	profileRE      = regexp.MustCompile(`(?:^|[./])(lite|balanced|4g)(?:[./]|$)`)
	composeNameRE  = regexp.MustCompile(`^(?:(?:docker|podman)-)?compose(?:[.-][^/]*)?\.ya?ml$`)
	baseComposeRE  = regexp.MustCompile(`^(?:(?:docker|podman)-)?compose\.ya?ml$`)
)

// flagEnvName maps a LocalAI command-line flag to the environment variable that sets the same thing.
func flagEnvName(flag string) string {
	f := strings.TrimLeft(flag, "-")
	switch f {
	case "api-keys":
		return "LOCALAI_API_KEY"
	case "auth-enabled":
		return "LOCALAI_AUTH"
	case "debug":
		return "LOCALAI_DEBUG"
	}
	return "LOCALAI_" + strings.ToUpper(strings.ReplaceAll(f, "-", "_"))
}

func boolFlag(f string) bool {
	f = strings.TrimLeft(f, "-")
	for _, p := range []string{"disable-", "enable-", "autoload-", "use-"} {
		if strings.HasPrefix(f, p) {
			return true
		}
	}
	switch f {
	case "p2p", "federated", "cors", "auth-enabled", "opaque-errors", "debug":
		return true
	}
	return false
}

// applyArgs reads --flag, --flag=value and --flag value out of a command line and records them as the variables they
// set. A command-line flag wins over the environment, as it does in LocalAI.
func applyArgs(l *launcher, args []string, from string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") || len(a) < 3 {
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		if !hasVal {
			switch {
			case boolFlag(name):
				val = "true"
				if i+1 < len(args) && (args[i+1] == "true" || args[i+1] == "false") {
					val = args[i+1]
					i++
				}
			case i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
				val = args[i+1]
				i++
			}
		}
		l.set(flagEnvName(name), &setting{Values: []string{val}, From: from + " " + name})
	}
}

// ---- compose ----

func yamlDoc(text string) (map[string]any, error) {
	var doc any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, err
	}
	// A file whose root is a list (a model list, a backend catalogue) is data, not a compose file or a configuration.
	m, _ := doc.(map[string]any)
	return m, nil
}

func anyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	}
	return []any{v}
}

func anyMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// scalar renders a YAML scalar the way the shell sees it; ok is false for null.
func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	}
	return fmt.Sprint(v), true
}

// envMapOf normalises a compose environment: a mapping, or a list of KEY=VALUE and bare KEY entries (nil value).
func envMapOf(v any) map[string]any {
	out := map[string]any{}
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			out[k] = val
		}
	case []any:
		for _, e := range x {
			s, _ := scalar(e)
			k, val, ok := strings.Cut(s, "=")
			if ok {
				out[k] = val
			} else {
				out[k] = nil
			}
		}
	}
	return out
}

var appendKeys = map[string]bool{
	"ports": true, "expose": true, "volumes": true, "cap_add": true, "cap_drop": true,
	"security_opt": true, "tmpfs": true, "env_file": true, "devices": true, "networks": true,
}

// mergeService applies an override to a service the way Compose does: mappings merge, the list keys append, the rest
// is replaced.
func mergeService(base, over map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		switch {
		case k == "environment" || k == "labels":
			m := envMapOf(out[k])
			for ek, ev := range envMapOf(v) {
				m[ek] = ev
			}
			out[k] = m
		case appendKeys[k]:
			if bm, ok := out[k].(map[string]any); ok { // networks as a mapping
				if vm, ok := v.(map[string]any); ok {
					for nk, nv := range vm {
						bm[nk] = nv
					}
					continue
				}
			}
			out[k] = append(append([]any(nil), anyList(out[k])...), anyList(v)...)
		default:
			if bm, ok := out[k].(map[string]any); ok {
				if vm, ok := v.(map[string]any); ok {
					out[k] = mergeService(bm, vm)
					continue
				}
			}
			out[k] = v
		}
	}
	return out
}

func mergeComposeDocs(base map[string]any, overs ...map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for _, o := range overs {
		for k, v := range o {
			if k != "services" && k != "volumes" && k != "networks" {
				out[k] = v
				continue
			}
			bm, vm := anyMap(out[k]), anyMap(v)
			merged := map[string]any{}
			for ik, iv := range bm {
				merged[ik] = iv
			}
			for ik, iv := range vm {
				if k == "services" {
					if existing, ok := merged[ik].(map[string]any); ok {
						if ov, ok := iv.(map[string]any); ok {
							merged[ik] = mergeService(existing, ov)
							continue
						}
					}
				}
				merged[ik] = iv
			}
			out[k] = merged
		}
	}
	return out
}

func sizeBytes(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for suffix, m := range map[string]int64{"kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "b": 1} {
		if strings.HasSuffix(s, suffix) {
			s, mult = strings.TrimSuffix(s, suffix), m
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatInt(int64(n*float64(mult)), 10)
}

func bindSource(src string) bool {
	return strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "~") || strings.HasPrefix(src, "%h") ||
		strings.Contains(src, "/") || (len(src) > 1 && src[1] == ':')
}

// composeLauncher builds the launcher of one merged service, or nil when the service is not LocalAI.
func composeLauncher(tr tree, name, dir, svcName string, svc map[string]any, doc map[string]any, example map[string]string, files []string) *launcher {
	image, _ := scalar(svc["image"])
	imgs := expand(image, example)
	isLocalAI := false
	for _, i := range imgs {
		if localAIImageRE.MatchString(i) {
			isLocalAI = true
		}
	}
	if !isLocalAI {
		return nil
	}
	l := newLauncher(name, "compose")
	l.Dir, l.Service, l.Container, l.Files, l.Images = dir, svcName, true, files, imgs
	for n := range anyMap(doc["services"]) {
		l.Names = append(l.Names, n)
	}
	sort.Strings(l.Names)
	for n := range anyMap(doc["volumes"]) {
		l.NamedVolumes[n] = true
	}
	// env_file entries are the lowest precedence.
	for _, e := range anyList(svc["env_file"]) {
		ef, _ := scalar(e)
		for _, cand := range []string{path.Join(dir, ef), path.Join(dir, ef) + ".example"} {
			if txt, ok := tr.read(cand); ok {
				for k, v := range parseEnvLines(txt) {
					l.set(k, &setting{Values: expand(v, example), From: cand, FromFile: true})
				}
				l.Files = append(l.Files, cand)
				break
			}
		}
	}
	for k, v := range envMapOf(svc["environment"]) {
		s, ok := scalar(v)
		if !ok {
			l.set(k, &setting{External: true, From: name + " environment"})
			continue
		}
		l.set(k, &setting{Values: expand(s, example), From: name + " environment"})
	}
	for _, key := range []string{"entrypoint", "command"} {
		switch x := svc[key].(type) {
		case string:
			applyArgs(l, splitWords(x), name+" "+key)
		case []any:
			var args []string
			for _, a := range x {
				s, _ := scalar(a)
				args = append(args, s)
			}
			applyArgs(l, args, name+" "+key)
		}
	}
	for _, p := range anyList(svc["ports"]) {
		switch x := p.(type) {
		case map[string]any:
			host, _ := scalar(x["host_ip"])
			pub, _ := scalar(x["published"])
			tgt, _ := scalar(x["target"])
			spec := pub + ":" + tgt
			if host != "" {
				spec = host + ":" + spec
			}
			l.Ports = append(l.Ports, expand(spec, example)...)
		default:
			s, _ := scalar(p)
			l.Ports = append(l.Ports, expand(s, example)...)
		}
	}
	for _, v := range anyList(svc["volumes"]) {
		switch x := v.(type) {
		case map[string]any:
			typ, _ := scalar(x["type"])
			src, _ := scalar(x["source"])
			tgt, _ := scalar(x["target"])
			ro, _ := x["read_only"].(bool)
			l.Mounts = append(l.Mounts, mount{Source: src, Target: tgt, ReadOnly: ro, Named: typ == "volume", Raw: fmt.Sprint(x)})
		default:
			raw, _ := scalar(v)
			parts := splitVolume(raw)
			m := mount{Raw: raw}
			switch len(parts) {
			case 1:
				m.Target, m.Named = parts[0], true // anonymous volume
			default:
				srcs := expand(parts[0], example)
				m.Source, m.Target = srcs[0], parts[1]
				m.Named = !bindSource(m.Source) || l.NamedVolumes[m.Source]
				if len(parts) > 2 {
					for _, o := range strings.Split(parts[2], ",") {
						if o == "ro" {
							m.ReadOnly = true
						}
					}
				}
			}
			l.Mounts = append(l.Mounts, m)
		}
	}
	l.NetworkMode, _ = scalar(svc["network_mode"])
	l.Privileged, _ = svc["privileged"].(bool)
	for _, c := range anyList(svc["cap_drop"]) {
		s, _ := scalar(c)
		l.CapDrop = append(l.CapDrop, s)
	}
	for _, c := range anyList(svc["cap_add"]) {
		s, _ := scalar(c)
		l.CapAdd = append(l.CapAdd, s)
	}
	if s, ok := scalar(svc["mem_limit"]); ok {
		l.MemLimit = sizeBytes(expand(s, example)[0])
	}
	if s, ok := scalar(svc["memswap_limit"]); ok {
		l.MemSwap = sizeBytes(expand(s, example)[0])
	}
	if lim := anyMap(anyMap(anyMap(svc["deploy"])["resources"])["limits"]); lim != nil {
		if s, ok := scalar(lim["memory"]); ok && l.MemLimit == "" {
			l.MemLimit = sizeBytes(s)
		}
	}
	if m := profileRE.FindStringSubmatch(strings.Join(files[1:], " ")); m != nil && len(files) > 1 {
		l.Profile = m[1]
	}
	return l
}

// composeGroup evaluates the compose files of one directory: the base alone, the base with each override, and the
// base with each profile and each other override, which is what an operator runs (-f compose.yaml -f compose.lite.yaml).
func composeGroup(tr tree, dir string, files []string, docs map[string]map[string]any) []*launcher {
	example := map[string]string{}
	for _, cand := range []string{path.Join(dir, ".env"), path.Join(dir, ".env.example")} {
		if txt, ok := tr.read(cand); ok {
			for k, v := range parseEnvLines(txt) {
				example[k] = v
			}
			break
		}
	}
	var base string
	var overlays []string
	for _, f := range files {
		if base == "" && baseComposeRE.MatchString(path.Base(f)) {
			base = f
			continue
		}
		overlays = append(overlays, f)
	}
	var combos [][]string
	if base == "" {
		for _, f := range files {
			combos = append(combos, []string{f})
		}
	} else {
		combos = append(combos, []string{base})
		var profiles, others []string
		for _, o := range overlays {
			if profileRE.MatchString(path.Base(o)) {
				profiles = append(profiles, o)
			} else {
				others = append(others, o)
			}
		}
		for _, o := range overlays {
			combos = append(combos, []string{base, o})
		}
		for _, p := range profiles {
			for _, o := range others {
				combos = append(combos, []string{base, p, o})
			}
		}
	}
	var out []*launcher
	for _, combo := range combos {
		merged := docs[combo[0]]
		var overs []map[string]any
		for _, f := range combo[1:] {
			overs = append(overs, docs[f])
		}
		merged = mergeComposeDocs(merged, overs...)
		var svcNames []string
		for n := range anyMap(merged["services"]) {
			svcNames = append(svcNames, n)
		}
		sort.Strings(svcNames)
		for _, n := range svcNames {
			svc := anyMap(anyMap(merged["services"])[n])
			label := strings.Join(combo, "+") + "#" + n
			if l := composeLauncher(tr, label, dir, n, svc, merged, example, combo); l != nil {
				out = append(out, l)
			}
		}
	}
	return out
}

// ---- Quadlet and systemd ----

func unitTokens(vals []string) []string {
	var out []string
	for _, v := range vals {
		out = append(out, splitWords(v)...)
	}
	return out
}

// applyUnitEnv records Environment= and EnvironmentFile= of a unit; the file values are read from the tree, where an
// installed file name such as localai.env stands for the shipped localai.env.example.
func applyUnitEnv(l *launcher, tr tree, dir, section string, u iniUnit, from string) {
	for _, ef := range u.get(section, "EnvironmentFile") {
		ef = strings.TrimPrefix(ef, "-")
		base := path.Base(ef)
		for _, cand := range []string{path.Join(dir, base), path.Join(dir, base) + ".example"} {
			if txt, ok := tr.read(cand); ok {
				for k, v := range parseEnvLines(txt) {
					l.set(k, &setting{Values: []string{v}, From: cand, FromFile: true})
				}
				l.Files = append(l.Files, cand)
				break
			}
		}
	}
	for _, tok := range unitTokens(u.get(section, "Environment")) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		l.set(k, &setting{Values: []string{v}, From: from + " Environment"})
	}
}

// podmanArgs reads the memory caps and the publish and network options out of PodmanArgs=.
func applyPodmanArgs(l *launcher, args []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		next := func() string {
			if hasVal {
				return val
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch name {
		case "--memory", "-m":
			l.MemLimit = sizeBytes(next())
		case "--memory-swap":
			l.MemSwap = sizeBytes(next())
		case "--publish", "-p":
			l.Ports = append(l.Ports, next())
		case "--network", "--net":
			if v := next(); v != "" {
				l.NetworkMode = v
			}
		case "--privileged":
			l.Privileged = true
		case "--cap-add":
			l.CapAdd = append(l.CapAdd, next())
		case "--cap-drop":
			l.CapDrop = append(l.CapDrop, next())
		}
	}
}

func quadletLauncher(tr tree, rel string, base iniUnit, drops []iniUnit, dropNames []string, label, profile string) *launcher {
	u := base
	for _, d := range drops {
		u = u.merge(d)
	}
	l := newLauncher(label, "quadlet")
	l.Dir, l.Container, l.Profile = path.Dir(rel), true, profile
	l.Files = append([]string{rel}, dropNames...)
	l.Images = u.get("Container", "Image")
	l.Service = strings.TrimSuffix(path.Base(rel), ".container")
	l.Names = append(l.Names, l.Service)
	applyUnitEnv(l, tr, l.Dir, "Container", u, label)
	// PodmanArgs may carry flags of the container tool, and the exec line carries LocalAI's own.
	for _, a := range u.get("Container", "Exec") {
		applyArgs(l, splitWords(a), label+" Exec")
	}
	applyPodmanArgs(l, unitTokens(u.get("Container", "PodmanArgs")))
	l.Ports = append(l.Ports, u.get("Container", "PublishPort")...)
	for _, v := range u.get("Container", "Volume") {
		parts := splitVolume(v)
		m := mount{Raw: v}
		if len(parts) == 1 {
			m.Target, m.Named = parts[0], true
		} else {
			m.Source, m.Target = parts[0], parts[1]
			m.Named = !bindSource(m.Source)
			if len(parts) > 2 {
				for _, o := range strings.Split(parts[2], ",") {
					if o == "ro" {
						m.ReadOnly = true
					}
				}
			}
		}
		l.Mounts = append(l.Mounts, m)
	}
	if nets := u.get("Container", "Network"); len(nets) > 0 && l.NetworkMode == "" {
		for _, n := range nets {
			if n == "host" {
				l.NetworkMode = "host"
			}
		}
	}
	l.CapDrop = append(l.CapDrop, u.get("Container", "DropCapability")...)
	l.CapAdd = append(l.CapAdd, u.get("Container", "AddCapability")...)
	return l
}

type supervisorConfig struct {
	LocalAI struct {
		APIKeyRef string `yaml:"api_key_ref"`
	} `yaml:"localai"`
	Supervisor struct {
		Runtime string `yaml:"runtime"`
		Exec    struct {
			Path      string   `yaml:"path"`
			Args      []string `yaml:"args"`
			Env       []string `yaml:"env"`
			APIKeyEnv string   `yaml:"api_key_env"`
		} `yaml:"exec"`
	} `yaml:"supervisor"`
}

// supervisorLauncherFrom adds what a supervisor configuration launches to l: the child's whole environment, its
// arguments and the credential it receives by reference.
func supervisorLauncherFrom(l *launcher, cfg supervisorConfig, from string) {
	for _, kv := range cfg.Supervisor.Exec.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		l.set(k, &setting{Values: []string{v}, From: from + " supervisor.exec.env"})
	}
	applyArgs(l, cfg.Supervisor.Exec.Args, from+" supervisor.exec.args")
	if k := cfg.Supervisor.Exec.APIKeyEnv; k != "" && cfg.LocalAI.APIKeyRef != "" {
		if _, literal := l.Env[k]; !literal {
			l.set(k, &setting{Ref: true, From: from + " localai.api_key_ref"})
		}
	}
}

func execLaunchesLocalAI(lines []string) bool {
	for _, c := range lines {
		c = strings.TrimLeft(c, "-@+!:|")
		if strings.Contains(c, "local-ai") || strings.Contains(c, "localai/localai") || strings.Contains(c, "supervisor exec") {
			return true
		}
	}
	return false
}

// ---- discovery ----

// discovery is what discover found: the launchers, and everything about the scan that cannot be trusted.
type discovery struct {
	Launchers []*launcher
	// Problems are files that could not be parsed, and launchers found where the repository does not deploy from.
	Problems []string
}

// shipsFrom is the one tree a LocalAI deployment is shipped from.
const shipsFrom = "deploy/"

// discover walks the tree and finds every file that launches LocalAI, by structure and not by name: a compose
// service whose image is LocalAI's, a Quadlet container with that image, a systemd unit that runs LocalAI or the
// supervisor, a supervisor configuration with an exec section, an env file that sets a LOCALAI_ variable. A launcher
// found outside deploy/ is a Problem: a second way of starting LocalAI that nobody reviewed.
func discover(tr tree) discovery {
	var d discovery
	problem := func(f string, a ...any) { d.Problems = append(d.Problems, fmt.Sprintf(f, a...)) }
	paths := tr.sorted()

	composeDirs := map[string][]string{}
	composeDocs := map[string]map[string]any{}
	claimedConfigs := map[string]bool{}
	supCfgs := map[string]supervisorConfig{}

	// Pass 0 reads the YAML (compose files, supervisor configurations), pass 1 the units, which refer to them.
	for pass := 0; pass < 2; pass++ {
		for _, rel := range paths {
			text := tr[rel]
			base := path.Base(rel)
			ext := path.Ext(rel)
			if isYAML := ext == ".yaml" || ext == ".yml"; (pass == 0) != isYAML {
				continue
			}
			switch {
			case ext == ".yaml" || ext == ".yml":
				doc, err := yamlDoc(text)
				if err != nil {
					if strings.HasPrefix(rel, shipsFrom) {
						problem("%s: not valid YAML: %v", rel, err)
					}
					continue
				}
				if _, hasServices := doc["services"]; hasServices && (composeNameRE.MatchString(base) || strings.HasPrefix(rel, shipsFrom)) {
					composeDirs[path.Dir(rel)] = append(composeDirs[path.Dir(rel)], rel)
					composeDocs[rel] = doc
				}
				var sc supervisorConfig
				if yaml.Unmarshal([]byte(text), &sc) == nil && sc.Supervisor.Exec.Path != "" {
					supCfgs[rel] = sc
				}
			case ext == ".service":
				u, err := parseINI(text)
				if err != nil {
					problem("%s: %v", rel, err)
					continue
				}
				var execs []string
				for _, k := range []string{"ExecStart", "ExecStartPre", "ExecStartPost"} {
					execs = append(execs, u.get("Service", k)...)
				}
				if !execLaunchesLocalAI(execs) {
					continue
				}
				l := newLauncher(rel, "systemd")
				l.Dir, l.Files, l.Service = path.Dir(rel), []string{rel}, path.Base(rel)
				l.Names = []string{path.Base(rel)}
				// drop-ins of the unit
				for _, p := range paths {
					if path.Dir(p) == rel+".d" && path.Ext(p) == ".conf" {
						if du, derr := parseINI(tr[p]); derr == nil {
							u = u.merge(du)
							l.Files = append(l.Files, p)
						}
					}
				}
				applyUnitEnv(l, tr, l.Dir, "Service", u, rel)
				for _, c := range u.get("Service", "ExecStart") {
					words := splitWords(strings.TrimLeft(c, "-@+!:|"))
					applyArgs(l, words, rel+" ExecStart")
					for i := 0; i+1 < len(words); i++ {
						if (words[i] == "-config" || words[i] == "--config") && strings.Contains(c, "supervisor") {
							cfgBase := path.Base(words[i+1])
							for cand, sc := range supCfgs {
								if path.Dir(cand) == l.Dir && path.Base(cand) == cfgBase {
									supervisorLauncherFrom(l, sc, cand)
									l.Files = append(l.Files, cand)
									claimedConfigs[cand] = true
								}
							}
						}
					}
				}
				l.MemoryMax = u.one("Service", "MemoryMax")
				l.MemorySwapMax = u.one("Service", "MemorySwapMax")
				l.CapBoundingSetSet = u.present("Service", "CapabilityBoundingSet")
				l.CapBoundingSet = strings.Fields(strings.Join(u.get("Service", "CapabilityBoundingSet"), " "))
				d.Launchers = append(d.Launchers, l)
			case ext == ".container":
				u, err := parseINI(text)
				if err != nil {
					problem("%s: %v", rel, err)
					continue
				}
				isLocalAI := false
				for _, i := range u.get("Container", "Image") {
					if localAIImageRE.MatchString(i) {
						isLocalAI = true
					}
				}
				if !isLocalAI {
					continue
				}
				unitDir := path.Dir(rel)
				dropDir := path.Base(rel) + ".d"
				groups := map[string][]string{} // parent directory of the .d directory to its conf files
				for _, p := range paths {
					if path.Base(path.Dir(p)) == dropDir && path.Ext(p) == ".conf" && strings.HasPrefix(p, unitDir+"/") {
						groups[path.Dir(path.Dir(p))] = append(groups[path.Dir(path.Dir(p))], p)
					}
				}
				d.Launchers = append(d.Launchers, quadletLauncher(tr, rel, u, nil, nil, rel, ""))
				var keys []string
				for g := range groups {
					keys = append(keys, g)
				}
				sort.Strings(keys)
				for _, g := range keys {
					var drops []iniUnit
					for _, p := range groups[g] {
						du, derr := parseINI(tr[p])
						if derr != nil {
							problem("%s: %v", p, derr)
							continue
						}
						drops = append(drops, du)
					}
					profile := ""
					if m := profileRE.FindStringSubmatch(strings.TrimPrefix(g, unitDir+"/")); m != nil {
						profile = m[1]
					}
					d.Launchers = append(d.Launchers, quadletLauncher(tr, rel, u, drops, groups[g], rel+"+"+strings.TrimPrefix(g, unitDir+"/"), profile))
				}
			}
		}
	}

	for _, l := range d.Launchers {
		if l.Kind != "quadlet" {
			continue
		}
		for _, p := range paths {
			if path.Dir(p) == l.Dir && path.Ext(p) == ".container" {
				l.Names = append(l.Names, strings.TrimSuffix(path.Base(p), ".container"))
			}
		}
	}

	var dirs []string
	for dir := range composeDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		d.Launchers = append(d.Launchers, composeGroup(tr, dir, composeDirs[dir], composeDocs)...)
	}

	for _, rel := range paths {
		if claimedConfigs[rel] {
			continue
		}
		if sc, ok := supCfgs[rel]; ok {
			l := newLauncher(rel, "supervisor")
			l.Dir, l.Files, l.Service = path.Dir(rel), []string{rel}, path.Base(rel)
			supervisorLauncherFrom(l, sc, rel)
			d.Launchers = append(d.Launchers, l)
		}
	}

	for _, rel := range paths {
		base := path.Base(rel)
		if !(strings.HasPrefix(base, ".env") || strings.HasSuffix(base, ".env") || strings.HasSuffix(base, ".env.example")) {
			continue
		}
		vars := parseEnvLines(tr[rel])
		hasLocalAI := false
		for k := range vars {
			if strings.HasPrefix(k, "LOCALAI_") {
				hasLocalAI = true
			}
		}
		if !hasLocalAI {
			continue
		}
		l := newLauncher(rel, "envfile")
		l.Dir, l.Files, l.EnvOnly = path.Dir(rel), []string{rel}, true
		for k, v := range vars {
			l.set(k, &setting{Values: expand(v, nil), From: rel, FromFile: true})
		}
		d.Launchers = append(d.Launchers, l)
	}

	// Deployments ship from deploy/ only.
	seen := map[string]bool{}
	for _, l := range d.Launchers {
		for _, f := range l.Files {
			if !strings.HasPrefix(f, shipsFrom) && !seen[f] {
				seen[f] = true
				problem("%s launches LocalAI from outside %s: a second way of starting it that this suite does not know how to review; move it under deploy/ or out of the repository", f, shipsFrom)
			}
		}
	}
	return d
}

// splitVolume splits a volume spec on its colons, but not inside a ${...} substitution (whose ":-" and ":?" are not
// separators) and not after a Windows drive letter.
func splitVolume(spec string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(spec); i++ {
		switch spec[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ':':
			if depth == 0 {
				parts = append(parts, spec[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, spec[start:])
	if len(parts) > 1 && len(parts[0]) == 1 && ((parts[0][0] >= 'A' && parts[0][0] <= 'Z') || (parts[0][0] >= 'a' && parts[0][0] <= 'z')) {
		parts = append([]string{parts[0] + ":" + parts[1]}, parts[2:]...)
	}
	return parts
}
