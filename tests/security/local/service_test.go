package localsec

import (
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/tests/security/secutil"
)

// definitions holds the text of every service definition the suite judges, by repository path, so the shipped files and
// a mutated copy of them go through the same code.
type definitions map[string]string

const (
	stackDir   = "deploy/container/stack/"
	devDir     = "deploy/container/localai/"
	quadletDir = "deploy/container/quadlet/"
	systemdDir = "deploy/native/systemd/"
	launchdDir = "deploy/native/launchd/"
	winSource  = "internal/cli/cmd_service.go"
)

func loadDefinitions(t testing.TB) definitions {
	t.Helper()
	d := definitions{}
	for _, rel := range secutil.RepoFiles(t) {
		base := path.Base(rel)
		switch {
		case strings.HasPrefix(rel, stackDir) && strings.HasPrefix(base, "compose") && strings.HasSuffix(base, ".yaml"),
			strings.HasPrefix(rel, devDir) && strings.HasPrefix(base, "compose") && strings.HasSuffix(base, ".yaml"),
			strings.HasPrefix(rel, quadletDir) && (strings.HasSuffix(base, ".container") || (strings.HasSuffix(base, ".conf") && strings.Contains(rel, ".container.d/"))),
			strings.HasPrefix(rel, systemdDir) && strings.HasSuffix(base, ".service"),
			strings.HasPrefix(rel, launchdDir) && strings.HasSuffix(base, ".plist"),
			rel == winSource:
			d[rel] = string(secutil.ReadFile(t, rel))
		}
	}
	return d
}

func (d definitions) clone() definitions {
	c := definitions{}
	for k, v := range d {
		c[k] = v
	}
	return c
}

func (d definitions) paths(pred func(string) bool) []string {
	var out []string
	for p := range d {
		if pred(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// judged counts what a run looked at, so a run that looked at nothing fails.
type judged struct{ stack, dev, containers, dropIns, units, plists, windows int }

// judge applies every rule of SEC-080 to the definitions.
func judge(d definitions) (ps problems, n judged, err error) {
	add := func(p problems, e error) {
		ps = append(ps, p...)
		if e != nil && err == nil {
			err = e
		}
	}
	for _, p := range d.paths(func(p string) bool { return strings.HasPrefix(p, stackDir) }) {
		role := "override"
		if path.Base(p) == "compose.yaml" {
			role = "base"
			add(composeIdentityProblems(p, d[p], "alixai", "localai", "anchor", "relay"))
		}
		add(composeProblems(p, d[p], role))
		n.stack++
	}
	for _, p := range d.paths(func(p string) bool { return strings.HasPrefix(p, devDir) }) {
		role := "dev-override"
		if path.Base(p) == "compose.yaml" {
			role = "dev"
		}
		add(composeProblems(p, d[p], role))
		n.dev++
	}
	units := map[string]string{}
	for _, p := range d.paths(func(p string) bool { return strings.HasPrefix(p, quadletDir) }) {
		if strings.HasSuffix(p, ".container") {
			units[path.Base(p)] = d[p]
			add(quadletContainerProblems(p, d[p], false))
			n.containers++
		} else {
			add(quadletContainerProblems(p, d[p], true))
			n.dropIns++
		}
	}
	add(quadletIdentityProblems(units, "alixai.container", "alixai-localai.container"))
	for _, other := range []string{"alixai-anchor.container", "alixai-relay.container"} {
		add(quadletIdentityProblems(units, other, "alixai.container"))
		add(quadletIdentityProblems(units, other, "alixai-localai.container"))
	}
	for _, p := range d.paths(func(p string) bool { return strings.HasPrefix(p, systemdDir) }) {
		add(systemdProblems(p, d[p]))
		n.units++
	}
	add(systemdIdentityProblems("alixai.service", d[systemdDir+"alixai.service"], "localai.service", d[systemdDir+"localai.service"]))
	plists := map[string]string{}
	for _, p := range d.paths(func(p string) bool { return strings.HasPrefix(p, launchdDir) }) {
		plists[p] = d[p]
		add(launchdProblems(p, d[p]))
		n.plists++
	}
	add(launchdIdentityProblems(plists, launchdDir+"ai.alixai.service.plist"))
	if src, ok := d[winSource]; ok {
		ps = append(ps, windowsServiceProblems(winSource, src)...)
		n.windows++
	}
	return ps, n, err
}

// mutation breaks one shipped definition in memory (and may add a file); the rules must then report rule.
type mutation struct {
	rule, file, old, new string
	add                  map[string]string
}

var mutations = []mutation{
	// compose, production stack
	{"user-root", stackDir + "compose.yaml", `user: "10001:10001"`, `user: "0:0"`, nil},
	{"user-root", stackDir + "compose.yaml", `user: "65532:65532"`, `user: "65532:0"`, nil},
	{"user-numeric", stackDir + "compose.yaml", `user: "10001:10001"`, `user: "localai"`, nil},
	{"user-numeric", stackDir + "compose.yaml", `    user: "10001:10001"` + "\n", ``, nil},
	{"shared-identity", stackDir + "compose.yaml", `user: "65532:65532"`, `user: "10001:10001"`, nil},
	{"shared-identity", stackDir + "compose.yaml", `user: "65534:65534"`, `user: "10001:65534"`, nil},
	{"read-only-rootfs", stackDir + "compose.yaml", `read_only: true`, `read_only: false`, nil},
	{"cap-drop-all", stackDir + "compose.yaml", "    cap_drop:\n      - ALL\n    pids_limit: 512", "    cap_drop:\n      - NET_RAW\n    pids_limit: 512", nil},
	{"cap-add", stackDir + "compose.yaml", "    cap_drop:\n      - ALL\n    pids_limit: 512", "    cap_drop:\n      - ALL\n    cap_add:\n      - SYS_ADMIN\n    pids_limit: 512", nil},
	{"no-new-privileges", stackDir + "compose.yaml", `no-new-privileges:true`, `no-new-privileges:false`, nil},
	{"privileged", stackDir + "compose.yaml", "    container_name: alixai-localai\n", "    container_name: alixai-localai\n    privileged: true\n", nil},
	{"host-namespace", stackDir + "compose.yaml", `network_mode: "service:anchor"`, `network_mode: host`, nil},
	{"host-namespace", stackDir + "compose.yaml", "    container_name: alixai-localai\n", "    container_name: alixai-localai\n    pid: host\n", nil},
	{"socket-mount", stackDir + "compose.yaml", `- ../localai/entrypoint.sh:/alixai/entrypoint.sh:ro`, `- /var/run/docker.sock:/var/run/docker.sock`, nil},
	{"privileged", stackDir + "compose.lite.yaml", "      ALIXAI_PROFILE: lite", "      ALIXAI_PROFILE: lite\n    privileged: true", nil},
	{"cap-add", stackDir + "compose.lite.yaml", "      ALIXAI_PROFILE: lite", "      ALIXAI_PROFILE: lite\n    cap_add: [NET_ADMIN]", nil},
	{"read-only-rootfs", stackDir + "compose.lite.yaml", "      ALIXAI_PROFILE: lite", "      ALIXAI_PROFILE: lite\n    read_only: false", nil},
	// compose, standalone development runtime
	{"cap-add", devDir + "compose.yaml", "      - CHOWN", "      - SYS_ADMIN", nil},
	{"no-new-privileges", devDir + "compose.yaml", `no-new-privileges:true`, `no-new-privileges:false`, nil},
	{"cap-drop-all", devDir + "compose.yaml", "    cap_drop:\n      - ALL", "    cap_drop:\n      - NET_RAW", nil},
	{"privileged", devDir + "compose.lite.yaml", "    cpus: 4\n", "    cpus: 4\n    privileged: true\n", nil},
	// Quadlet
	{"user-root", quadletDir + "alixai-localai.container", "User=10001", "User=0", nil},
	{"user-numeric", quadletDir + "alixai-localai.container", "User=10001\n", "", nil},
	{"shared-identity", quadletDir + "alixai-localai.container", "User=10001", "User=65532", nil},
	{"shared-identity", quadletDir + "alixai-localai.container", "Group=10001", "Group=65532", nil},
	{"read-only-rootfs", quadletDir + "alixai-localai.container", "ReadOnly=true", "ReadOnly=false", nil},
	{"cap-drop-all", quadletDir + "alixai-localai.container", "DropCapability=ALL\n", "", nil},
	{"cap-add", quadletDir + "alixai-localai.container", "NoNewPrivileges=true", "NoNewPrivileges=true\nAddCapability=SYS_ADMIN", nil},
	{"no-new-privileges", quadletDir + "alixai-localai.container", "NoNewPrivileges=true", "NoNewPrivileges=false", nil},
	{"privileged", quadletDir + "alixai-localai.container", "NoNewPrivileges=true", "NoNewPrivileges=true\nPodmanArgs=--privileged", nil},
	{"host-namespace", quadletDir + "alixai-localai.container", "Network=alixai-backend.network", "Network=host", nil},
	{"socket-mount", quadletDir + "alixai.container", "Volume=%h/.config/alixai/alixai.yaml:/etc/alixai/alixai.yaml:ro,Z", "Volume=/run/podman/podman.sock:/run/podman.sock", nil},
	{"user-root", quadletDir + "profiles/lite/alixai-localai.container.d/10-profile.conf", "Environment=LOCALAI_THREADS=4", "Environment=LOCALAI_THREADS=4\nUser=0", nil},
	{"privileged", quadletDir + "profiles/lite/alixai-localai.container.d/10-profile.conf", "PodmanArgs=--memory=8g --memory-swap=8g --cpus=4", "PodmanArgs=--memory=8g --privileged", nil},
	{"cap-add", quadletDir + "profiles/4g/alixai-localai.container.d/10-profile.conf", "Environment=LOCALAI_THREADS=4", "Environment=LOCALAI_THREADS=4\nAddCapability=NET_ADMIN", nil},
	// systemd
	{"sandbox-nonewprivileges", systemdDir + "alixai.service", "NoNewPrivileges=yes", "NoNewPrivileges=no", nil},
	{"sandbox-protectsystem", systemdDir + "alixai.service", "ProtectSystem=strict", "ProtectSystem=full", nil},
	{"sandbox-privatetmp", systemdDir + "alixai.service", "PrivateTmp=yes", "PrivateTmp=no", nil},
	{"sandbox-privatetmp", systemdDir + "localai.service", "PrivateTmp=yes", "", nil},
	{"sandbox-protecthome", systemdDir + "localai.service", "ProtectHome=yes", "ProtectHome=no", nil},
	{"cap-drop-all", systemdDir + "alixai.service", "CapabilityBoundingSet=\n", "CapabilityBoundingSet=CAP_SYS_ADMIN\n", nil},
	{"cap-drop-all", systemdDir + "localai.service", "CapabilityBoundingSet=\n", "", nil},
	{"cap-drop-all", systemdDir + "localai.service", "AmbientCapabilities=\n", "AmbientCapabilities=CAP_NET_BIND_SERVICE\n", nil},
	{"user-root", systemdDir + "alixai.service", "DynamicUser=yes", "DynamicUser=yes\nUser=root", nil},
	{"user-root", systemdDir + "localai.service", "DynamicUser=yes", "DynamicUser=no", nil},
	{"umask", systemdDir + "alixai.service", "UMask=0077", "UMask=0022", nil},
	{"state-mode", systemdDir + "localai.service", "StateDirectoryMode=0700", "StateDirectoryMode=0755", nil},
	{"privileged-exec", systemdDir + "alixai.service", "ExecStart=/usr/local/bin/alixai", "ExecStart=+/usr/local/bin/alixai", nil},
	{"shell-exec", systemdDir + "alixai.service", "ExecStart=/usr/local/bin/alixai serve -config /etc/alixai/alixai.yaml", `ExecStart=/bin/sh -c "/usr/local/bin/alixai serve -config /etc/alixai/alixai.yaml"`, nil},
	{"supplementary-groups", systemdDir + "alixai.service", "UMask=0077", "UMask=0077\nSupplementaryGroups=docker", nil},
	{"address-families", systemdDir + "localai.service", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_PACKET", nil},
	{"write-paths", systemdDir + "alixai.service", "UMask=0077", "UMask=0077\nReadWritePaths=/etc", nil},
	{"secret-in-unit", systemdDir + "localai.service", "UMask=0077", "UMask=0077\nEnvironment=LOCALAI_API_KEY=0123456789abcdef0123456789abcdef", nil},
	{"shared-identity", systemdDir + "localai.service", "StateDirectory=alixai-localai", "StateDirectory=alixai", nil},
	{"shared-identity", systemdDir + "localai.service", "UMask=0077", "UMask=0077\nReadWritePaths=/var/lib/alixai", nil},
	// launchd
	{"user-root", launchdDir + "ai.alixai.service.plist", "<key>UserName</key>\n  <string>_alixai</string>\n", "", nil},
	{"user-root", launchdDir + "ai.alixai.service.plist", "<key>GroupName</key>\n  <string>_alixai</string>", "<key>GroupName</key>\n  <string>wheel</string>", nil},
	{"umask", launchdDir + "ai.alixai.service.plist", "<integer>63</integer>", "<integer>18</integer>", nil},
	{"secret-in-unit", launchdDir + "ai.alixai.service.plist", "<key>RunAtLoad</key>", "<key>EnvironmentVariables</key><dict><key>LOCALAI_API_KEY</key><string>x</string></dict><key>RunAtLoad</key>", nil},
	{"shared-identity", launchdDir + "ai.alixai.service.plist", "<string>ai.alixai.service</string>", "<string>ai.alixai.service</string>", map[string]string{
		launchdDir + "ai.alixai.localai.plist": launchdPlistFor("ai.alixai.localai", "_alixai", "_alixai", "/opt/localai/local-ai"),
	}},
	// Windows
	{"windows-account", winSource, "`NT AUTHORITY\\LocalService`", "`LocalSystem`", nil},
	{"windows-account", winSource, "ServiceStartName: *account,", `ServiceStartName: "",`, nil},
	{"windows-account", winSource, "*account = `NT SERVICE\\` + *name", "*account = `NT AUTHORITY\\LocalService`", nil},
}

func launchdPlistFor(label, user, group, program string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>` + label + `</string>
<key>ProgramArguments</key><array><string>` + program + `</string></array>
<key>UserName</key><string>` + user + `</string>
<key>GroupName</key><string>` + group + `</string>
<key>Umask</key><integer>63</integer>
</dict></plist>`
}

// every rule the suite relies on must have at least one mutation of a shipped file that triggers it.
var requiredRules = []string{
	"user-root", "user-numeric", "shared-identity", "read-only-rootfs", "cap-drop-all", "cap-add", "no-new-privileges",
	"privileged", "host-namespace", "socket-mount", "sandbox-nonewprivileges", "sandbox-protectsystem", "sandbox-privatetmp",
	"sandbox-protecthome", "umask", "state-mode", "privileged-exec", "shell-exec", "supplementary-groups", "address-families",
	"write-paths", "secret-in-unit", "windows-account",
}

// SEC-080
func TestSecServiceUnitHardening(t *testing.T) {
	real := loadDefinitions(t)

	t.Run("shipped_definitions", func(t *testing.T) {
		ps, n, err := judge(real)
		if err != nil {
			t.Fatalf("a definition could not be read: %v", err)
		}
		if n.stack < 6 || n.dev < 4 || n.containers < 4 || n.dropIns < 3 || n.units < 2 || n.plists < 1 || n.windows != 1 {
			t.Fatalf("the scan looked at too little: %+v (stack compose files, dev compose files, Quadlet containers, drop-ins, systemd units, plists, Windows source)", n)
		}
		for _, p := range ps {
			t.Error(p)
		}
		t.Logf("judged %+v", n)
	})

	t.Run("an_empty_scan_fails", func(t *testing.T) {
		_, n, _ := judge(definitions{})
		if n.stack != 0 || n.units != 0 || n.windows != 0 {
			t.Errorf("an empty set judged something: %+v", n)
		}
		ps, _, _ := judge(definitions{systemdDir + "alixai.service": "[Service]\n", systemdDir + "localai.service": "[Service]\n"})
		if len(ps) == 0 {
			t.Error("two empty units passed")
		}
	})

	t.Run("planted_violations_in_shipped_files_are_flagged", func(t *testing.T) {
		covered := map[string]bool{}
		for _, m := range mutations {
			src, ok := real[m.file]
			if !ok {
				t.Errorf("%s is not among the judged definitions: the mutation list is stale", m.file)
				continue
			}
			if !strings.Contains(src, m.old) {
				t.Errorf("%s no longer contains %q: update the mutation", m.file, m.old)
				continue
			}
			mut := real.clone()
			mut[m.file] = strings.ReplaceAll(src, m.old, m.new)
			for p, s := range m.add {
				mut[p] = s
			}
			ps, _, err := judge(mut)
			if err != nil {
				t.Errorf("%s: %q -> %q made the file unreadable (%v): the control must break a rule, not the syntax", m.file, m.old, m.new, err)
				continue
			}
			if !ps.has(m.rule) {
				t.Errorf("breaking %s (%q -> %q) was not flagged as %s; problems: %v", m.file, m.old, m.new, m.rule, ps)
				continue
			}
			covered[m.rule] = true
		}
		for _, r := range requiredRules {
			if !covered[r] {
				t.Errorf("no planted violation proves the rules flag %s", r)
			}
		}
	})

	t.Run("a_weakened_override_cannot_hide_behind_a_clean_base", func(t *testing.T) {
		clean := "services:\n  alixai:\n    user: \"65532:65532\"\n    read_only: true\n    cap_drop: [ALL]\n    security_opt: [no-new-privileges:true]\n"
		if ps, err := composeProblems("clean.yaml", clean, "base"); err != nil || len(ps) != 0 {
			t.Fatalf("the clean inline service is flagged: %v %v", ps, err)
		}
		for rule, bad := range map[string]string{
			"privileged":        "services:\n  alixai:\n    privileged: true\n",
			"cap-add":           "services:\n  alixai:\n    cap_add: [SYS_PTRACE]\n",
			"user-root":         "services:\n  alixai:\n    user: \"0:0\"\n",
			"read-only-rootfs":  "services:\n  alixai:\n    read_only: false\n",
			"host-namespace":    "services:\n  alixai:\n    pid: host\n",
			"no-new-privileges": "services:\n  alixai:\n    security_opt: [\"no-new-privileges:false\"]\n",
		} {
			ps, err := composeProblems("override.yaml", bad, "override")
			if err != nil || !ps.has(rule) {
				t.Errorf("an override with %s was not flagged: %v %v", rule, ps, err)
			}
		}
		if ps, _ := composeProblems("override.yaml", "services:\n  alixai:\n    environment:\n      A: b\n", "override"); len(ps) != 0 {
			t.Errorf("an innocent override is flagged: %v", ps)
		}
	})

	t.Run("inline_identity_controls", func(t *testing.T) {
		a := "[Service]\nUser=svc\nStateDirectory=a\n"
		b := "[Service]\nUser=other\nStateDirectory=b\n"
		if ps, err := systemdIdentityProblems("a.service", a, "b.service", b); err != nil || len(ps) != 0 {
			t.Errorf("two distinct units are flagged: %v %v", ps, err)
		}
		for name, bad := range map[string]string{
			"same user":     "[Service]\nUser=svc\nStateDirectory=b\n",
			"same state":    "[Service]\nUser=other\nStateDirectory=a\n",
			"no state":      "[Service]\nUser=other\n",
			"reaches state": "[Service]\nUser=other\nStateDirectory=b\nReadWritePaths=/var/lib/a\n",
		} {
			if ps, err := systemdIdentityProblems("a.service", a, "b.service", bad); err != nil || !ps.has("shared-identity") {
				t.Errorf("%s was not flagged: %v %v", name, ps, err)
			}
		}
		// a path that merely begins with the other unit's name is not the other unit's directory
		near := "[Service]\nUser=other\nStateDirectory=b\nReadWritePaths=/var/lib/a-extra\n"
		if ps, _ := systemdIdentityProblems("a.service", a, "b.service", near); ps.has("shared-identity") {
			t.Errorf("/var/lib/a-extra was taken for /var/lib/a: %v", ps)
		}
		u := map[string]string{"x.container": "[Container]\nUser=1\nGroup=2\n", "y.container": "[Container]\nUser=1\nGroup=3\n"}
		if ps, _ := quadletIdentityProblems(u, "x.container", "y.container"); !ps.has("shared-identity") {
			t.Errorf("two containers with one user were not flagged: %v", ps)
		}
		if ps, _ := composeIdentityProblems("c.yaml", "services:\n  a:\n    user: \"1:1\"\n  l:\n    user: \"1:2\"\n", "a", "l"); !ps.has("shared-identity") {
			t.Errorf("two services with one uid were not flagged: %v", ps)
		}
		if ps, _ := composeIdentityProblems("c.yaml", "services:\n  a:\n    user: \"1:1\"\n  l:\n    user: \"2:2\"\n  f:\n    user: \"1:9\"\n", "a", "l", "f"); !ps.has("shared-identity") {
			t.Errorf("a forwarder sharing the AlixAI uid was not flagged: %v", ps)
		}
	})

	t.Run("separate_identities_across_forms", func(t *testing.T) {
		// AlixAI and LocalAI differ in every form that ships, checked directly and not through the rule set above.
		type id struct{ form, alixai, localai string }
		var ids []id
		for _, p := range []string{stackDir + "compose.yaml"} {
			svcs, err := composeServices(p, real[p])
			if err != nil {
				t.Fatal(err)
			}
			m := map[string]string{}
			for _, s := range svcs {
				m[s.name] = str(s.svc["user"])
			}
			ids = append(ids, id{"compose", m["alixai"], m["localai"]})
		}
		for _, name := range []string{"alixai.container", "alixai-localai.container"} {
			if real[quadletDir+name] == "" {
				t.Fatalf("%s is missing", name)
			}
		}
		qa, _ := parseINI(real[quadletDir+"alixai.container"])
		ql, _ := parseINI(real[quadletDir+"alixai-localai.container"])
		ua, _ := qa.one("Container", "User")
		ul, _ := ql.one("Container", "User")
		ids = append(ids, id{"quadlet", ua, ul})
		for _, i := range ids {
			if i.alixai == "" || i.localai == "" || i.alixai == i.localai {
				t.Errorf("%s: AlixAI runs as %q and LocalAI as %q", i.form, i.alixai, i.localai)
			}
			if strings.HasPrefix(i.alixai, "0") || strings.HasPrefix(i.localai, "0") {
				t.Errorf("%s: a service runs as root (%q, %q)", i.form, i.alixai, i.localai)
			}
		}
		// systemd: a transient user each, and a state directory of its own
		sa, _ := parseINI(real[systemdDir+"alixai.service"])
		sl, _ := parseINI(real[systemdDir+"localai.service"])
		da, _ := sa.one("Service", "DynamicUser")
		dl, _ := sl.one("Service", "DynamicUser")
		stA, _ := sa.one("Service", "StateDirectory")
		stL, _ := sl.one("Service", "StateDirectory")
		if !isYes(da) || !isYes(dl) || stA == "" || stL == "" || stA == stL {
			t.Errorf("systemd: DynamicUser %q/%q, StateDirectory %q/%q: AlixAI and LocalAI must each have a transient user and their own state", da, dl, stA, stL)
		}
	})
}
