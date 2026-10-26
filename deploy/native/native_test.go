package native

import (
	"bytes"
	"context"
	"encoding/xml"
	"net"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/cli"
)

// unit is a parsed systemd unit: section name to key to every value given for it, in order.
type unit map[string]map[string][]string

func parseUnit(t *testing.T, path string) unit {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u := unit{}
	section := ""
	for i, line := range strings.Split(string(raw), "\n") {
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
				t.Fatalf("%s:%d: not a directive: %q", path, i+1, line)
			}
			u[section][k] = append(u[section][k], strings.TrimSpace(v))
		}
	}
	return u
}

// one returns the single value of a directive, or "" and false when it is absent.
func (u unit) one(t *testing.T, section, key string) (string, bool) {
	t.Helper()
	vs := u[section][key]
	if len(vs) > 1 {
		t.Fatalf("[%s] %s is given %d times", section, key, len(vs))
	}
	if len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// SEC-080: the unit runs as a non-root identity of its own with the sandbox switched on, no capability and no
// privilege gain; a weakened unit fails here, in review, not on a host.
func TestSecServiceUnitHardening(t *testing.T) {
	u := parseUnit(t, "systemd/alixai.service")
	want := map[string]string{
		"DynamicUser":             "yes",
		"NoNewPrivileges":         "yes",
		"ProtectSystem":           "strict",
		"ProtectHome":             "yes",
		"PrivateTmp":              "yes",
		"PrivateDevices":          "yes",
		"ProtectKernelTunables":   "yes",
		"ProtectKernelModules":    "yes",
		"ProtectKernelLogs":       "yes",
		"ProtectControlGroups":    "yes",
		"ProtectClock":            "yes",
		"ProtectHostname":         "yes",
		"RestrictSUIDSGID":        "yes",
		"RestrictRealtime":        "yes",
		"RestrictNamespaces":      "yes",
		"LockPersonality":         "yes",
		"MemoryDenyWriteExecute":  "yes",
		"SystemCallArchitectures": "native",
		"CapabilityBoundingSet":   "",
		"AmbientCapabilities":     "",
		"StateDirectoryMode":      "0700",
		"UMask":                   "0077",
		"IPAddressDeny":           "any",
		"IPAddressAllow":          "localhost",
		"MemorySwapMax":           "0",
	}
	for k, v := range want {
		if got, ok := u.one(t, "Service", k); !ok || got != v {
			t.Errorf("%s = %q (present %v), want %q", k, got, ok, v)
		}
	}
	for _, k := range []string{"MemoryMax", "CPUQuota", "TasksMax"} {
		if got, ok := u.one(t, "Service", k); !ok || got == "" || got == "infinity" {
			t.Errorf("%s = %q: the unit must carry a finite cap", k, got)
		}
	}
	if user, ok := u.one(t, "Service", "User"); ok && (user == "root" || user == "0") {
		t.Errorf("User = %q: the service must not run as root", user)
	}
	if af, _ := u.one(t, "Service", "RestrictAddressFamilies"); strings.Contains(af, "AF_NETLINK") || strings.Contains(af, "AF_PACKET") {
		t.Errorf("RestrictAddressFamilies = %q: a loopback service needs neither netlink nor raw packets", af)
	}
	if f := strings.Join(u["Service"]["SystemCallFilter"], " "); !strings.Contains(f, "~") || !strings.Contains(f, "@mount") || !strings.Contains(f, "@module") {
		t.Errorf("SystemCallFilter = %q: want a deny list that includes @mount and @module", f)
	}
	// A prefix makes systemd run the command with full privileges or outside the sandbox.
	for _, key := range []string{"ExecStart", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecReload"} {
		for _, cmd := range u["Service"][key] {
			if strings.HasPrefix(cmd, "+") || strings.HasPrefix(cmd, "!") || strings.HasPrefix(cmd, "|") {
				t.Errorf("%s=%q runs with elevated privileges or outside the sandbox", key, cmd)
			}
		}
	}
	if _, ok := u["Service"]["PermissionsStartOnly"]; ok {
		t.Error("PermissionsStartOnly must not be set")
	}
	start, _ := u.one(t, "Service", "ExecStart")
	if f := strings.Fields(start); len(f) < 4 || !strings.HasPrefix(f[0], "/") || f[1] != "serve" || f[2] != "-config" {
		t.Errorf("ExecStart = %q, want an absolute binary running `serve -config <file>`", start)
	}
}

// The unit, its configuration and the README must agree on paths: the key reaches the service only through the
// credential store, and the data directory is the unit's state directory.
func TestSystemdUnitAndConfigAgree(t *testing.T) {
	u := parseUnit(t, "systemd/alixai.service")
	cfg := readConfig(t, "systemd/alixai.yaml")

	start, _ := u.one(t, "Service", "ExecStart")
	if !strings.HasSuffix(start, " -config /etc/alixai/alixai.yaml") {
		t.Errorf("ExecStart = %q, want the config /etc/alixai/alixai.yaml", start)
	}
	if cond, _ := u.one(t, "Unit", "ConditionPathExists"); cond != "/etc/alixai/alixai.yaml" {
		t.Errorf("ConditionPathExists = %q", cond)
	}
	state, _ := u.one(t, "Service", "StateDirectory")
	if got, want := cfg.DataDir, "/var/lib/"+state; got != want {
		t.Errorf("data_dir = %q, want the unit's state directory %q", got, want)
	}
	cred, _ := u.one(t, "Service", "LoadCredential")
	id, _, _ := strings.Cut(cred, ":")
	if want := "file:/run/credentials/alixai.service/" + id; id == "" || cfg.LocalAI.APIKeyRef != want {
		t.Errorf("api_key_ref = %q, want %q for LoadCredential=%q", cfg.LocalAI.APIKeyRef, want, cred)
	}
}

// plistNode is one element of a property list: its name, text and children.
type plistNode struct {
	XMLName  xml.Name
	Text     string      `xml:",chardata"`
	Children []plistNode `xml:",any"`
}

// plistDict turns the children of a <dict> element into key to value node.
func plistDict(t *testing.T, n plistNode) map[string]plistNode {
	t.Helper()
	if n.XMLName.Local != "dict" || len(n.Children)%2 != 0 {
		t.Fatalf("not a key/value dict: <%s> with %d children", n.XMLName.Local, len(n.Children))
	}
	out := map[string]plistNode{}
	for i := 0; i < len(n.Children); i += 2 {
		k := n.Children[i]
		if k.XMLName.Local != "key" {
			t.Fatalf("expected <key>, got <%s>", k.XMLName.Local)
		}
		out[strings.TrimSpace(k.Text)] = n.Children[i+1]
	}
	return out
}

// SEC-080 for macOS: a system daemon that drops to its own non-root account, restarts on failure only, and carries no
// secret in the property list.
func TestSecLaunchdPlistHardening(t *testing.T) {
	raw, err := os.ReadFile("launchd/ai.alixai.service.plist")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Dict plistNode `xml:"dict"`
	}
	if err := xml.NewDecoder(bytes.NewReader(raw)).Decode(&doc); err != nil {
		t.Fatalf("the plist is not well-formed XML: %v", err)
	}
	d := plistDict(t, doc.Dict)

	text := func(key string) string { return strings.TrimSpace(d[key].Text) }
	if text("Label") == "" {
		t.Error("Label is empty")
	}
	for _, k := range []string{"UserName", "GroupName"} {
		if v := text(k); v == "" || v == "root" || v == "wheel" || v == "0" {
			t.Errorf("%s = %q: the daemon must drop to its own account", k, v)
		}
	}
	if got := text("Umask"); got != "63" { // 0077
		t.Errorf("Umask = %q, want 63 (octal 0077)", got)
	}
	if d["RunAtLoad"].XMLName.Local != "true" {
		t.Error("RunAtLoad must be true")
	}
	keep := plistDict(t, d["KeepAlive"])
	if keep["SuccessfulExit"].XMLName.Local != "false" {
		t.Error("KeepAlive must restart after a failure only (SuccessfulExit false)")
	}
	args := d["ProgramArguments"].Children
	if len(args) < 4 || !strings.HasPrefix(strings.TrimSpace(args[0].Text), "/") || strings.TrimSpace(args[1].Text) != "serve" || strings.TrimSpace(args[2].Text) != "-config" {
		t.Errorf("ProgramArguments = %v, want an absolute binary running `serve -config <file>`", args)
	}
	for _, k := range []string{"EnvironmentVariables", "Sockets", "inetdCompatibility", "SessionCreate"} {
		if _, ok := d[k]; ok {
			t.Errorf("%s is set; a secret does not belong in the plist and the daemon opens no socket of launchd's", k)
		}
	}

	cfg := readConfig(t, "launchd/alixai.yaml")
	if want := strings.TrimSpace(args[3].Text); want != "/usr/local/etc/alixai/alixai.yaml" {
		t.Errorf("config argument = %q", want)
	}
	if wd := text("WorkingDirectory"); wd != cfg.DataDir {
		t.Errorf("WorkingDirectory = %q, want data_dir %q", wd, cfg.DataDir)
	}
}

type nativeConfig struct {
	DataDir string `yaml:"data_dir"`
	Server  struct {
		Listen string `yaml:"listen"`
	} `yaml:"server"`
	LocalAI struct {
		Endpoint  string `yaml:"endpoint"`
		APIKeyRef string `yaml:"api_key_ref"`
	} `yaml:"localai"`
	Inference struct {
		ModelsDir string   `yaml:"models_dir"`
		Models    []string `yaml:"models"`
	} `yaml:"inference"`
}

func readConfig(t *testing.T, path string) nativeConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c nativeConfig
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
}

// Each shipped configuration loads and validates under the real section set, listens on loopback, reaches LocalAI on
// loopback, names its key by reference and keeps the inference path off until a model is provisioned.
func TestNativeConfigsLoadAndStayLoopback(t *testing.T) {
	for _, path := range []string{"systemd/alixai.yaml", "launchd/alixai.yaml", "windows/alixai.yaml"} {
		t.Run(path, func(t *testing.T) {
			var out, errOut bytes.Buffer
			env := &cli.Env{
				Stdout:    &out,
				Stderr:    &errOut,
				LookupEnv: func(string) (string, bool) { return "", false },
			}
			if code := cli.Run(context.Background(), []string{"config", "check", "-config", path}, env); code != cli.ExitOK {
				t.Fatalf("config check = %d\n%s%s", code, out.String(), errOut.String())
			}
			c := readConfig(t, path)
			host, _, err := net.SplitHostPort(c.Server.Listen)
			if err != nil || host != "127.0.0.1" {
				t.Errorf("server.listen = %q, want a 127.0.0.1 address", c.Server.Listen)
			}
			if !strings.HasPrefix(c.LocalAI.Endpoint, "http://127.0.0.1:") {
				t.Errorf("localai.endpoint = %q, want loopback", c.LocalAI.Endpoint)
			}
			if !strings.HasPrefix(c.LocalAI.APIKeyRef, "file:") {
				t.Errorf("localai.api_key_ref = %q, want a file reference", c.LocalAI.APIKeyRef)
			}
			if c.DataDir == "" || (c.DataDir[0] != '/' && !(len(c.DataDir) > 2 && c.DataDir[1] == ':')) {
				t.Errorf("data_dir = %q must be absolute: a service does not start in the install directory", c.DataDir)
			}
			if len(c.Inference.Models) != 0 || c.Inference.ModelsDir == "" {
				t.Errorf("inference = %+v: ships off, with a models directory named", c.Inference)
			}
		})
	}
}

type goreleaserBuild struct {
	ID      string   `yaml:"id"`
	Main    string   `yaml:"main"`
	Binary  string   `yaml:"binary"`
	Env     []string `yaml:"env"`
	Flags   []string `yaml:"flags"`
	LDFlags []string `yaml:"ldflags"`
	Tags    []string `yaml:"tags"`
	Targets []string `yaml:"targets"`
}

// goreleaserArchiveFile is one archives[].files entry: a plain path string, or an
// object with src (and optional info.mtime for reproducible archives).
type goreleaserArchiveFile string

func (f *goreleaserArchiveFile) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		*f = goreleaserArchiveFile(value.Value)
		return nil
	case yaml.MappingNode:
		var obj struct {
			Src string `yaml:"src"`
		}
		if err := value.Decode(&obj); err != nil {
			return err
		}
		if obj.Src == "" {
			return &yaml.TypeError{Errors: []string{"archives files entry needs src"}}
		}
		*f = goreleaserArchiveFile(obj.Src)
		return nil
	default:
		return &yaml.TypeError{Errors: []string{"archives files entry must be a string or an object with src"}}
	}
}

type goreleaserFile struct {
	Version  int               `yaml:"version"`
	Builds   []goreleaserBuild `yaml:"builds"`
	Archives []struct {
		IDs   []string                `yaml:"ids"`
		Files []goreleaserArchiveFile `yaml:"files"`
	} `yaml:"archives"`
	Release struct {
		Disable bool `yaml:"disable"`
	} `yaml:"release"`
}

// The release configuration builds exactly the four supported targets, static and trimmed, and publishes nothing.
// It has two builds, the service and the parser worker (M5-22); both follow the same rules, the service also carries the
// version stamps.
func TestGoreleaserBuildsTheFourStaticTargets(t *testing.T) {
	raw, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var g goreleaserFile
	if err := yaml.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if g.Version != 2 || len(g.Builds) != 2 {
		t.Fatalf("want GoReleaser config version 2 with two builds (alixai and alixai-parser), got version %d and %d builds", g.Version, len(g.Builds))
	}
	for _, want := range []struct{ id, main string }{{"alixai", "./cmd/alixai"}, {"alixai-parser", "./cmd/alixai-parser"}} {
		var b *goreleaserBuild
		for i := range g.Builds {
			if g.Builds[i].ID == want.id {
				b = &g.Builds[i]
			}
		}
		if b == nil {
			t.Errorf("no build with id %q", want.id)
			continue
		}
		if b.Main != want.main {
			t.Errorf("%s: main = %q, want %q", want.id, b.Main, want.main)
		}
		if !slices.Contains(b.Env, "CGO_ENABLED=0") {
			t.Errorf("%s: env = %v: the binary must be static", want.id, b.Env)
		}
		if !slices.Contains(b.Flags, "-trimpath") {
			t.Errorf("%s: flags = %v: want -trimpath", want.id, b.Flags)
		}
		ld := strings.Join(b.LDFlags, " ")
		if !strings.Contains(ld, "-s -w") {
			t.Errorf("%s: ldflags %q lack -s -w", want.id, ld)
		}
		if want.id == "alixai" {
			for _, stamp := range []string{"internal/version.Version=", "internal/version.Commit=", "internal/version.Date="} {
				if !strings.Contains(ld, stamp) {
					t.Errorf("ldflags %q lack %q", ld, stamp)
				}
			}
		}
		got := slices.Clone(b.Targets)
		slices.Sort(got)
		if wantTargets := []string{"darwin_arm64", "linux_amd64", "linux_arm64", "windows_amd64"}; !slices.Equal(got, wantTargets) {
			t.Errorf("%s: targets = %v, want %v", want.id, got, wantTargets)
		}
		if len(b.Tags) != 0 {
			t.Errorf("%s: tags = %v: a release binary is built with no build tags", want.id, b.Tags)
		}
	}
	if !g.Release.Disable {
		t.Error("release must stay disabled: publishing belongs to the release pipeline (M5-03)")
	}
	if len(g.Archives) == 0 || !slices.Contains(g.Archives[0].Files, goreleaserArchiveFile("deploy/native/**/*")) {
		t.Error("the archives must carry deploy/native")
	}
	if wantIDs := []string{"alixai", "alixai-parser"}; len(g.Archives) == 0 || !slices.Equal(g.Archives[0].IDs, wantIDs) {
		got := []string(nil)
		if len(g.Archives) > 0 {
			got = g.Archives[0].IDs
		}
		t.Errorf("archives[0].ids = %v, want %v (build order; release runs GoReleaser with --parallelism 1 so archive member order stays stable)", got, wantIDs)
	}
	for _, f := range []string{"../../LICENSE", "../../NOTICE", "systemd/alixai.service", "launchd/ai.alixai.service.plist", "windows/alixai.yaml"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("the archives name a file that does not exist: %v", err)
		}
	}
	// The release workflow must keep --parallelism 1: higher parallelism lets the two binaries
	// finish in either order and the packed archives then differ between the two compare runs.
	wf, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if c := strings.Count(string(wf), "goreleaser release --clean --skip=publish --parallelism 1"); c != 2 {
		t.Errorf("release.yml: want two GoReleaser invocations with --parallelism 1 (the double-build compare), found %d", c)
	}
}
