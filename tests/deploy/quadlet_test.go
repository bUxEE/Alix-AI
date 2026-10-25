package deploy

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// quadletUnits returns every top-level Quadlet file of the deployment (containers, the pod, networks and
// builds), and fails when there are fewer than the nine the design has.
func quadletUnits(t testing.TB) []unit {
	t.Helper()
	var units []unit
	for _, pattern := range []string{"*.container", "*.pod", "*.network", "*.build"} {
		for _, rel := range repoGlob(t, quadletDir, pattern) {
			units = append(units, loadUnit(t, rel))
		}
	}
	if len(units) < 9 {
		t.Fatalf("found %d Quadlet units under %s, want at least 9 (4 containers, a pod, 3 networks, builds)", len(units), quadletDir)
	}
	return units
}

// quadletDropIns returns every opt-in and profile file below the Quadlet directory.
func quadletDropIns(t testing.TB) []unit {
	t.Helper()
	var out []unit
	for _, pattern := range []string{"external/*/*.conf", "external/*.network", "noexternal/*/*.conf", "profiles/*/*/*.conf"} {
		for _, rel := range repoGlob(t, quadletDir, pattern) {
			out = append(out, loadUnit(t, rel))
		}
	}
	if len(out) < 8 {
		t.Fatalf("found %d Quadlet drop-ins and opt-in files, want at least 8 (external 2, noexternal 1, three profiles of 2)", len(out))
	}
	return out
}

func pathUnder(u unit, dir string) bool { return strings.Contains(u.path, "/"+dir+"/") }

var forbiddenPodmanArgs = regexp.MustCompile(`(^|\s)--?(privileged|cap-add|device|network[= ]+host|net[= ]+host|pid[= ]+host|ipc[= ]+host|userns[= ]+host|uts[= ]+host|security-opt[= ]+(seccomp|apparmor|label)[=:]?(unconfined|disable)|volume|v|mount|publish|p|publish-all|P|user|u|read-only[= ]+false|cap-drop)\b`)

func quadletImagePinned(image string) bool {
	return imageDigest.MatchString(image) || strings.HasSuffix(image, ".build")
}

// quadletContainerProblems lists what a Quadlet container unit lacks or must not have.
func quadletContainerProblems(u unit) []string {
	var ps []string
	at := u.unitName()
	problem := func(format string, a ...any) { ps = append(ps, at+": "+fmt.Sprintf(format, a...)) }

	if !u.has("Container", "Image") {
		problem("no Image")
	} else if img := u.get("Container", "Image"); !quadletImagePinned(img) {
		problem("Image %q is neither pinned by digest nor a .build unit", img)
	}
	user := u.get("Container", "User")
	if n, err := strconv.Atoi(user); err != nil || n <= 0 {
		problem("User %q must be a numeric non-root uid", user)
	}
	if g := u.get("Container", "Group"); g == "" {
		problem("Group is not set")
	} else if n, err := strconv.Atoi(g); err != nil || n <= 0 {
		problem("Group %q must be numeric and non-root", g)
	}
	if !strings.EqualFold(u.get("Container", "ReadOnly"), "true") {
		problem("ReadOnly must be true (read-only root filesystem)")
	}
	if drops := u.all("Container", "DropCapability"); len(drops) != 1 || !strings.EqualFold(drops[0], "all") {
		problem("DropCapability must be exactly ALL, got %v", drops)
	}
	if u.has("Container", "AddCapability") {
		problem("AddCapability: every unit of the deployment runs with no capability")
	}
	if !strings.EqualFold(u.get("Container", "NoNewPrivileges"), "true") {
		problem("NoNewPrivileges must be true")
	}
	if n, err := strconv.Atoi(u.get("Container", "PidsLimit")); err != nil || n <= 0 {
		problem("PidsLimit must be set")
	}
	if !u.has("Container", "HealthCmd") {
		problem("HealthCmd is required (it gates the start order)")
	}
	if u.get("Container", "Notify") != "healthy" {
		problem("Notify must be healthy, so systemd orders the next unit after the health check")
	}
	for _, key := range []string{"AddDevice", "SecurityLabelDisable", "Unmask", "Sysctl", "SeccompProfile", "AppArmor"} {
		if u.has("Container", key) {
			problem("%s is not allowed", key)
		}
	}
	for _, key := range []string{"Network", "UserNS"} {
		for _, v := range u.all("Container", key) {
			if strings.EqualFold(v, "host") {
				problem("%s=host", key)
			}
		}
	}
	for _, v := range u.all("Container", "UserNS") {
		if strings.Contains(v, "keep-id") {
			problem("UserNS=%s maps one host uid into every container, which defeats the separate identities; use the default", v)
		}
	}
	for _, a := range u.all("Container", "PodmanArgs") {
		if m := forbiddenPodmanArgs.FindString(a); m != "" {
			problem("PodmanArgs %q uses %q, which changes what this file states", a, strings.TrimSpace(m))
		}
	}
	for _, v := range u.all("Container", "Volume") {
		parts := strings.Split(v, ":")
		if len(parts) < 2 {
			problem("Volume %q is an anonymous volume", v)
			continue
		}
		source, target := parts[0], parts[1]
		var opts []string
		if len(parts) > 2 {
			opts = strings.Split(parts[2], ",")
		}
		if !(strings.HasPrefix(source, "/") || strings.HasPrefix(source, "%h") || strings.HasPrefix(source, "~") || strings.HasPrefix(source, ".")) {
			problem("Volume %q names a volume, not a host path (named volumes are not allowed)", v)
		}
		if isBadMount(source, target) {
			problem("Volume %q mounts a host path or socket the deployment must not touch", v)
		}
		if (target == "/models" || target == "/configuration" || strings.HasPrefix(target, "/etc/alixai/") || target == "/alixai/entrypoint.sh") && !hasOption(opts, "ro") {
			problem("Volume %q must be read-only", v)
		}
	}
	if r := u.get("Service", "Restart"); r != "on-failure" && r != "always" {
		problem("[Service] Restart is %q, want on-failure or always", r)
	}
	return ps
}

// SEC-066 (LocalAI on an internal network, unpublished), SEC-068 (LocalAI settings in the env file), SEC-080 (separate
// non-root identities, no capability, read-only root) and SEC-064 (images pinned by digest or built here) for Quadlet.
func TestQuadletHardening(t *testing.T) {
	units := quadletUnits(t)
	containers := map[string]unit{}
	for _, u := range units {
		if strings.HasSuffix(u.path, ".container") {
			containers[strings.TrimSuffix(u.unitName(), ".container")] = u
			reportProblems(t, quadletContainerProblems(u))
		}
	}
	for _, want := range []string{"alixai", "alixai-anchor", "alixai-localai", "alixai-relay"} {
		if _, ok := containers[want]; !ok {
			t.Fatalf("container unit %s.container is missing", want)
		}
	}

	// Different OS identities (16.2).
	seen := map[string]string{}
	for name, u := range containers {
		uid := u.get("Container", "User")
		if name == "alixai-relay" || name == "alixai-anchor" {
			continue // the two forwarders may share the nobody identity; AlixAI and LocalAI may not
		}
		if other, dup := seen[uid]; dup {
			t.Errorf("%s and %s run as the same uid %s", other, name, uid)
		}
		seen[uid] = name
	}
	for _, name := range []string{"alixai-relay", "alixai-anchor"} {
		uid := containers[name].get("Container", "User")
		if uid == containers["alixai"].get("Container", "User") || uid == containers["alixai-localai"].get("Container", "User") {
			t.Errorf("%s shares a uid with alixai or alixai-localai", name)
		}
	}

	// Networks: LocalAI on the internal backend network only, with the alias the anchor dials; AlixAI and the
	// anchor in the pod; the relay on front and edge.
	localai := containers["alixai-localai"]
	if nets := localai.all("Container", "Network"); len(nets) != 1 || nets[0] != "alixai-backend.network" {
		t.Errorf("alixai-localai networks = %v, want only alixai-backend.network", nets)
	}
	if localai.get("Container", "NetworkAlias") != "localai" {
		t.Error("alixai-localai needs NetworkAlias=localai, the name the anchor forwards to")
	}
	if localai.has("Container", "PublishPort") {
		t.Error("alixai-localai publishes a port")
	}
	for _, name := range []string{"alixai", "alixai-anchor"} {
		u := containers[name]
		if u.get("Container", "Pod") != "alixai.pod" {
			t.Errorf("%s must run in alixai.pod, the network namespace without a route out", name)
		}
		if u.has("Container", "Network") {
			t.Errorf("%s sets Network next to Pod; the pod owns the network", name)
		}
	}
	relayNets := containers["alixai-relay"].all("Container", "Network")
	sort.Strings(relayNets)
	if strings.Join(relayNets, ",") != "alixai-edge.network,alixai-front.network" {
		t.Errorf("alixai-relay networks = %v, want alixai-edge.network and alixai-front.network", relayNets)
	}

	byName := map[string]unit{}
	for _, u := range units {
		byName[u.unitName()] = u
	}
	for _, n := range []string{"alixai-backend.network", "alixai-front.network"} {
		if !strings.EqualFold(byName[n].get("Network", "Internal"), "true") {
			t.Errorf("%s must be Internal=true: no route out, no host access", n)
		}
	}
	if strings.EqualFold(byName["alixai-edge.network"].get("Network", "Internal"), "true") {
		t.Error("alixai-edge.network is internal; a published port needs a network that is not")
	}
	podNets := byName["alixai.pod"].all("Pod", "Network")
	sort.Strings(podNets)
	if strings.Join(podNets, ",") != "alixai-backend.network,alixai-front.network" {
		t.Errorf("alixai.pod networks = %v, want only the two internal ones", podNets)
	}

	// The static LocalAI environment file: every off switch, read-only models, no secret.
	env := parseEnvFile(readRepoFile(t, quadletDir+"/localai.env.example"))
	reportProblems(t, localAIEnvProblems(quadletDir+"/localai.env.example", env))
	secret := parseEnvFile(readRepoFile(t, quadletDir+"/alixai.env.example"))
	if !strings.HasPrefix(secret["LOCALAI_API_KEY"], "change-me") || len(secret) != 1 {
		t.Errorf("alixai.env.example must hold only the placeholder LOCALAI_API_KEY, got %d keys", len(secret))
	}

	// Drop-ins and opt-in files can only do what their name says.
	for _, d := range quadletDropIns(t) {
		reportProblems(t, dropInProblems(d))
	}

	// detector
	good := `[Container]
Image=docker.io/localai/localai:v4.10.0@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b
User=10001
Group=10001
ReadOnly=true
DropCapability=ALL
NoNewPrivileges=true
PidsLimit=64
HealthCmd=true
Notify=healthy
Volume=%h/x:/models:ro,z
[Service]
Restart=on-failure
`
	if ps := quadletContainerProblems(parseUnit("good.container", []byte(good))); len(ps) != 0 {
		t.Fatalf("a clean unit was flagged: %v", ps)
	}
	for name, edit := range map[string][2]string{
		"root user":        {"User=10001", "User=0"},
		"named user":       {"User=10001", "User=nobody"},
		"writable rootfs":  {"ReadOnly=true", "ReadOnly=false"},
		"no drop":          {"DropCapability=ALL", "DropCapability=NET_RAW"},
		"added capability": {"DropCapability=ALL", "DropCapability=ALL\nAddCapability=CHOWN"},
		"new privileges":   {"NoNewPrivileges=true", "NoNewPrivileges=false"},
		"no pids limit":    {"PidsLimit=64", ""},
		"no health":        {"HealthCmd=true", ""},
		"no notify":        {"Notify=healthy", ""},
		"tag image":        {"@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b", ""},
		"device":           {"PidsLimit=64", "PidsLimit=64\nAddDevice=/dev/kvm"},
		"host network":     {"PidsLimit=64", "PidsLimit=64\nNetwork=host"},
		"keep-id":          {"PidsLimit=64", "PidsLimit=64\nUserNS=keep-id:uid=1000"},
		"privileged arg":   {"PidsLimit=64", "PidsLimit=64\nPodmanArgs=--privileged"},
		"cap-add arg":      {"PidsLimit=64", "PidsLimit=64\nPodmanArgs=--memory=1g --cap-add=SYS_ADMIN"},
		"named volume":     {"Volume=%h/x:/models:ro,z", "Volume=models:/models:ro"},
		"anonymous volume": {"Volume=%h/x:/models:ro,z", "Volume=/models"},
		"writable models":  {"Volume=%h/x:/models:ro,z", "Volume=%h/x:/models:z"},
		"docker socket":    {"Volume=%h/x:/models:ro,z", "Volume=/run/docker.sock:/s"},
		"no restart":       {"Restart=on-failure", "Restart=no"},
	} {
		u := parseUnit("bad.container", []byte(strings.Replace(good, edit[0], edit[1], 1)))
		if len(quadletContainerProblems(u)) == 0 {
			t.Errorf("%s went unnoticed", name)
		}
	}
}

// parseEnvFile reads KEY=VALUE lines of an environment file.
func parseEnvFile(data []byte) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			env[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return env
}

func localAIEnvProblems(path string, env map[string]string) []string {
	ps := envProblems(path, env)
	for flag, off := range localAIOffFlags {
		if env[flag] != off {
			ps = append(ps, fmt.Sprintf("%s: %s must be %s, is %q", path, flag, off, env[flag]))
		}
	}
	if env["ALIXAI_MODELS_READONLY"] != "true" {
		ps = append(ps, path+": ALIXAI_MODELS_READONLY must be true")
	}
	if env["LOCALAI_LOG_LEVEL"] != "info" {
		ps = append(ps, path+": LOCALAI_LOG_LEVEL must be info")
	}
	if _, ok := env["LOCALAI_API_KEY"]; ok {
		ps = append(ps, path+": the API key is a secret and does not belong in a file that is committed")
	}
	if _, ok := env["ALIXAI_ALLOW_VERBOSE_LOG"]; ok {
		ps = append(ps, path+": ALIXAI_ALLOW_VERBOSE_LOG is a development switch")
	}
	sort.Strings(ps)
	return ps
}

// dropInProblems holds each optional file to what it is for: profiles set limits, labels and the profile
// name; the external files add one network to the pod; the noexternal file sets one build argument.
func dropInProblems(d unit) []string {
	var ps []string
	at := d.path
	allowed := func(section string, keys ...string) {
		for _, s := range sectionNames(d) {
			if s != section {
				ps = append(ps, fmt.Sprintf("%s: unexpected section [%s]", at, s))
			}
		}
		for _, k := range d.keys(section) {
			if !contains(keys, k) {
				ps = append(ps, fmt.Sprintf("%s: [%s] sets %s, which this file may not touch", at, section, k))
			}
		}
	}
	switch {
	case pathUnder(d, "profiles"):
		allowed("Container", "PodmanArgs", "Environment", "Label")
		for _, a := range d.all("Container", "PodmanArgs") {
			if m := forbiddenPodmanArgs.FindString(a); m != "" {
				ps = append(ps, fmt.Sprintf("%s: PodmanArgs %q uses %q", at, a, strings.TrimSpace(m)))
			}
		}
	case strings.HasSuffix(d.path, "external/alixai.pod.d/10-external.conf"):
		allowed("Pod", "Network")
		if n := d.all("Pod", "Network"); len(n) != 1 || n[0] != "alixai-egress.network" {
			ps = append(ps, fmt.Sprintf("%s: must add only alixai-egress.network, got %v", at, n))
		}
	case strings.HasSuffix(d.path, "external/alixai-egress.network"):
		allowed("Network", "NetworkName")
		if strings.EqualFold(d.get("Network", "Internal"), "true") {
			ps = append(ps, at+": the egress network is the opt-in route out and must not be internal")
		}
	case pathUnder(d, "noexternal"):
		allowed("Build", "BuildArg")
		if a := d.all("Build", "BuildArg"); len(a) != 1 || a[0] != "GO_TAGS=noexternal" {
			ps = append(ps, fmt.Sprintf("%s: must set only GO_TAGS=noexternal, got %v", at, a))
		}
	default:
		ps = append(ps, at+": an opt-in or profile file nothing here knows how to check")
	}
	return ps
}

// ---- no init containers: Quadlet ----

// quadletInitProblems finds one-shot units and start orders that do not wait for a health check.
func quadletInitProblems(units []unit) []string {
	var ps []string
	healthy := map[string]bool{}
	for _, u := range units {
		if strings.HasSuffix(u.path, ".container") {
			healthy[strings.TrimSuffix(u.unitName(), ".container")+".service"] = u.has("Container", "HealthCmd") && u.get("Container", "Notify") == "healthy"
		}
	}
	for _, u := range units {
		at := u.unitName()
		if initName.MatchString(strings.TrimSuffix(at, "."+u.path[strings.LastIndex(u.path, ".")+1:])) {
			ps = append(ps, at+": the name says it is an init or setup unit")
		}
		if strings.EqualFold(u.get("Service", "Type"), "oneshot") {
			ps = append(ps, at+": Type=oneshot is a one-shot container")
		}
		for _, key := range []string{"RemainAfterExit", "ExecStartPre", "ExecStartPost", "ExecStart"} {
			if u.has("Service", key) {
				ps = append(ps, fmt.Sprintf("%s: [Service] %s runs a setup step beside the container", at, key))
			}
		}
		if strings.EqualFold(u.get("Service", "Restart"), "no") {
			ps = append(ps, at+": Restart=no is how a one-shot unit is declared")
		}
		for _, key := range []string{"Requires", "BindsTo", "Wants", "After"} {
			for _, list := range u.all("Unit", key) {
				for _, dep := range strings.Fields(list) {
					ok, isContainer := healthy[dep]
					if isContainer && !ok {
						ps = append(ps, fmt.Sprintf("%s: %s=%s, which has no HealthCmd and Notify=healthy to wait for", at, key, dep))
					}
				}
			}
		}
	}
	sort.Strings(ps)
	return ps
}

func TestQuadletStartOrderIsHealthGated(t *testing.T) {
	units := quadletUnits(t)
	byName := map[string]unit{}
	for _, u := range units {
		byName[u.unitName()] = u
	}
	// the chain: localai -> anchor -> alixai -> relay, each Requires and After the previous one
	for unitName, prev := range map[string]string{
		"alixai-anchor.container": "alixai-localai.service",
		"alixai.container":        "alixai-anchor.service",
		"alixai-relay.container":  "alixai.service",
	} {
		u := byName[unitName]
		if !contains(strings.Fields(strings.Join(u.all("Unit", "Requires"), " ")), prev) || !contains(strings.Fields(strings.Join(u.all("Unit", "After"), " ")), prev) {
			t.Errorf("%s must Require and be After %s", unitName, prev)
		}
	}

	good := []unit{
		parseUnit("a.container", []byte("[Container]\nHealthCmd=true\nNotify=healthy\n[Service]\nRestart=on-failure\n")),
		parseUnit("b.container", []byte("[Unit]\nRequires=a.service\nAfter=a.service\n[Container]\nHealthCmd=true\nNotify=healthy\n[Service]\nRestart=on-failure\n")),
	}
	if ps := quadletInitProblems(good); len(ps) != 0 {
		t.Errorf("a gated chain was flagged: %v", ps)
	}
	for name, doc := range map[string]string{
		"oneshot":           "[Service]\nType=oneshot\n",
		"remain":            "[Service]\nRemainAfterExit=yes\n",
		"exec start pre":    "[Service]\nExecStartPre=/usr/bin/podman run --rm busybox true\n",
		"restart no":        "[Service]\nRestart=no\n",
		"ungated dep":       "[Unit]\nRequires=a.service\n",
		"init in the name":  "",
		"setup in the name": "",
	} {
		units := []unit{
			parseUnit("a.container", []byte("[Container]\nNotify=conmon\n")),
			parseUnit("b.container", []byte(doc)),
		}
		switch name {
		case "init in the name":
			units[1] = parseUnit("db-init.container", []byte("[Container]\n"))
		case "setup in the name":
			units[1] = parseUnit("setup.container", []byte("[Container]\n"))
		}
		if len(quadletInitProblems(units)) == 0 {
			t.Errorf("%s went unnoticed", name)
		}
	}
}
