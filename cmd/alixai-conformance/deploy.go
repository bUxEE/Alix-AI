package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/cli"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/trust"
)

// The audit and the conversation stage run in a throwaway deployment: a data directory
// and a configuration file in a temporary directory, an audit-signing key generated for
// this run and registered under alixai.platform, and the package installed into it. It
// is what an operator does by hand with the real commands, run in-process through
// cli.Run, so the audit that gates a package in production is the audit that runs here
// (every transport, the provisioning bridge, the RAG checks). Nothing outlives the run.
const (
	conformanceOperator = "alixai-conformance"
	// conformanceDeployment is the deployment id of the throwaway deployment.
	conformanceDeployment = "dep_conformance"
	conformanceAudience   = "alixai"
	platformSignerID      = "conformance-audit-signer"
)

// cliMu serialises the in-process command line: the configuration registry and the
// sections it fills are process-wide, as in the real binary (one process per command).
var cliMu sync.Mutex

// cliResult is what one command did.
type cliResult struct {
	Code   int
	Stdout string
	Stderr string
}

// deployment is the throwaway deployment of one run.
type deployment struct {
	root    string
	dataDir string
	cfgPath string
	// publisher signs the copy of the package that is installed (the original's signature
	// is checked by the package stage; the copy differs from it in the endpoints it is
	// pointed at, so it cannot carry the original's).
	publisher trust.SigningKey
	// publisherPublic is the public half, base64.
	publisherPublic string
}

// newDeployment creates the deployment under root and registers the audit-signing key.
func newDeployment(ctx context.Context, root string) (*deployment, error) {
	d := &deployment{root: root, dataDir: filepath.Join(root, "data")}
	if err := os.MkdirAll(d.dataDir, 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := trust.GenerateKey()
	if err != nil {
		return nil, err
	}
	privPath, pubPath, err := trust.WriteKeyFiles(root, platformSignerID, priv)
	if err != nil {
		return nil, err
	}
	d.cfgPath = filepath.Join(root, "alixai.yaml")
	cfg := fmt.Sprintf(`data_dir: %q
identity:
  deployment_id: %s
  audience: %s
integration_audit:
  signing_key_id: %s
  signing_key_file: %q
  accept_operators: []
`, filepath.ToSlash(d.dataDir), conformanceDeployment, conformanceAudience, platformSignerID, filepath.ToSlash(privPath))
	if err := os.WriteFile(d.cfgPath, []byte(cfg), 0o600); err != nil {
		return nil, err
	}
	if r := d.run(ctx, "trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", platformSignerID,
		"-public-key-file", pubPath, "-operator", conformanceOperator); r.Code != cli.ExitOK {
		return nil, fmt.Errorf("register the audit-signing key: %s", firstNonEmpty(r.Stderr, r.Stdout))
	}
	_ = pub

	ppub, ppriv, err := trust.GenerateKey()
	if err != nil {
		return nil, err
	}
	d.publisher = trust.SigningKey{Key: ppriv}
	d.publisherPublic = base64.StdEncoding.EncodeToString(ppub)
	return d, nil
}

// run executes one command of the real binary in-process against this deployment.
func (d *deployment) run(ctx context.Context, args ...string) cliResult {
	return runCLI(ctx, d.cfgPath, args...)
}

// runCLI executes one `alixai` command in-process. A configuration path is added after
// the two words of the command's name unless it is empty.
func runCLI(ctx context.Context, cfgPath string, args ...string) cliResult {
	cliMu.Lock()
	defer cliMu.Unlock()
	full := args
	if cfgPath != "" && len(args) >= 2 {
		full = append(append(append([]string{}, args[:2]...), "-config", cfgPath), args[2:]...)
	}
	var out, errb bytes.Buffer
	code := cli.Run(ctx, full, &cli.Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		LookupEnv: func(string) (string, bool) { return "", false },
		Config:    config.Default,
	})
	return cliResult{Code: code, Stdout: out.String(), Stderr: errb.String()}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

// installed is what `alixai package install` prints.
type installed struct {
	Created      bool   `json:"created"`
	Application  string `json:"application_id"`
	ManifestHash string `json:"manifest_hash"`
	State        string `json:"state"`
	Path         string `json:"path"`
}

// install puts a package directory into the deployment, in quarantine, where every
// package starts and where the audit looks at it.
func (d *deployment) install(ctx context.Context, dir, publisherKeyID, applicationID string) (installed, error) {
	if r := d.run(ctx, "trust", "add-key", "-app", applicationID, "-key-id", publisherKeyID,
		"-public-key", d.publisherPublic, "-operator", conformanceOperator); r.Code != cli.ExitOK {
		return installed{}, fmt.Errorf("register the publisher key: %s", firstNonEmpty(r.Stderr, r.Stdout))
	}
	r := d.run(ctx, "package", "install", "-operator", conformanceOperator, dir)
	if r.Code != cli.ExitOK {
		return installed{}, fmt.Errorf("install the package: %s", firstNonEmpty(r.Stderr, r.Stdout))
	}
	var out installed
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		return installed{}, fmt.Errorf("install the package: the output is not JSON: %w", err)
	}
	if out.State != "quarantined" {
		return installed{}, fmt.Errorf("an installed package is quarantined until an audit activates it, got %q", out.State)
	}
	return out, nil
}

// ---- the copy that is installed ----

// prepareCopy copies the package into dir, points the endpoints named in targets at the
// application under test, and signs the copy with the run's publisher key under keyID.
// Nothing else of the package changes: its capabilities, risk levels, fields, policy and
// conversation contracts are the author's.
func (d *deployment) prepareCopy(src, dir string, applicationID, packageVersion, keyID string, targets map[string]string) error {
	if err := copyPackage(src, dir); err != nil {
		return err
	}
	if len(targets) > 0 {
		if err := retarget(filepath.Join(dir, "alixai.yaml"), targets); err != nil {
			return err
		}
	}
	d.publisher.KeyID = keyID
	now := time.Now().UTC().Add(-time.Minute)
	_, err := trust.SignPackageDir(dir, trust.SignOptions{
		PackageIdentity: trust.PackageIdentity{ApplicationID: applicationID, PackageVersion: packageVersion},
		Keys:            []trust.SigningKey{d.publisher},
		Now:             now,
		ExpiresAt:       now.Add(48 * time.Hour),
	})
	if err != nil {
		return fmt.Errorf("sign the copy of the package: %w", err)
	}
	return nil
}

// copyPackage copies a package directory. A symbolic link or anything that is not a
// regular file is refused: the package loader refuses them too, and a copy must not
// follow one out of the directory.
func copyPackage(from, to string) error {
	return filepath.WalkDir(from, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		switch {
		case e.IsDir():
			return os.MkdirAll(dst, 0o700)
		case !e.Type().IsRegular():
			return fmt.Errorf("%s is not a regular file", rel)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}

// retarget rewrites the url of each endpoint named in targets in the manifest at path (the url
// of an mcp endpoint, the base_url of an openapi one, the host:port address of a native one over
// loopback_tcp), and the network.egress entry that allowed the old url's host and port (the
// manifest names the destination twice, and the egress allowlist is built from them).
func retarget(path string, targets map[string]string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("alixai.yaml: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("alixai.yaml is not a mapping")
	}
	root := doc.Content[0]
	endpoints := mapValue(root, "endpoints")
	if endpoints == nil {
		return fmt.Errorf("alixai.yaml declares no endpoints to retarget")
	}
	egress := mapValue(mapValue(root, "network"), "egress")
	for id, target := range targets {
		ep := mapValue(endpoints, id)
		if ep == nil {
			return fmt.Errorf("the package declares no endpoint %q", id)
		}
		u := mapValue(ep, "url") // mcp
		if u == nil {
			u = mapValue(ep, "base_url") // openapi
		}
		if u == nil {
			// native: the bridge's address, host:port, when it is reached over loopback_tcp. It is local
			// IPC, not an egress destination, so there is no allowlist entry to follow it.
			addr, err := nativeAddress(ep, id)
			if err != nil {
				return err
			}
			if _, port, err := net.SplitHostPort(target); err != nil || port == "" {
				return fmt.Errorf("endpoint %q: target %q is not a host:port address", id, target)
			}
			addr.Value = target
			continue
		}
		if u.Kind != yaml.ScalarNode {
			return fmt.Errorf("endpoint %q has no url to retarget", id)
		}
		oldHost, oldPort, err := hostPort(u.Value)
		if err != nil {
			return fmt.Errorf("endpoint %q: %w", id, err)
		}
		newHost, newPort, err := hostPort(target)
		if err != nil {
			return fmt.Errorf("endpoint %q: target: %w", id, err)
		}
		u.Value = target
		if egress == nil {
			continue
		}
		for i := 1; i < len(egress.Content); i += 2 {
			entry := egress.Content[i]
			h, ports := mapValue(entry, "host"), mapValue(entry, "ports")
			if h == nil || ports == nil || h.Value != oldHost {
				continue
			}
			for _, p := range ports.Content {
				if p.Value == strconv.Itoa(oldPort) {
					p.Value = strconv.Itoa(newPort)
					h.Value = newHost
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// nativeAddress returns the address node of a native endpoint reached over loopback_tcp: the one
// kind whose address a run can point at a running application. An endpoint of any other kind has no
// url to retarget.
func nativeAddress(ep *yaml.Node, id string) (*yaml.Node, error) {
	ipc := mapValue(ep, "ipc")
	kind, addr := mapValue(ipc, "kind"), mapValue(ipc, "address")
	if kind == nil || kind.Value != "loopback_tcp" || addr == nil || addr.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("endpoint %q has no url to retarget (a native endpoint can be retargeted only over loopback_tcp)", id)
	}
	return addr, nil
}

// mapValue returns the value node of a key of a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// hostPort splits the host and port of an http(s) url.
func hostPort(raw string) (string, int, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", 0, fmt.Errorf("%q is not a url with a host", raw)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		// no explicit port: the scheme's
		host = u.Hostname()
		switch u.Scheme {
		case "http":
			portText = "80"
		case "https":
			portText = "443"
		default:
			return "", 0, fmt.Errorf("%q has no port", raw)
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%q has no usable port", raw)
	}
	return host, port, nil
}
