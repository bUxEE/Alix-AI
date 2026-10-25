package native

import (
	"bytes"
	"context"
	"net"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/cli"
)

// localaiConfig is what the supervisor and the LocalAI adapter read from either native configuration.
type localaiConfig struct {
	LocalAI struct {
		Endpoint          string   `yaml:"endpoint"`
		APIKeyRef         string   `yaml:"api_key_ref"`
		QualifiedVersions []string `yaml:"qualified_versions"`
	} `yaml:"localai"`
	Supervisor struct {
		Enabled bool   `yaml:"enabled"`
		Runtime string `yaml:"runtime"`
		Exec    struct {
			Path      string   `yaml:"path"`
			Args      []string `yaml:"args"`
			Env       []string `yaml:"env"`
			APIKeyEnv string   `yaml:"api_key_env"`
		} `yaml:"exec"`
	} `yaml:"supervisor"`
}

func readLocalAIConfig(t *testing.T, path string) localaiConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c localaiConfig
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
}

// SEC-080 for the separate LocalAI unit (M5-21): its own identity, no capability, no privilege gain, loopback only, a
// finite memory cap with no swap, and restart on failure; its key arrives through its own credential store.
func TestSecLocalAIUnitHardening(t *testing.T) {
	u := parseUnit(t, "systemd/localai.service")
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
		"SystemCallArchitectures": "native",
		"CapabilityBoundingSet":   "",
		"AmbientCapabilities":     "",
		"StateDirectoryMode":      "0700",
		"UMask":                   "0077",
		"IPAddressDeny":           "any",
		"IPAddressAllow":          "localhost",
		"MemorySwapMax":           "0",
		"Restart":                 "on-failure",
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
		t.Errorf("User = %q: LocalAI must not run as root", user)
	}
	if af, _ := u.one(t, "Service", "RestrictAddressFamilies"); strings.Contains(af, "AF_NETLINK") || strings.Contains(af, "AF_PACKET") {
		t.Errorf("RestrictAddressFamilies = %q: a loopback runtime needs neither netlink nor raw packets", af)
	}
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

	// Its own identity and its own state: nothing is shared with the AlixAI service.
	a := parseUnit(t, "systemd/alixai.service")
	as, _ := a.one(t, "Service", "StateDirectory")
	ls, _ := u.one(t, "Service", "StateDirectory")
	if ls == "" || ls == as {
		t.Errorf("StateDirectory = %q, want a directory of its own (AlixAI's is %q)", ls, as)
	}
	if user, ok := a.one(t, "Service", "User"); ok {
		if lu, ok := u.one(t, "Service", "User"); ok && lu == user {
			t.Errorf("both units run as %q", user)
		}
	}

	// The unit runs the supervisor's exec command with the configuration the repository ships for it.
	start, _ := u.one(t, "Service", "ExecStart")
	if f := strings.Fields(start); len(f) != 5 || !strings.HasPrefix(f[0], "/") || f[1] != "supervisor" || f[2] != "exec" || f[3] != "-config" || f[4] != "/etc/alixai/localai.yaml" {
		t.Errorf("ExecStart = %q, want an absolute binary running `supervisor exec -config /etc/alixai/localai.yaml`", start)
	}
	if cond, _ := u.one(t, "Unit", "ConditionPathExists"); cond != "/etc/alixai/localai.yaml" {
		t.Errorf("ConditionPathExists = %q", cond)
	}
	cfg := readLocalAIConfig(t, "systemd/localai.yaml")
	cred, _ := u.one(t, "Service", "LoadCredential")
	id, _, _ := strings.Cut(cred, ":")
	if want := "file:/run/credentials/localai.service/" + id; id == "" || cfg.LocalAI.APIKeyRef != want {
		t.Errorf("api_key_ref = %q, want %q for LoadCredential=%q", cfg.LocalAI.APIKeyRef, want, cred)
	}
}

// The exec supervisor belongs to the LocalAI unit and never to the AlixAI service: the service's unit and
// configuration only attach (SEC-080, HC-41), and the two configurations agree on where LocalAI listens.
func TestExecSupervisorRunsOnlyInTheLocalAIUnit(t *testing.T) {
	a := parseUnit(t, "systemd/alixai.service")
	for _, key := range []string{"ExecStart", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecReload"} {
		for _, cmd := range a["Service"][key] {
			if strings.Contains(cmd, "supervisor") {
				t.Errorf("alixai.service %s=%q: the exec supervisor must not run inside the AlixAI service", key, cmd)
			}
		}
	}
	after := strings.Fields(strings.Join(a["Unit"]["After"], " "))
	if !slices.Contains(a["Unit"]["Wants"], "localai.service") || !slices.Contains(after, "localai.service") {
		t.Errorf("alixai.service must want and follow localai.service: Wants=%v After=%v", a["Unit"]["Wants"], a["Unit"]["After"])
	}

	svc := readLocalAIConfig(t, "systemd/alixai.yaml")
	if !svc.Supervisor.Enabled || svc.Supervisor.Runtime != "unit" || svc.Supervisor.Exec.Path != "" {
		t.Errorf("alixai.yaml supervisor = %+v, want enabled with the attach (unit) runtime and no exec section", svc.Supervisor)
	}
	lai := readLocalAIConfig(t, "systemd/localai.yaml")
	s := lai.Supervisor
	if !s.Enabled || s.Runtime != "exec" || !strings.HasPrefix(s.Exec.Path, "/") || s.Exec.APIKeyEnv == "" {
		t.Errorf("localai.yaml supervisor = %+v, want the enabled exec runtime with an absolute path and a key variable", s)
	}
	env := map[string]string{}
	for _, kv := range s.Exec.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
		if k == s.Exec.APIKeyEnv {
			t.Errorf("exec.env sets %s: the key must arrive through localai.api_key_ref only", k)
		}
	}
	addr := env["LOCALAI_ADDRESS"]
	if host, _, err := net.SplitHostPort(addr); err != nil || host != "127.0.0.1" {
		t.Errorf("LOCALAI_ADDRESS = %q, want a 127.0.0.1 address", addr)
	}
	if want := "http://" + addr; svc.LocalAI.Endpoint != want || lai.LocalAI.Endpoint != want {
		t.Errorf("endpoints: alixai.yaml %q, localai.yaml %q, want both %q", svc.LocalAI.Endpoint, lai.LocalAI.Endpoint, want)
	}
	if !slices.Equal(svc.LocalAI.QualifiedVersions, lai.LocalAI.QualifiedVersions) || len(lai.LocalAI.QualifiedVersions) == 0 {
		t.Errorf("qualified_versions differ: %v and %v", svc.LocalAI.QualifiedVersions, lai.LocalAI.QualifiedVersions)
	}
	for _, off := range []string{"LOCALAI_DISABLE_WEBUI", "LOCALAI_DISABLE_GALLERY_ENDPOINT", "LOCALAI_DISABLE_MCP", "LOCALAI_DISABLE_AGENTS"} {
		if env[off] != "true" {
			t.Errorf("%s = %q, want true (deploy/container/localai/hardening.md)", off, env[off])
		}
	}
	if env["LOCALAI_P2P"] != "false" || env["LOCALAI_AUTH"] != "false" {
		t.Errorf("LOCALAI_P2P = %q, LOCALAI_AUTH = %q, want both false", env["LOCALAI_P2P"], env["LOCALAI_AUTH"])
	}
}

// The LocalAI unit's configuration loads and validates under the real section set, and the exec command refuses the
// AlixAI service's own configuration: that one only attaches.
func TestLocalAIUnitConfigLoadsAndExecRefusesTheServiceConfig(t *testing.T) {
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		env := &cli.Env{Stdout: &out, Stderr: &errOut, LookupEnv: func(string) (string, bool) { return "", false }}
		code := cli.Run(context.Background(), args, env)
		return code, out.String() + errOut.String()
	}
	if code, out := run("config", "check", "-config", "systemd/localai.yaml"); code != cli.ExitOK {
		t.Fatalf("config check = %d\n%s", code, out)
	}
	if code, out := run("supervisor", "exec", "-config", "systemd/alixai.yaml"); code == cli.ExitOK {
		t.Fatalf("supervisor exec ran with the AlixAI service's configuration\n%s", out)
	}
}
