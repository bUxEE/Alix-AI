package localsec

import (
	"encoding/xml"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The rules of SEC-080, one function per kind of service definition. Each takes the text of a file and returns the
// problems it has, every problem tagged with the id of the rule it breaks, so a planted violation can be asked for by
// id. The text is passed in, not read, so the same rule judges the shipped file and a mutated copy of it.

type problems []string

func (p *problems) add(rule, where, format string, a ...any) {
	*p = append(*p, fmt.Sprintf("[%s] %s: %s", rule, where, fmt.Sprintf(format, a...)))
}

func (p problems) has(rule string) bool {
	for _, s := range p {
		if strings.HasPrefix(s, "["+rule+"]") {
			return true
		}
	}
	return false
}

// ---- compose ----

func anyMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func anyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	}
	return []any{v}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if b, ok := v.(bool); ok {
		return strconv.FormatBool(b)
	}
	return fmt.Sprint(v)
}

func strList(v any) []string {
	var out []string
	for _, e := range anyList(v) {
		out = append(out, str(e))
	}
	return out
}

// uidGid reads "uid:gid" with both numeric.
func uidGid(user string) (uid, gid int, ok bool) {
	u, g, found := strings.Cut(strings.TrimSpace(user), ":")
	if !found {
		return 0, 0, false
	}
	a, e1 := strconv.Atoi(u)
	b, e2 := strconv.Atoi(g)
	return a, b, e1 == nil && e2 == nil
}

var hostNamespaceKeys = []string{"network_mode", "pid", "ipc", "uts", "userns_mode"}

// composeService is one service of a compose file, with the name of the file it came from.
type composeService struct {
	file, name string
	svc        map[string]any
}

func composeServices(file, text string) ([]composeService, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var out []composeService
	names := []string{}
	for n := range anyMap(doc["services"]) {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, composeService{file, n, anyMap(anyMap(doc["services"])[n])})
	}
	return out, nil
}

var devCaps = map[string]bool{"CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true}

// composeProblems judges one compose file. role is "base" for the production base file (every property is required),
// "override" for an override of it (a property it sets must not weaken the base: absence is not a finding), and "dev" and
// "dev-override" for the standalone M0-08 runtime and its overrides, which is the development profile of the checklist: it keeps the properties that
// hold for it (capabilities dropped and only file-access ones added back, no new privileges, nothing privileged, no host
// namespace, no engine socket) and not the production-only ones (a non-root user, a read-only root filesystem).
func composeProblems(file, text, role string) (problems, error) {
	svcs, err := composeServices(file, text)
	if err != nil {
		return nil, err
	}
	var ps problems
	for _, s := range svcs {
		at := file + " service " + s.name
		sv := s.svc
		strict := role == "base" || role == "dev"
		devProfile := strings.HasPrefix(role, "dev")
		has := func(key string) bool { _, ok := sv[key]; return ok }

		if user := str(sv["user"]); !devProfile && (strict || has("user")) {
			uid, gid, ok := uidGid(user)
			if !ok {
				ps.add("user-numeric", at, "user %q must be a numeric uid:gid", user)
			} else if uid <= 0 || gid <= 0 {
				ps.add("user-root", at, "user %q runs as root or the root group", user)
			}
		}
		if !devProfile && (strict || has("read_only")) && str(sv["read_only"]) != "true" {
			ps.add("read-only-rootfs", at, "read_only is %q, want true", str(sv["read_only"]))
		}
		drops := strList(sv["cap_drop"])
		if (strict || has("cap_drop")) && !containsFold(drops, "ALL") {
			ps.add("cap-drop-all", at, "cap_drop %v does not contain ALL", drops)
		}
		for _, c := range strList(sv["cap_add"]) {
			if !devProfile || !devCaps[strings.ToUpper(c)] {
				ps.add("cap-add", at, "cap_add %s gives back a capability", c)
			}
		}
		nnp := false
		for _, o := range strList(sv["security_opt"]) {
			o = strings.ReplaceAll(strings.ToLower(o), "=", ":")
			if o == "no-new-privileges:true" || o == "no-new-privileges" {
				nnp = true
			}
			if o == "no-new-privileges:false" || strings.HasSuffix(o, ":unconfined") {
				ps.add("no-new-privileges", at, "security_opt %q", o)
			}
		}
		if strict && !nnp {
			ps.add("no-new-privileges", at, "security_opt lacks no-new-privileges:true")
		}
		if str(sv["privileged"]) == "true" {
			ps.add("privileged", at, "privileged: true")
		}
		for _, k := range hostNamespaceKeys {
			if strings.TrimSpace(str(sv[k])) == "host" {
				ps.add("host-namespace", at, "%s: host", k)
			}
		}
		if len(anyList(sv["devices"])) > 0 {
			ps.add("devices", at, "devices are passed through")
		}
		for _, v := range anyList(sv["volumes"]) {
			spec := str(v)
			if m, ok := v.(map[string]any); ok {
				spec = str(m["source"]) + ":" + str(m["target"])
			}
			low := strings.ToLower(spec)
			if strings.Contains(low, "docker.sock") || strings.Contains(low, "podman.sock") || strings.Contains(low, "containerd.sock") {
				ps.add("socket-mount", at, "volume %q mounts the container engine", spec)
			}
			if src, _, _ := strings.Cut(spec, ":"); src == "/" || src == "/etc" || src == "/proc" || src == "/sys" || src == "/dev" || src == "/root" {
				ps.add("socket-mount", at, "volume %q mounts a host path the service must not touch", spec)
			}
		}
	}
	return ps, nil
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// composeIdentityProblems checks that AlixAI and LocalAI run as different identities, and that the forwarders (which may
// share the unprivileged "nobody" identity between them) share neither of theirs.
func composeIdentityProblems(file, text, alixai, localai string, forwarders ...string) (problems, error) {
	svcs, err := composeServices(file, text)
	if err != nil {
		return nil, err
	}
	var ps problems
	idOf := map[string][2]int{}
	for _, name := range append([]string{alixai, localai}, forwarders...) {
		found := false
		for _, s := range svcs {
			if s.name == name {
				found = true
				if uid, gid, ok := uidGid(str(s.svc["user"])); ok {
					idOf[name] = [2]int{uid, gid}
				}
			}
		}
		if !found {
			ps.add("shared-identity", file, "service %s is missing, so its identity cannot be compared", name)
		}
	}
	same := func(a, b string) {
		x, okx := idOf[a]
		y, oky := idOf[b]
		if okx && oky && (x[0] == y[0] || x[1] == y[1]) {
			ps.add("shared-identity", file, "services %s and %s share a uid or gid (%d:%d and %d:%d)", a, b, x[0], x[1], y[0], y[1])
		}
	}
	same(alixai, localai)
	for _, f := range forwarders {
		same(alixai, f)
		same(localai, f)
	}
	return ps, nil
}

// ---- systemd and Quadlet ----

type ini map[string]map[string][]string

func parseINI(text string) (ini, error) {
	u := ini{}
	section := ""
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			if u[section] == nil {
				u[section] = map[string][]string{}
			}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || section == "" {
				return nil, fmt.Errorf("line %d: not a directive: %q", i+1, line)
			}
			u[section][strings.TrimSpace(k)] = append(u[section][strings.TrimSpace(k)], strings.TrimSpace(v))
		}
	}
	return u, nil
}

func (u ini) all(sec, key string) []string { return u[sec][key] }

func (u ini) one(sec, key string) (string, bool) {
	vs := u[sec][key]
	if len(vs) == 0 {
		return "", false
	}
	return vs[len(vs)-1], true
}

var podmanPrivilegeRE = regexp.MustCompile(`(^|\s)--(privileged|cap-add|device|userns[= ]+host|network[= ]+host|net[= ]+host|pid[= ]+host|ipc[= ]+host|uts[= ]+host|security-opt[= ]+\S*(unconfined|disable))\b`)

func numericID(v string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	return n, err == nil
}

// quadletContainerProblems judges one .container file. dropIn is true for a drop-in, which must not weaken what the
// unit sets (absence is not a finding there).
func quadletContainerProblems(file, text string, dropIn bool) (problems, error) {
	u, err := parseINI(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var ps problems
	at := path.Base(file)
	c := "Container"
	need := !dropIn
	for _, key := range []string{"User", "Group"} {
		v, ok := u.one(c, key)
		switch {
		case !ok && need:
			ps.add("user-numeric", at, "%s is not set: the container would run as the image's default (often root)", key)
		case ok:
			if n, num := numericID(v); !num {
				ps.add("user-numeric", at, "%s=%q must be a numeric id", key, v)
			} else if n <= 0 {
				ps.add("user-root", at, "%s=%d is root", key, n)
			}
		}
	}
	if v, ok := u.one(c, "ReadOnly"); (need && !strings.EqualFold(v, "true")) || (ok && !strings.EqualFold(v, "true")) {
		ps.add("read-only-rootfs", at, "ReadOnly=%q, want true", v)
	}
	drops := u.all(c, "DropCapability")
	if (need && !containsFold(drops, "ALL")) || (len(drops) > 0 && !containsFold(drops, "ALL")) {
		ps.add("cap-drop-all", at, "DropCapability %v does not contain ALL", drops)
	}
	if len(u.all(c, "AddCapability")) > 0 {
		ps.add("cap-add", at, "AddCapability %v gives back a capability", u.all(c, "AddCapability"))
	}
	if v, ok := u.one(c, "NoNewPrivileges"); (need && !strings.EqualFold(v, "true")) || (ok && !strings.EqualFold(v, "true")) {
		ps.add("no-new-privileges", at, "NoNewPrivileges=%q, want true", v)
	}
	for _, key := range []string{"AddDevice", "SecurityLabelDisable", "Unmask", "SeccompProfile", "AppArmor", "Sysctl"} {
		if len(u.all(c, key)) > 0 {
			ps.add("privileged", at, "%s relaxes the sandbox", key)
		}
	}
	for _, a := range u.all(c, "PodmanArgs") {
		if m := podmanPrivilegeRE.FindString(a); m != "" {
			ps.add("privileged", at, "PodmanArgs %q uses %q", a, strings.TrimSpace(m))
		}
	}
	for _, key := range []string{"Network", "UserNS"} {
		for _, v := range u.all(c, key) {
			if strings.EqualFold(v, "host") {
				ps.add("host-namespace", at, "%s=host", key)
			}
		}
	}
	for _, v := range u.all(c, "Volume") {
		low := strings.ToLower(v)
		if strings.Contains(low, "docker.sock") || strings.Contains(low, "podman.sock") || strings.HasPrefix(low, "/:") || strings.HasPrefix(low, "/etc:") {
			ps.add("socket-mount", at, "Volume=%s", v)
		}
	}
	return ps, nil
}

// quadletIdentityProblems checks that the AlixAI container and the LocalAI container do not share a user or a group.
func quadletIdentityProblems(units map[string]string, a, b string) (problems, error) {
	var ps problems
	ids := map[string]map[string]string{}
	for _, name := range []string{a, b} {
		text, ok := units[name]
		if !ok {
			ps.add("shared-identity", name, "unit is missing, so its identity cannot be compared")
			continue
		}
		u, err := parseINI(text)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		user, _ := u.one("Container", "User")
		group, _ := u.one("Container", "Group")
		ids[name] = map[string]string{"user": user, "group": group}
	}
	if len(ids) == 2 {
		for _, k := range []string{"user", "group"} {
			if ids[a][k] != "" && ids[a][k] == ids[b][k] {
				ps.add("shared-identity", a+" and "+b, "both containers run with %s %s", k, ids[a][k])
			}
		}
	}
	return ps, nil
}

var (
	systemdYes     = []string{"NoNewPrivileges", "PrivateTmp", "ProtectHome", "PrivateDevices", "ProtectKernelTunables", "ProtectKernelModules", "ProtectControlGroups", "RestrictSUIDSGID", "LockPersonality"}
	riskyGroupsRE  = regexp.MustCompile(`(?i)\b(root|wheel|sudo|docker|disk|adm|shadow)\b`)
	riskyWritePath = map[string]bool{"/": true, "/etc": true, "/usr": true, "/var": true, "/home": true, "/root": true, "/boot": true, "/run": true, "/sys": true, "/proc": true, "/dev": true}
)

// systemdProblems judges one .service file: a non-root identity of its own, no capability, no privilege gain, and the
// sandbox of the unit switched on.
func systemdProblems(file, text string) (problems, error) {
	u, err := parseINI(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var ps problems
	at := path.Base(file)
	s := "Service"
	for _, k := range systemdYes {
		if v, ok := u.one(s, k); !ok || !isYes(v) {
			ps.add("sandbox-"+strings.ToLower(k), at, "%s=%q, want yes", k, v)
		}
	}
	if v, ok := u.one(s, "ProtectSystem"); !ok || v != "strict" {
		ps.add("sandbox-protectsystem", at, "ProtectSystem=%q, want strict", v)
	}
	if v, ok := u.one(s, "UMask"); !ok || !umaskBlocksOthers(v) {
		ps.add("umask", at, "UMask=%q must exclude group and others (0077)", v)
	}
	dyn, _ := u.one(s, "DynamicUser")
	user, hasUser := u.one(s, "User")
	switch {
	case hasUser && (user == "root" || user == "0"):
		ps.add("user-root", at, "User=%s", user)
	case !isYes(dyn) && !hasUser:
		ps.add("user-root", at, "neither DynamicUser=yes nor User= is set: the unit runs as root")
	}
	if g, ok := u.one(s, "Group"); ok && (g == "root" || g == "0") {
		ps.add("user-root", at, "Group=%s", g)
	}
	if g := strings.Join(u.all(s, "SupplementaryGroups"), " "); riskyGroupsRE.MatchString(g) {
		ps.add("supplementary-groups", at, "SupplementaryGroups=%s", g)
	}
	for _, k := range []string{"CapabilityBoundingSet", "AmbientCapabilities"} {
		vs, ok := u[s][k]
		if k == "CapabilityBoundingSet" && !ok {
			ps.add("cap-drop-all", at, "CapabilityBoundingSet is not set: every capability stays in the bounding set")
			continue
		}
		for _, v := range vs {
			if strings.TrimSpace(v) != "" {
				ps.add("cap-drop-all", at, "%s=%q must be empty", k, v)
			}
		}
	}
	for _, k := range []string{"ExecStart", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecReload"} {
		for _, cmd := range u.all(s, k) {
			if strings.HasPrefix(cmd, "+") || strings.HasPrefix(cmd, "!") || strings.HasPrefix(cmd, "|") {
				ps.add("privileged-exec", at, "%s=%q runs outside the sandbox or with full privileges", k, cmd)
			}
			if regexp.MustCompile(`(^|/)(ba|da|z)?sh\s+-c\b`).MatchString(strings.TrimLeft(cmd, "-@")) {
				ps.add("shell-exec", at, "%s=%q runs a shell", k, cmd)
			}
		}
	}
	for _, v := range u.all(s, "RestrictAddressFamilies") {
		if strings.Contains(v, "AF_PACKET") || strings.Contains(v, "AF_NETLINK") {
			ps.add("address-families", at, "RestrictAddressFamilies=%s allows raw packets or netlink", v)
		}
	}
	for _, k := range []string{"ReadWritePaths", "BindPaths"} {
		for _, v := range u.all(s, k) {
			for _, p := range strings.Fields(v) {
				p, _, _ = strings.Cut(strings.TrimPrefix(strings.TrimPrefix(p, "-"), "+"), ":")
				if riskyWritePath[strings.TrimRight(p, "/")] || p == "/" {
					ps.add("write-paths", at, "%s=%s opens a system directory to the unit", k, p)
				}
			}
		}
	}
	for _, v := range u.all(s, "Environment") {
		if regexp.MustCompile(`(?i)(KEY|TOKEN|SECRET|PASSWORD)=\S`).MatchString(v) {
			ps.add("secret-in-unit", at, "Environment=%s carries a secret value; use LoadCredential=", "…")
		}
	}
	if dir, ok := u.one(s, "StateDirectory"); ok && dir != "" {
		if m, ok := u.one(s, "StateDirectoryMode"); !ok || !modeOwnerOnly(m) {
			ps.add("state-mode", at, "StateDirectoryMode=%q must be owner-only (0700)", m)
		}
	}
	return ps, nil
}

func isYes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "on", "1":
		return true
	}
	return false
}

func parseOctal(v string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 8, 32)
	return n, err == nil
}

func umaskBlocksOthers(v string) bool { n, ok := parseOctal(v); return ok && n&0o077 == 0o077 }

func modeOwnerOnly(v string) bool { n, ok := parseOctal(v); return ok && n&0o077 == 0 }

// systemdIdentityProblems checks that two units do not share an identity: each has its own transient or named user,
// its own state directory, and neither can write the other's.
func systemdIdentityProblems(aName, aText, bName, bText string) (problems, error) {
	a, err := parseINI(aText)
	if err != nil {
		return nil, err
	}
	b, err := parseINI(bText)
	if err != nil {
		return nil, err
	}
	var ps problems
	s := "Service"
	for _, k := range []string{"User", "Group"} {
		av, aok := a.one(s, k)
		bv, bok := b.one(s, k)
		if aok && bok && av == bv {
			ps.add("shared-identity", aName+" and "+bName, "both units set %s=%s", k, av)
		}
	}
	as, _ := a.one(s, "StateDirectory")
	bs, _ := b.one(s, "StateDirectory")
	if as == "" || bs == "" || as == bs {
		ps.add("shared-identity", aName+" and "+bName, "StateDirectory %q and %q must both be set and different", as, bs)
	}
	reach := func(owner string, other ini, otherState string) {
		for _, key := range []string{"ReadWritePaths", "ReadOnlyPaths", "BindPaths", "BindReadOnlyPaths"} {
			for _, v := range other.all(s, key) {
				for _, p := range strings.Fields(v) {
					if otherState != "" && strings.Contains(p+"/", "/"+otherState+"/") {
						ps.add("shared-identity", owner, "%s=%s reaches the other unit's state directory", key, p)
					}
				}
			}
		}
	}
	reach(bName, b, as)
	reach(aName, a, bs)
	return ps, nil
}

// ---- launchd ----

type plistNode struct {
	XMLName  xml.Name
	Text     string      `xml:",chardata"`
	Children []plistNode `xml:",any"`
}

func plistDict(n plistNode) (map[string]plistNode, error) {
	if n.XMLName.Local != "dict" || len(n.Children)%2 != 0 {
		return nil, fmt.Errorf("not a key/value dict: <%s> with %d children", n.XMLName.Local, len(n.Children))
	}
	out := map[string]plistNode{}
	for i := 0; i < len(n.Children); i += 2 {
		if n.Children[i].XMLName.Local != "key" {
			return nil, fmt.Errorf("expected <key>, got <%s>", n.Children[i].XMLName.Local)
		}
		out[strings.TrimSpace(n.Children[i].Text)] = n.Children[i+1]
	}
	return out, nil
}

type plistInfo struct {
	user, group string
	umask       int
	umaskSet    bool
	args        []string
	dict        map[string]plistNode
}

func readPlist(text string) (plistInfo, error) {
	var doc struct {
		Dict plistNode `xml:"dict"`
	}
	if err := xml.NewDecoder(strings.NewReader(text)).Decode(&doc); err != nil {
		return plistInfo{}, fmt.Errorf("not well-formed XML: %w", err)
	}
	d, err := plistDict(doc.Dict)
	if err != nil {
		return plistInfo{}, err
	}
	pi := plistInfo{dict: d}
	pi.user = strings.TrimSpace(d["UserName"].Text)
	pi.group = strings.TrimSpace(d["GroupName"].Text)
	if n, err := strconv.Atoi(strings.TrimSpace(d["Umask"].Text)); err == nil {
		pi.umask, pi.umaskSet = n, true
	}
	for _, c := range d["ProgramArguments"].Children {
		pi.args = append(pi.args, strings.TrimSpace(c.Text))
	}
	return pi, nil
}

var secretKeyRE = regexp.MustCompile(`(?i)(key|token|secret|password)`)

// launchdProblems judges one LaunchDaemon plist.
func launchdProblems(file, text string) (problems, error) {
	pi, err := readPlist(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var ps problems
	at := path.Base(file)
	switch pi.user {
	case "":
		ps.add("user-root", at, "no UserName: a LaunchDaemon without one runs as root")
	case "root", "0":
		ps.add("user-root", at, "UserName=%s", pi.user)
	}
	switch pi.group {
	case "":
		ps.add("user-root", at, "no GroupName: the daemon keeps the wheel group")
	case "wheel", "admin", "0", "root":
		ps.add("user-root", at, "GroupName=%s", pi.group)
	}
	if !pi.umaskSet || pi.umask&0o077 != 0o077 {
		ps.add("umask", at, "Umask=%d (octal %o) must exclude group and others (0077 = 63)", pi.umask, pi.umask)
	}
	if env, ok := pi.dict["EnvironmentVariables"]; ok {
		if m, err := plistDict(env); err == nil {
			for k := range m {
				if secretKeyRE.MatchString(k) {
					ps.add("secret-in-unit", at, "EnvironmentVariables carries %s", k)
				}
			}
		}
	}
	if _, ok := pi.dict["Sockets"]; ok {
		ps.add("privileged", at, "Sockets: launchd would open a listener for the daemon")
	}
	return ps, nil
}

// launchdIdentityProblems checks that no two plists use the same account, and that a plist which launches LocalAI does
// not run as the AlixAI account.
func launchdIdentityProblems(plists map[string]string, alixai string) (problems, error) {
	var ps problems
	byUser := map[string]string{}
	var names []string
	for n := range plists {
		names = append(names, n)
	}
	sort.Strings(names)
	alixaiUser := ""
	if t, ok := plists[alixai]; ok {
		pi, err := readPlist(t)
		if err != nil {
			return nil, err
		}
		alixaiUser = pi.user
	}
	for _, n := range names {
		pi, err := readPlist(plists[n])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		if other, dup := byUser[pi.user]; dup && pi.user != "" {
			ps.add("shared-identity", n, "runs as %s, like %s", pi.user, other)
		}
		byUser[pi.user] = n
		if n != alixai && alixaiUser != "" && pi.user == alixaiUser {
			ps.add("shared-identity", n, "shares the AlixAI account %s", alixaiUser)
		}
	}
	return ps, nil
}

// ---- Windows ----

var (
	accountFuncRE = regexp.MustCompile(`(?s)func serviceCheckAccount\(.*?\n}\n`)
	literalRE     = regexp.MustCompile("`[^`]*`|\"[^\"]*\"")
)

// windowsServiceProblems judges the Windows service definition, which is Go code (internal/cli/cmd_service.go): the
// service is registered under the per-service virtual account by default, the account it is registered under is the
// checked one, and the check admits the virtual account and LocalService only, never LocalSystem or a named user.
func windowsServiceProblems(file, src string) problems {
	var ps problems
	at := path.Base(file)
	if !strings.Contains(src, "ServiceStartName: *account") {
		ps.add("windows-account", at, "the service is not created under the checked account (ServiceStartName: *account)")
	}
	if !strings.Contains(src, "*account = `NT SERVICE\\` + *name") {
		ps.add("windows-account", at, "the default account is not the per-service virtual account NT SERVICE\\<name>")
	}
	fn := accountFuncRE.FindString(src)
	if fn == "" {
		ps.add("windows-account", at, "serviceCheckAccount is not there: nothing limits the account")
		return ps
	}
	allowed := map[string]bool{`nt service\`: true, `nt authority\localservice`: true}
	body := fn[strings.Index(fn, "{"):]
	n := 0
	for _, lit := range literalRE.FindAllString(body, -1) {
		v := strings.ToLower(lit[1 : len(lit)-1])
		if strings.HasPrefix(v, "-account must be") {
			continue
		}
		n++
		if !allowed[v] {
			ps.add("windows-account", at, "serviceCheckAccount admits %s", lit)
		}
	}
	if n < 2 {
		ps.add("windows-account", at, "serviceCheckAccount does not name the accounts it admits")
	}
	if regexp.MustCompile(`(?i)localsystem|nt authority\\system`).MatchString(strings.Replace(body, "never runs as LocalSystem", "", 1)) {
		ps.add("windows-account", at, "serviceCheckAccount mentions LocalSystem outside its refusal")
	}
	return ps
}
