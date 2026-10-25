package deploy

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestStackMatchesLocalAIDeployment keeps the stack from drifting away from the deployment M0-08 qualified and
// M0-13 tested: the same image digest, the same off switches, the same profile caps.
func TestStackMatchesLocalAIDeployment(t *testing.T) {
	m008 := loadCompose(t, localAIDir+"/compose.yaml").services["localai"]
	stack := loadCompose(t, stackDir+"/compose.yaml").services["localai"]
	if m008["image"] == nil || m008["image"] != stack["image"] {
		t.Errorf("the stack LocalAI image %v differs from deploy/container/localai/compose.yaml %v", stack["image"], m008["image"])
	}
	quadletImage := loadUnit(t, quadletDir+"/alixai-localai.container").get("Container", "Image")
	if quadletImage != m008["image"] {
		t.Errorf("the Quadlet LocalAI image %q differs from the M0-08 pin %v", quadletImage, m008["image"])
	}

	// Every static LocalAI setting of M0-08 is in the stack, and in the Quadlet env file, with the same value.
	m008Env, stackEnv := envMap(m008), envMap(stack)
	quadletEnv := parseEnvFile(readRepoFile(t, quadletDir+"/localai.env.example"))
	var drift []string
	for k, v := range m008Env {
		if !strings.HasPrefix(k, "LOCALAI_") || strings.Contains(v, "${") || k == "LOCALAI_API_KEY" {
			continue
		}
		if stackEnv[k] != v {
			drift = append(drift, fmt.Sprintf("compose %s=%q, M0-08 has %q", k, stackEnv[k], v))
		}
		if q, ok := quadletEnv[k]; !ok || strings.Trim(q, `'"`) != strings.Trim(v, `'"`) {
			drift = append(drift, fmt.Sprintf("quadlet %s=%q, M0-08 has %q", k, q, v))
		}
	}
	sort.Strings(drift)
	for _, d := range drift {
		t.Error(d)
	}
	if len(m008Env) < 20 {
		t.Fatalf("read only %d settings from the M0-08 compose file; the comparison is not seeing them", len(m008Env))
	}

	// Profile caps are the M0-08 ones.
	for _, profile := range []string{"lite", "balanced", "4g"} {
		want := loadCompose(t, fmt.Sprintf("%s/compose.%s.yaml", localAIDir, profile)).services["localai"]
		got := loadCompose(t, fmt.Sprintf("%s/compose.%s.yaml", stackDir, profile)).services["localai"]
		for _, key := range []string{"mem_limit", "memswap_limit", "cpus"} {
			if fmt.Sprint(got[key]) != fmt.Sprint(want[key]) || want[key] == nil {
				t.Errorf("profile %s: %s is %v, M0-08 has %v", profile, key, got[key], want[key])
			}
		}
		if fmt.Sprint(got["mem_limit"]) != fmt.Sprint(got["memswap_limit"]) {
			t.Errorf("profile %s: memswap_limit differs from mem_limit; the stack allows no swap (7.4)", profile)
		}
		for _, k := range []string{"LOCALAI_THREADS", "LOCALAI_MAX_ACTIVE_BACKENDS"} {
			if a, b := envMap(got)[k], envMap(want)[k]; a != b {
				t.Errorf("profile %s: %s is %q, M0-08 has %q", profile, k, a, b)
			}
		}
		// the Quadlet drop-in carries the same caps
		d := loadUnit(t, fmt.Sprintf("%s/profiles/%s/alixai-localai.container.d/10-profile.conf", quadletDir, profile))
		args := strings.Join(d.all("Container", "PodmanArgs"), " ")
		if !strings.Contains(args, "--memory="+fmt.Sprint(want["mem_limit"])) || !strings.Contains(args, "--memory-swap="+fmt.Sprint(want["memswap_limit"])) || !strings.Contains(args, "--cpus="+fmt.Sprint(want["cpus"])) {
			t.Errorf("Quadlet profile %s: PodmanArgs %q do not carry the M0-08 caps", profile, args)
		}
	}

	// The LocalAI configuration the stack mounts is the M0-08 one by default.
	vols := strings.Join(listStrings(stack["volumes"]), "\n")
	for _, want := range []string{"../localai/config", "../localai/entrypoint.sh:/alixai/entrypoint.sh:ro"} {
		if !strings.Contains(vols, want) {
			t.Errorf("the stack LocalAI volumes do not use %s:\n%s", want, vols)
		}
	}
}

// TestExternalOverrideIsOptIn: the default stack has no route out; compose.external.yaml and the Quadlet
// drop-in are the only place one is added, they add it for the AlixAI namespace alone, and the README says what
// that means.
func TestExternalOverrideIsOptIn(t *testing.T) {
	base := loadCompose(t, stackDir+"/compose.yaml")
	if _, ok := asMap(base.doc["networks"])["egress"]; ok {
		t.Error("the base compose file declares the egress network; it belongs to compose.external.yaml")
	}
	ext := loadCompose(t, stackDir+"/compose.external.yaml")
	if names := ext.names(); len(names) != 1 || names[0] != "anchor" {
		t.Errorf("compose.external.yaml touches services %v, want only anchor (the namespace AlixAI runs in)", names)
	}
	if nets := networkNames(ext.services["anchor"]); len(nets) != 1 || nets[0] != "egress" {
		t.Errorf("compose.external.yaml adds networks %v to the anchor, want only egress", nets)
	}
	egress, ok := asMap(ext.doc["networks"])["egress"]
	if !ok {
		t.Fatal("compose.external.yaml does not declare the egress network")
	}
	if internal, _ := asMap(egress)["internal"].(bool); internal {
		t.Error("the egress network is internal, so the override would open nothing")
	}
	if ps, _ := composePortProblems(ext); len(ps) != 0 || ext.services["anchor"]["ports"] != nil {
		t.Errorf("compose.external.yaml publishes ports: %v", ps)
	}

	// No file besides the opt-in ones mentions the egress network.
	for _, rel := range stackComposeFiles(t) {
		if rel == stackDir+"/compose.external.yaml" {
			continue
		}
		if strings.Contains(string(readRepoFile(t, rel)), "egress:") {
			t.Errorf("%s declares or uses an egress network; only compose.external.yaml may", rel)
		}
	}
	for _, u := range quadletUnits(t) {
		if strings.Contains(u.path, "egress") || strings.Contains(strings.Join(u.all("Pod", "Network"), " "), "egress") {
			t.Errorf("%s refers to the egress network; only quadlet/external/ may", u.path)
		}
	}

	// noexternal: one build argument, and an image variant that cannot be combined with External mode by accident.
	noext := loadCompose(t, stackDir+"/compose.noexternal.yaml")
	args := asMap(asMap(noext.services["alixai"]["build"])["args"])
	if args["GO_TAGS"] != "noexternal" {
		t.Errorf("compose.noexternal.yaml build args = %v, want GO_TAGS: noexternal", args)
	}

	// README: the opt-in states what it costs.
	readme := strings.ToLower(string(readRepoFile(t, stackDir+"/README.md")))
	for _, phrase := range []string{"external provider mode", "network-dependent", "provider terms", "compose.external.yaml", "noexternal"} {
		if !strings.Contains(readme, strings.ToLower(phrase)) {
			t.Errorf("the README does not mention %q", phrase)
		}
	}
}

// TestDockerfileHardening reads the Dockerfile as text: pinned bases, a static binary, a distroless non-root
// runtime, no shell, no VOLUME, the noexternal switch.
func TestDockerfileHardening(t *testing.T) {
	df := string(readRepoFile(t, dockerfile))
	if ps := dockerfileProblems(df); len(ps) != 0 {
		reportProblems(t, ps)
	}

	for name, edit := range map[string][2]string{
		"unpinned go image":    {pinnedDigest(t, df, "GO_IMAGE"), ""},
		"cgo on":               {"ENV CGO_ENABLED=0", "ENV CGO_ENABLED=1"},
		"no trimpath":          {"-trimpath ", ""},
		"root user":            {"USER 65532:65532\nWORKDIR", "USER 0\nWORKDIR"},
		"not distroless":       {"gcr.io/distroless/static-debian13:nonroot", "docker.io/library/alpine:3"},
		"volume":               {"WORKDIR /var/lib/alixai", "VOLUME /var/lib/alixai\nWORKDIR /var/lib/alixai"},
		"no go tags arg":       {"ARG GO_TAGS=\"\"", ""},
		"shell entrypoint":     {"ENTRYPOINT [\"/usr/local/bin/alixai\"]", "ENTRYPOINT [\"/bin/sh\", \"-c\", \"alixai\"]"},
		"curl pipe":            {"go mod verify", "go mod verify && curl -fsSL https://example.com/x | sh"},
		"insecure skip":        {"go mod verify", "go mod verify && GOINSECURE=* GOFLAGS=-insecure true"},
		"unpinned runtime":     {pinnedDigest(t, df, "RUNTIME_IMAGE"), ""},
		"default target moves": {"FROM ${RUNTIME_IMAGE} AS alixai", "FROM ${RUNTIME_IMAGE} AS netctl2"},
	} {
		if len(dockerfileProblems(strings.Replace(df, edit[0], edit[1], 1))) == 0 {
			t.Errorf("%s went unnoticed", name)
		}
	}
}

// pinnedDigest returns the "@sha256:..." pin of the Dockerfile's `ARG name=...` base image, so the mutations above
// keep testing the pin check when the image is bumped.
func pinnedDigest(t *testing.T, df, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^ARG ` + name + `=\S+(@sha256:[0-9a-f]{64})\s*$`).FindStringSubmatch(df)
	if m == nil {
		t.Fatalf("the Dockerfile has no digest-pinned ARG %s", name)
	}
	return m[1]
}

var (
	fromLine      = regexp.MustCompile(`(?m)^FROM\s+(\S+)(?:\s+AS\s+(\S+))?`)
	argImageLine  = regexp.MustCompile(`(?m)^ARG\s+(\w+_IMAGE)=(\S+)`)
	userLine      = regexp.MustCompile(`(?m)^USER\s+(\S+)`)
	entrypointRE  = regexp.MustCompile(`(?m)^ENTRYPOINT\s+(.*)$`)
	forbiddenDF   = regexp.MustCompile(`(?m)^(VOLUME|ADD\s+https?://)|curl\s[^\n]*\|\s*(ba)?sh|wget\s[^\n]*\|\s*(ba)?sh|GOINSECURE|GONOSUMDB|GONOSUMCHECK|GOFLAGS=[^\n]*-insecure|--no-check-certificate|--insecure`)
	imageWithPin  = regexp.MustCompile(`^[\w./:-]+@sha256:[0-9a-f]{64}$`)
	runtimeStatic = regexp.MustCompile(`^gcr\.io/distroless/static[\w-]*:nonroot@sha256:[0-9a-f]{64}$`)
)

func dockerfileProblems(df string) []string {
	var ps []string
	// both base images are ARG defaults pinned by digest
	args := map[string]string{}
	for _, m := range argImageLine.FindAllStringSubmatch(df, -1) {
		args[m[1]] = m[2]
	}
	for _, name := range []string{"GO_IMAGE", "RUNTIME_IMAGE"} {
		if v, ok := args[name]; !ok {
			ps = append(ps, "Dockerfile: ARG "+name+" is missing")
		} else if !imageWithPin.MatchString(v) {
			ps = append(ps, fmt.Sprintf("Dockerfile: %s=%s is not pinned by digest", name, v))
		}
	}
	if v := args["RUNTIME_IMAGE"]; v != "" && !runtimeStatic.MatchString(v) {
		ps = append(ps, fmt.Sprintf("Dockerfile: RUNTIME_IMAGE=%s is not distroless/static:nonroot", v))
	}
	// every FROM is an ARG reference (so the pin is in one place) or a stage of this file
	stages := map[string]bool{}
	var last string
	for _, m := range fromLine.FindAllStringSubmatch(df, -1) {
		if !strings.HasPrefix(m[1], "${") && !stages[m[1]] {
			ps = append(ps, fmt.Sprintf("Dockerfile: FROM %s is neither an ARG image nor an earlier stage", m[1]))
		}
		if m[2] != "" {
			stages[m[2]] = true
			last = m[2]
		}
	}
	if last != "alixai" {
		ps = append(ps, fmt.Sprintf("Dockerfile: the last stage is %q; the default build target must be alixai", last))
	}
	if !stages["netctl"] || !stages["build"] {
		ps = append(ps, "Dockerfile: stages build, netctl and alixai are required")
	}
	if !regexp.MustCompile(`(?m)^ENV\s+CGO_ENABLED=0(\s|$)`).MatchString(df) {
		ps = append(ps, "Dockerfile: ENV CGO_ENABLED=0 is required (static binary)")
	}
	builds := regexp.MustCompile(`go build [^\n]*`).FindAllString(df, -1)
	if len(builds) < 2 {
		ps = append(ps, "Dockerfile: expected a go build for alixai and one for alixai-netctl")
	}
	for _, b := range builds {
		if !strings.Contains(b, "-trimpath") {
			ps = append(ps, "Dockerfile: go build needs -trimpath: "+b)
		}
	}
	if !regexp.MustCompile(`(?m)^ARG GO_TAGS=""`).MatchString(df) || !strings.Contains(df, `-tags "$GO_TAGS"`) {
		ps = append(ps, "Dockerfile: the GO_TAGS build argument (the noexternal variant) is not wired into go build")
	}
	// every USER is a numeric non-root; the last stage has one
	users := userLine.FindAllStringSubmatch(df, -1)
	if len(users) < 2 {
		ps = append(ps, "Dockerfile: the runtime stages must each set USER")
	}
	for _, m := range users {
		if uidOf(m[1]) <= 0 {
			ps = append(ps, fmt.Sprintf("Dockerfile: USER %s is not a numeric non-root uid", m[1]))
		}
	}
	for _, m := range entrypointRE.FindAllStringSubmatch(df, -1) {
		if regexp.MustCompile(`\b(sh|bash)\b`).MatchString(m[1]) {
			ps = append(ps, "Dockerfile: ENTRYPOINT "+m[1]+" starts a shell")
		}
	}
	if m := forbiddenDF.FindString(df); m != "" {
		ps = append(ps, fmt.Sprintf("Dockerfile: %q is not allowed (anonymous volumes, piped installs, disabled verification)", strings.TrimSpace(m)))
	}
	if !strings.Contains(df, "go mod verify") {
		ps = append(ps, "Dockerfile: dependencies are not verified (go mod verify)")
	}
	sort.Strings(ps)
	return ps
}

// TestStackAlixAIConfig: the configuration the stack ships keeps AlixAI on loopback, reads the LocalAI key from
// the environment and carries no secret.
func TestStackAlixAIConfig(t *testing.T) {
	var cfg struct {
		DataDir string `yaml:"data_dir"`
		Server  struct {
			Listen string `yaml:"listen"`
		} `yaml:"server"`
		Localai struct {
			Endpoint  string   `yaml:"endpoint"`
			APIKeyRef string   `yaml:"api_key_ref"`
			Versions  []string `yaml:"qualified_versions"`
		} `yaml:"localai"`
		Gateway map[string]any `yaml:"gateway"`
		Dev     map[string]any `yaml:"dev"`
	}
	raw := readRepoFile(t, stackDir+"/config/alixai.yaml")
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Errorf("server.listen %q must be a literal loopback address (the relay carries the published port; a wider listener needs TLS)", cfg.Server.Listen)
	}
	u, err := url.Parse(cfg.Localai.Endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() != "8080" {
		t.Errorf("localai.endpoint %q must be http://127.0.0.1:8080, the address the anchor forwards to LocalAI", cfg.Localai.Endpoint)
	}
	if !strings.HasPrefix(cfg.Localai.APIKeyRef, "env:") && !strings.HasPrefix(cfg.Localai.APIKeyRef, "file:") {
		t.Errorf("localai.api_key_ref %q must be env: or file:", cfg.Localai.APIKeyRef)
	}
	if len(cfg.Localai.Versions) == 0 {
		t.Error("localai.qualified_versions is empty; a runtime version AlixAI was not qualified against must be refused (SEC-072)")
	}
	if cfg.DataDir != "/var/lib/alixai" {
		t.Errorf("data_dir %q must be the bind-mounted /var/lib/alixai", cfg.DataDir)
	}
	if cfg.Dev["spike_endpoint"] == true {
		t.Error("dev.spike_endpoint is on; the unauthenticated spike endpoint never ships")
	}
	if regexp.MustCompile(`(?im)^\s*(api_key|apikey|password|secret|token)\s*:\s*\S`).Match(raw) {
		t.Error("the shipped configuration holds a secret value")
	}
}
