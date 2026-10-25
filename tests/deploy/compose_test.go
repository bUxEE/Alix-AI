package deploy

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The four services of the stack and what each one is for. A fifth would have to be argued for here.
var stackServices = []string{"alixai", "anchor", "localai", "relay"}

// localAIOffFlags are the LocalAI settings that keep its administrative and tool surfaces off
// (docs/security/localai-hardening-checklist.md; SEC-068). The stack never differs from the M0-08 deployment
// here, and no override may set one of them to another value.
var localAIOffFlags = map[string]string{
	"LOCALAI_DISABLE_WEBUI":              "true",
	"LOCALAI_DISABLE_GALLERY_ENDPOINT":   "true",
	"LOCALAI_DISABLE_RUNTIME_SETTINGS":   "true",
	"LOCALAI_DISABLE_MCP":                "true",
	"LOCALAI_DISABLE_AGENTS":             "true",
	"LOCALAI_DISABLE_ASSISTANT":          "true",
	"LOCALAI_P2P":                        "false",
	"LOCALAI_FEDERATED":                  "false",
	"LOCALAI_DISTRIBUTED":                "false",
	"LOCALAI_CORS":                       "false",
	"LOCALAI_AUTH":                       "false",
	"LOCALAI_ENABLE_TRACING":             "false",
	"LOCALAI_AUTOLOAD_GALLERIES":         "false",
	"LOCALAI_AUTOLOAD_BACKEND_GALLERIES": "false",
	"LOCALAI_AUTO_UPGRADE_BACKENDS":      "false",
}

// envProblems judges the environment of one service or env file: no surface switched on, no verbose logging
// (HC-34: at debug and trace LocalAI logs every completion in full).
func envProblems(at string, env map[string]string) []string {
	var ps []string
	for flag, off := range localAIOffFlags {
		if v, ok := env[flag]; ok && !strings.Contains(v, "${") && !strings.EqualFold(v, off) {
			ps = append(ps, fmt.Sprintf("%s: %s=%q switches a surface back on (must stay %s)", at, flag, v, off))
		}
	}
	if v, ok := env["LOCALAI_LOG_LEVEL"]; ok && !strings.EqualFold(v, "info") && !strings.EqualFold(v, "warn") && !strings.EqualFold(v, "error") {
		ps = append(ps, fmt.Sprintf("%s: LOCALAI_LOG_LEVEL=%q puts model completions in the container log", at, v))
	}
	for _, k := range []string{"ALIXAI_ALLOW_VERBOSE_LOG", "LOCALAI_DEBUG", "DEBUG"} {
		if v, ok := env[k]; ok && v != "" && !strings.EqualFold(v, "false") && v != "0" {
			ps = append(ps, fmt.Sprintf("%s: %s=%q is a development switch", at, k, v))
		}
	}
	sort.Strings(ps)
	return ps
}

var imageDigest = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// ---- what no compose file may do, base or override ----

func isBadMount(source, target string) bool {
	for _, s := range []string{source, target} {
		l := strings.ToLower(s)
		if strings.Contains(l, "docker.sock") || strings.Contains(l, "podman.sock") || strings.Contains(l, "containerd.sock") {
			return true
		}
	}
	if source == "/" {
		return true
	}
	switch strings.TrimRight(source, "/") {
	case "/proc", "/sys", "/dev", "/etc", "/var/run", "/run", "/root", "/home":
		return true
	}
	return false
}

// weakeningProblems lists what a service must never do, in any file of the stack: an override cannot loosen
// the base by adding these.
func weakeningProblems(c compose) []string {
	var ps []string
	for _, name := range c.names() {
		svc := c.services[name]
		at := fmt.Sprintf("%s: service %s", c.path, name)
		if priv, _ := svc["privileged"].(bool); priv {
			ps = append(ps, at+": privileged")
		}
		if _, ok := svc["cap_add"]; ok {
			ps = append(ps, at+": cap_add (every service of the stack runs with no capability)")
		}
		if caps := listStrings(svc["cap_drop"]); svc["cap_drop"] != nil && !contains(caps, "ALL") {
			ps = append(ps, at+": cap_drop does not drop ALL")
		}
		for _, key := range []string{"network_mode", "pid", "ipc", "userns_mode", "uts"} {
			if v, _ := svc[key].(string); strings.TrimSpace(v) == "host" {
				ps = append(ps, fmt.Sprintf("%s: %s: host", at, key))
			}
		}
		if user, ok := svc["user"].(string); ok && uidOf(user) <= 0 {
			ps = append(ps, fmt.Sprintf("%s: user %q is not a numeric non-root uid", at, user))
		}
		if ro, ok := svc["read_only"].(bool); ok && !ro {
			ps = append(ps, at+": read_only: false")
		}
		for _, opt := range listStrings(svc["security_opt"]) {
			o := strings.ReplaceAll(strings.ToLower(opt), "=", ":")
			if o == "no-new-privileges:false" || strings.HasSuffix(o, ":unconfined") {
				ps = append(ps, fmt.Sprintf("%s: security_opt %q", at, opt))
			}
		}
		if len(asList(svc["devices"])) > 0 {
			ps = append(ps, at+": devices")
		}
		ps = append(ps, envProblems(at, envMap(svc))...)
		for _, e := range asList(svc["volumes"]) {
			spec, ok := e.(string)
			if !ok {
				ps = append(ps, at+": a volume in long syntax; the stack uses short-syntax bind mounts only")
				continue
			}
			source, target, _ := volumeParts(spec)
			switch {
			case !bindSource(source):
				ps = append(ps, fmt.Sprintf("%s: volume %q is not a bind mount (named and anonymous volumes are not allowed)", at, spec))
			case isBadMount(source, target):
				ps = append(ps, fmt.Sprintf("%s: volume %q mounts a host path or socket the stack must not touch", at, spec))
			}
		}
	}
	if _, ok := c.doc["volumes"]; ok {
		ps = append(ps, c.path+": a top-level volumes key declares named volumes")
	}
	sort.Strings(ps)
	return ps
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ---- what the base file must have ----

// baseHardeningProblems checks the required properties of the base compose file.
func baseHardeningProblems(c compose) []string {
	var ps []string
	for _, want := range stackServices {
		if _, ok := c.services[want]; !ok {
			ps = append(ps, fmt.Sprintf("%s: service %s is missing", c.path, want))
		}
	}
	for _, name := range c.names() {
		if !contains(stackServices, name) {
			ps = append(ps, fmt.Sprintf("%s: unexpected service %s", c.path, name))
		}
	}
	uids := map[string]int{}
	for _, name := range c.names() {
		svc := c.services[name]
		at := fmt.Sprintf("%s: service %s", c.path, name)

		user, _ := svc["user"].(string)
		uid := uidOf(user)
		if uid <= 0 {
			ps = append(ps, fmt.Sprintf("%s: user %q must be a numeric non-root uid:gid", at, user))
		} else if !strings.Contains(user, ":") {
			ps = append(ps, fmt.Sprintf("%s: user %q needs an explicit gid (uid:gid)", at, user))
		}
		uids[name] = uid

		if ro, _ := svc["read_only"].(bool); !ro {
			ps = append(ps, at+": read_only must be true (read-only root filesystem)")
		}
		if !contains(listStrings(svc["cap_drop"]), "ALL") {
			ps = append(ps, at+": cap_drop must contain ALL")
		}
		if !contains(listStrings(svc["security_opt"]), "no-new-privileges:true") {
			ps = append(ps, at+": security_opt must contain no-new-privileges:true")
		}
		if n, _ := svc["pids_limit"].(int); n <= 0 {
			ps = append(ps, at+": pids_limit must be set")
		}
		if hc := asMap(svc["healthcheck"]); len(hc) == 0 || hc["test"] == nil {
			ps = append(ps, at+": healthcheck is required (it gates the start order)")
		}
		if opts := asMap(asMap(svc["logging"])["options"]); opts["max-size"] == nil {
			ps = append(ps, at+": logging needs max-size (HC-35)")
		}
		if name != "localai" && svc["mem_limit"] == nil {
			ps = append(ps, at+": mem_limit must be set (localai is capped by the profile files)")
		}
		if v, _ := svc["restart"].(string); v != "unless-stopped" {
			ps = append(ps, fmt.Sprintf("%s: restart is %q, want unless-stopped", at, v))
		}
		for _, e := range asList(svc["volumes"]) {
			spec, _ := e.(string)
			source, target, opts := volumeParts(spec)
			needRO := strings.HasPrefix(target, "/etc/alixai/") || target == "/configuration" || target == "/models" ||
				target == "/alixai/entrypoint.sh" || strings.HasPrefix(target, "/backends/alixai-backend-gallery")
			if needRO && !hasOption(opts, "ro") {
				ps = append(ps, fmt.Sprintf("%s: volume %q must be read-only", at, spec))
			}
			if source != "" && strings.HasPrefix(target, "/var/lib/alixai") && hasOption(opts, "ro") {
				ps = append(ps, fmt.Sprintf("%s: volume %q is the data directory and must be writable", at, spec))
			}
		}
	}

	// Different OS identities (16.2): AlixAI, LocalAI and the forwarders never share a uid.
	seen := map[int]string{}
	for _, name := range []string{"alixai", "localai", "anchor"} {
		if uid := uids[name]; uid > 0 {
			if other, dup := seen[uid]; dup {
				ps = append(ps, fmt.Sprintf("%s: services %s and %s run as the same uid %d", c.path, other, name, uid))
			}
			seen[uid] = name
		}
	}
	if uids["relay"] > 0 && (uids["relay"] == uids["alixai"] || uids["relay"] == uids["localai"]) {
		ps = append(ps, c.path+": relay shares a uid with alixai or localai")
	}

	// Networks: LocalAI and the anchor live on internal networks only; only the relay is on the one that routes out.
	nets := asMap(c.doc["networks"])
	internal := map[string]bool{}
	for n, v := range nets {
		if b, _ := asMap(v)["internal"].(bool); b {
			internal[n] = true
		}
	}
	for _, n := range []string{"backend", "front"} {
		if !internal[n] {
			ps = append(ps, fmt.Sprintf("%s: network %s must be internal: true", c.path, n))
		}
	}
	for name, svc := range c.services {
		for _, n := range networkNames(svc) {
			if !internal[n] && name != "relay" {
				ps = append(ps, fmt.Sprintf("%s: service %s is on %s, which is not internal; only the relay may be", c.path, name, n))
			}
		}
	}
	if svc, ok := c.services["localai"]; ok {
		if svc["ports"] != nil {
			ps = append(ps, c.path+": localai must not publish a port")
		}
		env := envMap(svc)
		for flag, off := range localAIOffFlags {
			if env[flag] != off {
				ps = append(ps, fmt.Sprintf("%s: localai must set %s=%s", c.path, flag, off))
			}
		}
		if env["ALIXAI_MODELS_READONLY"] != "true" {
			ps = append(ps, c.path+": localai must run with ALIXAI_MODELS_READONLY=true and the models mounted :ro (HC-24)")
		}
		if !strings.Contains(env["LOCALAI_API_KEY"], ":?") {
			ps = append(ps, c.path+": LOCALAI_API_KEY must be required (${LOCALAI_API_KEY:?...})")
		}
	}
	if svc, ok := c.services["alixai"]; ok {
		if mode, _ := svc["network_mode"].(string); mode != "service:anchor" {
			ps = append(ps, fmt.Sprintf("%s: alixai network_mode is %q, want service:anchor (loopback to LocalAI, no route out)", c.path, mode))
		}
		if svc["ports"] != nil || svc["networks"] != nil {
			ps = append(ps, c.path+": alixai lives in the anchor's network namespace and sets no ports or networks")
		}
	}
	sort.Strings(ps)
	return ps
}

// networkNames returns the networks of a service in list or mapping form.
func networkNames(svc map[string]any) []string {
	switch v := svc["networks"].(type) {
	case []any:
		return listStrings(v)
	case map[string]any:
		out := make([]string, 0, len(v))
		for n := range v {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

// SEC-066 (LocalAI reachable only by AlixAI: no ports, internal network, API key required), SEC-068 (unsafe LocalAI
// settings), SEC-080 (separate non-root identities, cap_drop ALL, no-new-privileges, read-only root) and SEC-064
// (images pinned by digest) for the Compose files.
func TestComposeHardening(t *testing.T) {
	files := stackComposeFiles(t)
	var base compose
	for _, rel := range files {
		c := loadCompose(t, rel)
		reportProblems(t, weakeningProblems(c))
		if rel == stackDir+"/compose.yaml" {
			base = c
		}
	}
	if base.path == "" {
		t.Fatal("compose.yaml is not among the stack files")
	}
	reportProblems(t, baseHardeningProblems(base))

	// Every stack image is pinned by digest, or built here (no image key at all).
	for _, name := range base.names() {
		img, ok := base.services[name]["image"].(string)
		build := base.services[name]["build"] != nil
		switch {
		case ok && build:
			t.Errorf("service %s has both image and build; a built image has no digest to pin", name)
		case ok && !imageDigest.MatchString(img):
			t.Errorf("service %s: image %q is not pinned by digest", name, img)
		case !ok && !build:
			t.Errorf("service %s has neither a pinned image nor a build", name)
		}
	}

	// The overrides cannot undo it: each profile file may set limits, labels and profile variables only.
	for _, rel := range files {
		if rel == stackDir+"/compose.yaml" {
			continue
		}
		c := loadCompose(t, rel)
		for _, name := range c.names() {
			for key := range c.services[name] {
				if !overrideKeyAllowed[rel[strings.LastIndex(rel, "/")+1:]][key] {
					t.Errorf("%s: service %s sets %q, which this override file may not touch", rel, name, key)
				}
			}
		}
	}
}

// overrideKeyAllowed is what each override may set on a service. compose.external.yaml adds a network and
// nothing else; compose.noexternal.yaml changes a build argument; a profile sets limits.
var overrideKeyAllowed = map[string]map[string]bool{
	"compose.lite.yaml":       {"mem_limit": true, "memswap_limit": true, "cpus": true, "environment": true, "labels": true},
	"compose.balanced.yaml":   {"mem_limit": true, "memswap_limit": true, "cpus": true, "environment": true, "labels": true},
	"compose.4g.yaml":         {"mem_limit": true, "memswap_limit": true, "cpus": true, "environment": true, "labels": true},
	"compose.external.yaml":   {"networks": true},
	"compose.noexternal.yaml": {"build": true, "labels": true},
}

func TestComposeHardeningDetectors(t *testing.T) {
	good := `services:
  localai:
    image: x@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b
    user: "10001:10001"
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    volumes:
      - ./data/a:/data
      - ${VAR:-../x}/y:/y:ro
`
	c, err := parseCompose("good.yaml", []byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if ps := weakeningProblems(c); len(ps) != 0 {
		t.Fatalf("a clean service was flagged: %v", ps)
	}
	for name, doc := range map[string]string{
		"privileged":       "services:\n  a:\n    privileged: true\n",
		"cap_add":          "services:\n  a:\n    cap_add: [CHOWN]\n",
		"cap_drop partial": "services:\n  a:\n    cap_drop: [NET_RAW]\n",
		"host network":     "services:\n  a:\n    network_mode: host\n",
		"host pid":         "services:\n  a:\n    pid: host\n",
		"root user":        "services:\n  a:\n    user: \"0:0\"\n",
		"named user":       "services:\n  a:\n    user: nobody\n",
		"writable rootfs":  "services:\n  a:\n    read_only: false\n",
		"unconfined":       "services:\n  a:\n    security_opt: [\"seccomp:unconfined\"]\n",
		"devices":          "services:\n  a:\n    devices: [\"/dev/kvm\"]\n",
		"docker socket":    "services:\n  a:\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n",
		"named volume":     "services:\n  a:\n    volumes:\n      - models:/models\n",
		"anonymous volume": "services:\n  a:\n    volumes:\n      - /models\n",
		"host etc":         "services:\n  a:\n    volumes:\n      - /etc:/hostetc:ro\n",
		"top-level volume": "services:\n  a:\n    image: x\nvolumes:\n  models: {}\n",
		"long syntax":      "services:\n  a:\n    volumes:\n      - {type: bind, source: ./x, target: /x}\n",
	} {
		c, err := parseCompose(name, []byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(weakeningProblems(c)) == 0 {
			t.Errorf("%s went unnoticed", name)
		}
	}

	// The base file rules: an empty service has everything wrong with it.
	empty, _ := parseCompose("empty.yaml", []byte("services:\n  alixai: {}\n  localai: {}\n  anchor: {}\n  relay: {}\n"))
	if ps := baseHardeningProblems(empty); len(ps) < 20 {
		t.Errorf("an empty base file produced only %d problems: %v", len(ps), ps)
	}
	extra, _ := parseCompose("extra.yaml", []byte("services:\n  sidecar: {}\n"))
	if ps := baseHardeningProblems(extra); !anyContains(ps, "unexpected service sidecar") {
		t.Errorf("an unexpected service went unnoticed: %v", ps)
	}
}

func anyContains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---- no init containers ----

var initName = regexp.MustCompile(`(^|[-_.])(init|setup|bootstrap|migrate|migration|seed|provision|prepare|preload)($|[-_.])`)

// initProblems lists what makes a compose service a one-shot init container, or lets a start order skip a
// health check: the user's rule is that setup lives in the main service or behind service_healthy.
func initProblems(c compose) []string {
	var ps []string
	for _, name := range c.names() {
		svc := c.services[name]
		at := fmt.Sprintf("%s: service %s", c.path, name)
		if initName.MatchString(name) {
			ps = append(ps, at+": the name says it is an init or setup container")
		}
		if cn, _ := svc["container_name"].(string); initName.MatchString(cn) {
			ps = append(ps, at+": container_name says it is an init container")
		}
		switch v := svc["restart"].(type) {
		case string:
			if v == "no" || v == "" {
				ps = append(ps, at+": restart is no, which is how a one-shot container is declared")
			}
		case bool:
			if !v { // YAML 1.1 reads an unquoted no as false
				ps = append(ps, at+": restart is no, which is how a one-shot container is declared")
			}
		case nil:
			ps = append(ps, at+": restart is not set, so it is not declared a long-running service")
		}
		if init, _ := svc["init"].(bool); init {
			ps = append(ps, at+": init: true is not an init container but is not used by this stack either")
		}
		deps := svc["depends_on"]
		switch d := deps.(type) {
		case []any:
			if len(d) > 0 {
				ps = append(ps, at+": depends_on in list form means service_started; use the mapping with condition: service_healthy")
			}
		case map[string]any:
			for dep, v := range d {
				cond, _ := asMap(v)["condition"].(string)
				if cond != "service_healthy" {
					ps = append(ps, fmt.Sprintf("%s: depends_on %s has condition %q, want service_healthy", at, dep, cond))
				}
				target, ok := c.services[dep]
				if !ok {
					ps = append(ps, fmt.Sprintf("%s: depends_on names unknown service %s", at, dep))
					continue
				}
				if asMap(target["healthcheck"])["test"] == nil {
					ps = append(ps, fmt.Sprintf("%s: depends_on %s, which has no healthcheck to wait for", at, dep))
				}
			}
		}
	}
	sort.Strings(ps)
	return ps
}

func TestNoInitContainers(t *testing.T) {
	// compose
	base := loadCompose(t, stackDir+"/compose.yaml")
	for _, rel := range stackComposeFiles(t) {
		c := loadCompose(t, rel)
		if rel != base.path && len(c.services) > 0 {
			// An override sets no depends_on, restart or service of its own.
			for _, name := range c.names() {
				if _, ok := base.services[name]; !ok {
					t.Errorf("%s adds service %s; the stack has no service besides the four of compose.yaml", rel, name)
				}
				for _, key := range []string{"depends_on", "restart", "entrypoint", "command", "init"} {
					if c.services[name][key] != nil {
						t.Errorf("%s: service %s sets %s; start order and processes belong to compose.yaml", rel, name, key)
					}
				}
			}
			continue
		}
		reportProblems(t, initProblems(c))
	}
	// the chain is the one the design states: localai -> anchor -> alixai -> relay, every link service_healthy
	for dep, on := range map[string]string{"anchor": "localai", "alixai": "anchor", "relay": "alixai"} {
		cond, _ := asMap(asMap(base.services[dep]["depends_on"])[on])["condition"].(string)
		if cond != "service_healthy" {
			t.Errorf("service %s must depend on %s with service_healthy, has %q", dep, on, cond)
		}
	}

	// Quadlet
	units := quadletUnits(t)
	reportProblems(t, quadletInitProblems(units))

	// Dockerfile: the image runs the service, not a setup script
	df := string(readRepoFile(t, dockerfile))
	if regexp.MustCompile(`(?m)^ENTRYPOINT .*(sh|bash)\b`).MatchString(df) {
		t.Error("the Dockerfile entrypoint starts a shell; the distroless image has none and setup belongs in the service")
	}

	// detector
	for name, doc := range map[string]string{
		"init service name":        "services:\n  db-init:\n    restart: unless-stopped\n",
		"setup name":               "services:\n  setup:\n    restart: unless-stopped\n",
		"restart no":               "services:\n  a:\n    image: x\n    restart: \"no\"\n",
		"restart no unquoted":      "services:\n  a:\n    image: x\n    restart: no\n",
		"no restart":               "services:\n  a:\n    image: x\n",
		"completed successfully":   "services:\n  a:\n    restart: unless-stopped\n    depends_on:\n      b:\n        condition: service_completed_successfully\n  b:\n    restart: unless-stopped\n",
		"service started":          "services:\n  a:\n    restart: unless-stopped\n    depends_on:\n      b:\n        condition: service_started\n  b:\n    restart: unless-stopped\n",
		"list depends_on":          "services:\n  a:\n    restart: unless-stopped\n    depends_on: [b]\n  b:\n    restart: unless-stopped\n",
		"healthy but no check":     "services:\n  a:\n    restart: unless-stopped\n    depends_on:\n      b:\n        condition: service_healthy\n  b:\n    restart: unless-stopped\n",
		"container_name migration": "services:\n  a:\n    restart: unless-stopped\n    container_name: app-migrate\n",
	} {
		c, err := parseCompose("inline.yaml", []byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(initProblems(c)) == 0 {
			t.Errorf("%s went unnoticed", name)
		}
	}
	ok, _ := parseCompose("inline.yaml", []byte("services:\n  a:\n    image: x\n    restart: unless-stopped\n    depends_on:\n      b:\n        condition: service_healthy\n  b:\n    image: y\n    restart: unless-stopped\n    healthcheck:\n      test: [CMD, true]\n"))
	if ps := initProblems(ok); len(ps) != 0 {
		t.Errorf("a healthcheck-gated chain was flagged: %v", ps)
	}
}

// ---- ports ----

// shortPortProblem judges a short-syntax port mapping: [HOST_IP:]HOST:CONTAINER[/PROTOCOL]. The host address
// must be a literal loopback IP; a missing, variable or non-loopback host address publishes on the network.
func shortPortProblem(s string) string {
	spec := strings.TrimSpace(s)
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		spec = spec[:i]
	}
	var host string
	if strings.HasPrefix(spec, "[") {
		end := strings.Index(spec, "]")
		if end < 0 || !strings.HasPrefix(spec[end+1:], ":") {
			return fmt.Sprintf("port %q is malformed", s)
		}
		host = spec[1:end]
	} else {
		i := strings.Index(spec, ":")
		if i < 0 {
			return fmt.Sprintf("port %q has no host address, so it binds every interface", s)
		}
		host = spec[:i]
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Sprintf("port %q does not bind a literal loopback address", s)
	}
	return ""
}

func composePortProblems(c compose) (problems []string, published map[string]int) {
	published = map[string]int{}
	for _, name := range c.names() {
		svc := c.services[name]
		if svc["ports"] == nil {
			continue
		}
		ports := asList(svc["ports"])
		if ports == nil {
			problems = append(problems, fmt.Sprintf("%s: service %s: ports is not a list", c.path, name))
			continue
		}
		for _, p := range ports {
			published[name]++
			s, ok := p.(string)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: service %s: port %v is not a short-syntax string with a host address", c.path, name, p))
				continue
			}
			if msg := shortPortProblem(s); msg != "" {
				problems = append(problems, fmt.Sprintf("%s: service %s: %s", c.path, name, msg))
			}
		}
	}
	return problems, published
}

// SEC-066 (LocalAI is never published) and SEC-093 (every published port is 127.0.0.1) for Compose and Quadlet.
func TestPortsLoopbackOnly(t *testing.T) {
	// compose: every published port of every stack file is a loopback one, and only the relay publishes
	totalCompose := 0
	for _, rel := range stackComposeFiles(t) {
		c := loadCompose(t, rel)
		ps, published := composePortProblems(c)
		reportProblems(t, ps)
		for name, n := range published {
			totalCompose += n
			if name != "relay" {
				t.Errorf("%s: service %s publishes a port; only the relay does (LocalAI and AlixAI are never published)", rel, name)
			}
		}
		for _, name := range c.names() {
			if c.services[name]["expose"] != nil && name != "localai" {
				t.Errorf("%s: service %s declares expose; nothing but LocalAI needs to name a port", rel, name)
			}
		}
	}
	if totalCompose == 0 {
		t.Fatal("no stack compose file publishes a port; the scan is not seeing the relay")
	}

	// the relay publishes on 127.0.0.1 with a variable host PORT only
	relay := loadCompose(t, stackDir+"/compose.yaml").services["relay"]
	ports := listStrings(relay["ports"])
	if len(ports) != 1 || !strings.HasPrefix(ports[0], "127.0.0.1:") {
		t.Errorf("relay ports = %v, want exactly one starting with the literal 127.0.0.1", ports)
	}

	// Quadlet: PublishPort only in the relay unit, loopback only; the pod publishes nothing
	totalQuadlet := 0
	for _, u := range quadletUnits(t) {
		for _, v := range u.all("Container", "PublishPort") {
			totalQuadlet++
			if msg := shortPortProblem(v); msg != "" {
				t.Errorf("%s: %s", u.path, msg)
			}
			if u.unitName() != "alixai-relay.container" {
				t.Errorf("%s publishes %s; only alixai-relay.container does", u.path, v)
			}
		}
		if u.has("Pod", "PublishPort") {
			t.Errorf("%s: the pod publishes a port; it is the network namespace AlixAI runs in and must stay unreachable from outside", u.path)
		}
		for _, p := range u.all("Container", "PodmanArgs") {
			if regexp.MustCompile(`(^|\s)(-p|--publish|-P|--publish-all)\b`).MatchString(p) {
				t.Errorf("%s: PodmanArgs publishes a port behind the unit's back: %s", u.path, p)
			}
		}
	}
	if totalQuadlet == 0 {
		t.Fatal("no Quadlet unit publishes a port; the scan is not seeing the relay")
	}

	// detector
	for _, bad := range []string{"7470:7470", "7470", "0.0.0.0:7470:7470", "[::]:7470:7470", "192.168.1.5:7470:7470", "${BIND:-127.0.0.1}:7470:7470", "localhost:7470:7470"} {
		if shortPortProblem(bad) == "" {
			t.Errorf("port %q went unnoticed", bad)
		}
	}
	for _, ok := range []string{"127.0.0.1:7470:7470", "127.0.0.1:${P:-7470}:7470", "[::1]:7470:7470", "127.0.0.1:7470:7470/tcp"} {
		if msg := shortPortProblem(ok); msg != "" {
			t.Errorf("port %q was flagged: %s", ok, msg)
		}
	}
	long, _ := parseCompose("inline.yaml", []byte("services:\n  a:\n    ports:\n      - {target: 1, published: 1, host_ip: 127.0.0.1}\n"))
	if ps, _ := composePortProblems(long); len(ps) == 0 {
		t.Error("a long-syntax port went unnoticed; the stack requires the short syntax with a literal host address")
	}
}
