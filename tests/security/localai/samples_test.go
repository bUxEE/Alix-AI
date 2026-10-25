package localaisec

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The planted-violation controls render one clean LocalAI deployment in every file form the suite parses (a compose
// file with the settings as a mapping, as a list and as command-line flags; a Quadlet unit with its env files; a
// systemd unit with the supervisor configuration it names; a bare env file) and then break one thing at a time. The
// detector must be silent on the clean sample and must name the right checklist row for every planted violation, in
// every form that can carry it.

// sample is one deployment, described abstractly.
type sample struct {
	env    map[string]string // LOCALAI_* and related settings, other than the key
	absent map[string]bool   // settings to leave out entirely
	flags  []string          // settings given as --flags on the command line instead of the environment (compose)

	// key is how the API key is delivered: required (a required substitution, or its equivalent in the other forms),
	// none, empty, literal:<v> or default:<v>.
	key string

	image      string
	ports      []string
	volumes    []string
	topVols    []string
	memLimit   string
	memSwap    string
	capDrop    []string
	privileged bool
	netMode    string
	service    string
	extra      []string // more service names, for the init rule
	profile    string   // lite, balanced, 4g or "": adds the profile override carrying profileMax
	profileMax string
	envAsList  bool
	galleryRO  bool // mount the backend catalogue read-only
	keepAddr   bool // keep LOCALAI_ADDRESS as written in the systemd form (a native address is normally rewritten to loopback)
}

const (
	cleanDigest   = "docker.io/localai/localai:v4.10.0@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b"
	cleanGallery  = `[{"name":"alixai-pinned","url":"file:///backends/alixai-backend-gallery.yaml"}]`
	catalogueFile = "deploy/x/config/backend-gallery.yaml"
	catalogueText = `- name: "cpu-llama-cpp"
  alias: "llama-cpp"
  uri: "quay.io/go-skynet/local-ai-backends@sha256:d7b0d9c69ecce93c2e7f7500ac3d99aba28648dab1ab29b698e0cc4538662579"
  mirrors:
    - "docker.io/localai/localai-backends@sha256:d7b0d9c69ecce93c2e7f7500ac3d99aba28648dab1ab29b698e0cc4538662579"
`
)

// cleanSample is a deployment every rule accepts, with the exceptions of the checklist table in force: the pinned local
// backend catalogue (EX-01) and its backend names (EX-02), no opaque errors (EX-03) and no metrics switch (EX-04).
func cleanSample() sample {
	return sample{
		env: map[string]string{
			"LOCALAI_SUBTLE_KEY_COMPARISON":      "true",
			"LOCALAI_AUTH":                       "false",
			"LOCALAI_ADDRESS":                    ":8080",
			"LOCALAI_CORS":                       "false",
			"LOCALAI_BACKEND_GALLERIES":          cleanGallery,
			"LOCALAI_EXTERNAL_BACKENDS":          "cpu-llama-cpp",
			"LOCALAI_DISABLE_WEBUI":              "true",
			"LOCALAI_DISABLE_RUNTIME_SETTINGS":   "true",
			"LOCALAI_DISABLE_GALLERY_ENDPOINT":   "true",
			"LOCALAI_GALLERIES":                  "[]",
			"LOCALAI_AUTOLOAD_GALLERIES":         "false",
			"LOCALAI_AUTOLOAD_BACKEND_GALLERIES": "false",
			"LOCALAI_DISABLE_MCP":                "true",
			"LOCALAI_DISABLE_AGENTS":             "true",
			"LOCALAI_P2P":                        "false",
			"LOCALAI_FEDERATED":                  "false",
			"LOCALAI_MAX_ACTIVE_BACKENDS":        "2",
			"LOCALAI_LOG_LEVEL":                  "info",
		},
		absent:    map[string]bool{},
		key:       "required",
		image:     cleanDigest,
		ports:     []string{"127.0.0.1:8080:8080"},
		volumes:   []string{"./data/models:/models", "./data/backends:/backends", "./data/state:/data", "./config:/configuration:ro"},
		memLimit:  "8g",
		memSwap:   "8g",
		capDrop:   []string{"ALL"},
		service:   "localai",
		galleryRO: true,
	}
}

func (s sample) clone() sample {
	c := s
	c.env = map[string]string{}
	for k, v := range s.env {
		c.env[k] = v
	}
	c.absent = map[string]bool{}
	for k, v := range s.absent {
		c.absent[k] = v
	}
	c.flags = append([]string(nil), s.flags...)
	c.ports = append([]string(nil), s.ports...)
	c.volumes = append([]string(nil), s.volumes...)
	c.topVols = append([]string(nil), s.topVols...)
	c.capDrop = append([]string(nil), s.capDrop...)
	c.extra = append([]string(nil), s.extra...)
	return c
}

// settings returns the environment of the sample, without the absent ones.
func (s sample) settings() map[string]string {
	out := map[string]string{}
	for k, v := range s.env {
		if !s.absent[k] {
			out[k] = v
		}
	}
	return out
}

func marshal(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func sortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// keyComposeValue renders the key setting of the sample in a compose environment, and whether it is there at all.
func (s sample) keyComposeValue() (string, bool) {
	switch {
	case s.key == "required":
		return "${LOCALAI_API_KEY:?set LOCALAI_API_KEY in .env}", true
	case s.key == "empty":
		return "", true
	case s.key == "none":
		return "", false
	case strings.HasPrefix(s.key, "literal:"):
		return strings.TrimPrefix(s.key, "literal:"), true
	case strings.HasPrefix(s.key, "default:"):
		return "${LOCALAI_API_KEY:-" + strings.TrimPrefix(s.key, "default:") + "}", true
	}
	panic("unknown key mode " + s.key)
}

// composeTree renders the sample as a compose project: the base file, its .env.example, the backend catalogue and, for
// a profile, the override file.
func composeTree(s sample) tree {
	env := s.settings()
	svc := map[string]any{
		"image":   s.image,
		"command": []any{"run"},
	}
	if s.privileged {
		svc["privileged"] = true
	}
	if s.netMode != "" {
		svc["network_mode"] = s.netMode
	}
	if len(s.ports) > 0 {
		var ports []any
		for _, p := range s.ports {
			ports = append(ports, p)
		}
		svc["ports"] = ports
	}
	if len(s.capDrop) > 0 {
		var cd []any
		for _, c := range s.capDrop {
			cd = append(cd, c)
		}
		svc["cap_drop"] = cd
	}
	vols := append([]string(nil), s.volumes...)
	if s.galleryRO {
		vols = append(vols, "./config/backend-gallery.yaml:/backends/alixai-backend-gallery.yaml:ro")
	} else {
		vols = append(vols, "./config/backend-gallery.yaml:/backends/alixai-backend-gallery.yaml")
	}
	var vl []any
	for _, v := range vols {
		vl = append(vl, v)
	}
	svc["volumes"] = vl
	if s.memLimit != "" {
		svc["mem_limit"] = s.memLimit
	}
	if s.memSwap != "" {
		svc["memswap_limit"] = s.memSwap
	}
	var flagList []any = []any{"run"}
	for _, f := range s.flags {
		flagList = append(flagList, f)
	}
	svc["command"] = flagList
	if kv, ok := s.keyComposeValue(); ok {
		env["LOCALAI_API_KEY"] = kv
	}
	if s.envAsList {
		var l []any
		for _, k := range sortedKeys(env) {
			l = append(l, k+"="+env[k])
		}
		svc["environment"] = l
	} else {
		m := map[string]any{}
		for k, v := range env {
			m[k] = v
		}
		svc["environment"] = m
	}
	services := map[string]any{s.service: svc}
	for _, n := range s.extra {
		services[n] = map[string]any{"image": "docker.io/library/busybox@sha256:" + strings.Repeat("a", 64)}
	}
	doc := map[string]any{"services": services}
	if len(s.topVols) > 0 {
		vs := map[string]any{}
		for _, v := range s.topVols {
			vs[v] = map[string]any{}
		}
		doc["volumes"] = vs
	}
	tr := tree{
		"deploy/x/compose.yaml": marshal(doc),
		"deploy/x/.env.example": "LOCALAI_API_KEY=change-me-generate-with-openssl\n",
		catalogueFile:           catalogueText,
	}
	if s.profile != "" {
		prof := map[string]any{"services": map[string]any{s.service: map[string]any{
			"mem_limit":     "8g",
			"memswap_limit": "8g",
			"environment":   map[string]any{"LOCALAI_MAX_ACTIVE_BACKENDS": s.profileMax},
		}}}
		tr["deploy/x/compose."+s.profile+".yaml"] = marshal(prof)
	}
	return tr
}

// quadletTree renders the sample as a Quadlet unit with the env files it reads, the form of the shipped Quadlet
// deployment: static settings in one env file, the key in another.
func quadletTree(s sample) tree {
	env := s.settings()
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=sample\n\n[Container]\n")
	fmt.Fprintf(&b, "ContainerName=%s\n", s.service)
	fmt.Fprintf(&b, "Image=%s\n", s.image)
	b.WriteString("User=10001\nGroup=10001\nReadOnly=true\nNoNewPrivileges=true\n")
	for _, c := range s.capDrop {
		fmt.Fprintf(&b, "DropCapability=%s\n", c)
	}
	if s.netMode != "" {
		fmt.Fprintf(&b, "Network=%s\n", s.netMode)
	} else {
		b.WriteString("Network=alixai-backend.network\n")
	}
	for _, p := range s.ports {
		fmt.Fprintf(&b, "PublishPort=%s\n", p)
	}
	args := ""
	if s.memLimit != "" {
		args += " --memory=" + s.memLimit
	}
	if s.memSwap != "" {
		args += " --memory-swap=" + s.memSwap
	}
	if s.privileged {
		args += " --privileged"
	}
	if args != "" {
		fmt.Fprintf(&b, "PodmanArgs=%s\n", strings.TrimSpace(args))
	}
	b.WriteString("Entrypoint=/bin/bash\nExec=/alixai/entrypoint.sh run" + strings.Join(append([]string{""}, s.flags...), " ") + "\n")
	b.WriteString("EnvironmentFile=%h/.config/alixai/localai.env\nEnvironmentFile=%h/.config/alixai/alixai.env\n")
	for _, v := range s.volumes {
		fmt.Fprintf(&b, "Volume=%s\n", quadletVolume(v))
	}
	if s.galleryRO {
		b.WriteString("Volume=%h/.config/alixai/localai/backend-gallery.yaml:/backends/alixai-backend-gallery.yaml:ro,Z\n")
	} else {
		b.WriteString("Volume=%h/.config/alixai/localai/backend-gallery.yaml:/backends/alixai-backend-gallery.yaml:Z\n")
	}
	b.WriteString("\n[Service]\nRestart=on-failure\n")
	var envFile strings.Builder
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&envFile, "%s=%s\n", k, env[k])
	}
	keyFile := "# the key\n"
	switch {
	case s.key == "required":
		keyFile += "LOCALAI_API_KEY=change-me-generate-with-openssl-rand-hex-32\n"
	case s.key == "none":
	case s.key == "empty" || strings.HasPrefix(s.key, "default:"):
		keyFile += "LOCALAI_API_KEY=\n"
	case strings.HasPrefix(s.key, "literal:"):
		keyFile += "LOCALAI_API_KEY=" + strings.TrimPrefix(s.key, "literal:") + "\n"
	}
	tr := tree{
		"deploy/q/" + s.service + ".container": b.String(),
		"deploy/q/localai.env.example":         envFile.String(),
		"deploy/q/alixai.env.example":          keyFile,
		"deploy/q/other.container":             "[Container]\nImage=alixai.build\nUser=65532\n",
		"deploy/q/config/backend-gallery.yaml": catalogueText,
	}
	for _, n := range s.extra {
		tr["deploy/q/"+n+".container"] = "[Container]\nImage=alixai.build\nUser=65532\n"
	}
	if s.profile != "" {
		tr["deploy/q/profiles/"+s.profile+"/"+s.service+".container.d/10-profile.conf"] =
			"[Container]\nPodmanArgs=--memory=8g --memory-swap=8g\nEnvironment=LOCALAI_MAX_ACTIVE_BACKENDS=" + s.profileMax + "\n"
	}
	return tr
}

func quadletVolume(v string) string {
	src, rest, ok := strings.Cut(v, ":")
	if !ok {
		return v
	}
	if strings.HasPrefix(src, "./") {
		src = "%h/.local/share/alixai/" + strings.TrimPrefix(src, "./")
	}
	return src + ":" + rest
}

// envFileTree renders only the env file.
func envFileTree(s sample) tree {
	var b strings.Builder
	env := s.settings()
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	switch {
	case s.key == "required":
		b.WriteString("LOCALAI_API_KEY=change-me-generate-with-openssl-rand-hex-32\n")
	case s.key == "empty" || strings.HasPrefix(s.key, "default:"):
		b.WriteString("LOCALAI_API_KEY=\n")
	case strings.HasPrefix(s.key, "literal:"):
		b.WriteString("LOCALAI_API_KEY=" + strings.TrimPrefix(s.key, "literal:") + "\n")
	}
	return tree{"deploy/e/localai.env.example": b.String(), catalogueFile: catalogueText}
}

// systemdTree renders the native form: a unit that runs the supervisor, and the supervisor configuration with the
// child's environment.
func systemdTree(s sample) tree {
	env := s.settings()
	var list []any
	for _, k := range sortedKeys(env) {
		list = append(list, k+"="+env[k])
	}
	exec := map[string]any{"path": "/opt/localai/local-ai", "args": []any{"run"}}
	cfg := map[string]any{"supervisor": map[string]any{"runtime": "exec", "exec": exec}}
	switch {
	case s.key == "required":
		exec["api_key_env"] = "LOCALAI_API_KEY"
		cfg["localai"] = map[string]any{"api_key_ref": "file:/run/credentials/localai.service/localai-api-key"}
	case strings.HasPrefix(s.key, "literal:"):
		list = append(list, "LOCALAI_API_KEY="+strings.TrimPrefix(s.key, "literal:"))
	case s.key == "empty" || strings.HasPrefix(s.key, "default:"):
		list = append(list, "LOCALAI_API_KEY=")
	}
	var flags []any
	flags = append(flags, "run")
	for _, f := range s.flags {
		flags = append(flags, f)
	}
	exec["args"] = flags
	exec["env"] = list
	unit := "[Unit]\nDescription=sample\n\n[Service]\nType=exec\nExecStart=/usr/local/bin/alixai supervisor exec -config /etc/alixai/localai.yaml\nDynamicUser=yes\n"
	if s.capDrop != nil {
		unit += "CapabilityBoundingSet=\n"
	}
	if s.memLimit != "" {
		unit += "MemoryMax=" + s.memLimit + "\n"
	}
	switch {
	case s.memSwap == "":
	case s.memSwap == s.memLimit:
		unit += "MemorySwapMax=0\n" // no swap, in systemd's words
	default:
		unit += "MemorySwapMax=" + s.memSwap + "\n"
	}
	name := s.service
	return tree{
		"deploy/n/" + name + ".service": unit,
		"deploy/n/localai.yaml":         marshal(cfg),
		catalogueFile:                   catalogueText,
	}
}

// exceptionsDoc renders the checklist's section 6 with the named rows; ids may be any of EX-01..EX-04.
func exceptionsDoc(expires string, ids ...string) string {
	rows := map[string][2]string{
		"EX-01": {"HC-14", "LOCALAI_BACKEND_GALLERIES names exactly one local catalogue and nothing else"},
		"EX-02": {"HC-18", "LOCALAI_EXTERNAL_BACKENDS is the install mechanism of the shipped deployment"},
		"EX-03": {"HC-20", "LOCALAI_OPAQUE_ERRORS stays off because the error mapping needs the status"},
		"EX-04": {"HC-21", "LOCALAI_DISABLE_METRICS_ENDPOINT is unset, metrics stay local telemetry"},
	}
	var b strings.Builder
	b.WriteString("## 5. Live verification\n\n| EX-99 | HC-99 | not in section six, never read | nobody | 2000-01-01 | none |\n\n")
	b.WriteString("## 6. Exceptions\n\n| ID | Row | Reason | Owner | Expires | Mitigation |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, id := range ids {
		r := rows[id]
		fmt.Fprintf(&b, "| %s | %s | %s | deployment (M0-18) | %s, or earlier | behind the key and loopback |\n", id, r[0], r[1], expires)
	}
	b.WriteString("\n## 7. Open points\n\n| EX-98 | HC-98 | not in section six either | nobody | 2000-01-01 | none |\n")
	return b.String()
}

var (
	allExceptions = []string{"EX-01", "EX-02", "EX-03", "EX-04"}
	fixedNow      = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
)

// checkTree discovers the launchers of a planted tree and judges them with the exceptions of doc, as of fixedNow.
// An empty discovery is itself a failure of the detector, never a pass.
func checkTree(tr tree, doc string) (findings []finding, launchers int, problems []string) {
	ex, exProblems := parseExceptions(doc)
	problems = append(problems, exProblems...)
	d := discover(tr)
	problems = append(problems, d.Problems...)
	c := checkCtx{ex: exceptions{byRow: ex, now: fixedNow}, tr: tr}
	for _, l := range d.Launchers {
		findings = append(findings, c.check(l)...)
	}
	return findings, len(d.Launchers), problems
}

func hasRule(fs []finding, rule string) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}
