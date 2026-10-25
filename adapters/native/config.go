package native

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Transport is the transport name the adapter reports in Describe.
const Transport = "native"

// The networks an endpoint can use.
const (
	// NetworkUnix is a Unix domain socket (AF_UNIX; Windows 10 and later too).
	NetworkUnix = "unix"
	// NetworkTCP is the loopback TCP fallback: 127.0.0.1, [::1] or localhost.
	NetworkTCP = "tcp"
)

// Defaults and bounds.
const (
	// DefaultHandshakeTimeout bounds opening a session (the two-round handshake)
	// when the caller's context has no earlier deadline.
	DefaultHandshakeTimeout = 5 * time.Second
	// DefaultMaxRequestBytes bounds the frame of one request.
	DefaultMaxRequestBytes = 1 << 20
	// envelopeSlack is what the JSON-RPC envelope adds to a result.
	envelopeSlack = 4 << 10
	// maxSocketPath is the longest Unix socket path every supported system binds.
	maxSocketPath = 103
	// MinKeyBytes is the shortest application key accepted.
	MinKeyBytes = 32

	maxEndpoints    = 16
	maxCapabilities = 1024
)

var (
	methodRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	endpointIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	keyIDRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	argumentRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Endpoint is one bridge: where it listens, and the application key that proves
// both sides to each other.
type Endpoint struct {
	// ID is the endpoint's ID in the manifest.
	ID string
	// Network is NetworkUnix or NetworkTCP.
	Network string
	// Address is an absolute socket path, or a loopback host and port
	// (127.0.0.1:7001, [::1]:7001, localhost:7001). Nothing else is reachable:
	// the adapter never dials a host that is not loopback.
	Address string
	// KeyID names the application key on the wire, so the bridge can hold several
	// (rotation) and revoke one.
	KeyID string
	// Key is the application key, at least MinKeyBytes bytes. It never leaves the
	// process: only HMACs of the handshake transcript do.
	Key Secret
	// Provisioning are the management operations the adapter configuration
	// approves for this endpoint (the audit's create_tenant, seed, cleanup). They
	// are not capability operations: only Manage reaches them.
	Provisioning []string
	// PeerCheck, when set, decides whether the operating system's account of the
	// bridge process (SO_PEERCRED, Linux Unix sockets) is acceptable. It is not
	// called where the system gives none; the key is the control there.
	PeerCheck func(PeerCreds) error
}

// Binding is one operation of a capability mapped to one method of one bridge.
type Binding struct {
	// Endpoint is the ID of the endpoint that owns the method.
	Endpoint string
	// Method is the bridge method's exact name (native_method of the package).
	Method string
	// Timeout bounds one call; 0 leaves the caller's deadline.
	Timeout time.Duration
	// Arguments renames capability fields to the argument names the method takes,
	// for the fields that differ.
	Arguments map[string]string
}

// Capability is what a package maps for one capability.
type Capability struct {
	// ID is the capability ID.
	ID string
	// Operations maps adapter operations to bridge methods.
	Operations map[adapters.Operation]Binding
	// Resolve maps an entity type to the method that resolves a mention of it. A
	// Read whose fields carry the reserved argument entity_type goes here and
	// nowhere else.
	Resolve map[string]Binding
}

// Config configures New.
type Config struct {
	// ApplicationID names the application, for logs only; the scope check is
	// adapters.Wrap's.
	ApplicationID string
	// Endpoints are the bridges. At least one.
	Endpoints []Endpoint
	// Capabilities is the package's mapping. Only what is mapped here can ever be
	// called.
	Capabilities []Capability
	// Dialer makes every loopback TCP connection; nil refuses them all (default
	// deny). Unix sockets are not network egress and do not use it.
	Dialer *egress.Dialer
	// MaxResponseBytes bounds one result; 0 means adapters.DefaultMaxResponseBytes.
	// A reply frame may be 4 KiB larger (the JSON-RPC envelope).
	MaxResponseBytes int64
	// MaxRequestBytes bounds one request frame; 0 means DefaultMaxRequestBytes.
	MaxRequestBytes int64
	// HandshakeTimeout bounds the handshake; 0 means DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// Logger receives refusals (a bridge that failed authentication, a socket
	// with loose permissions). It never receives a key, a proof or a body. nil
	// discards.
	Logger *slog.Logger
}

// ConfigError is the cause of the validation_failed error New returns for a
// configuration it refuses.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return "native adapter config: " + e.Msg }

func cfgErr(format string, args ...any) error {
	return contracts.Wrap(contracts.ErrValidationFailed, &ConfigError{Msg: fmt.Sprintf(format, args...)}, "the native adapter configuration is invalid")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func reservedMethod(m string) bool {
	return strings.HasPrefix(m, "session.") || strings.HasPrefix(m, "alixai.")
}

// validate checks a Config and fills its defaults.
func (c *Config) validate() error {
	if c.ApplicationID == "" {
		return cfgErr("ApplicationID is empty")
	}
	if n := len(c.Endpoints); n == 0 || n > maxEndpoints {
		return cfgErr("between 1 and %d endpoints are required", maxEndpoints)
	}
	if len(c.Capabilities) > maxCapabilities {
		return cfgErr("more than %d capabilities", maxCapabilities)
	}
	byID := map[string]*Endpoint{}
	for i := range c.Endpoints {
		ep := &c.Endpoints[i]
		if !endpointIDRE.MatchString(ep.ID) {
			return cfgErr("endpoint %d has an empty or malformed ID", i)
		}
		if byID[ep.ID] != nil {
			return cfgErr("endpoint %q is declared twice", ep.ID)
		}
		byID[ep.ID] = ep
		if err := checkAddress(ep.Network, ep.Address); err != nil {
			return cfgErr("endpoint %q: %v", ep.ID, err)
		}
		if !keyIDRE.MatchString(ep.KeyID) {
			return cfgErr("endpoint %q: the key id is empty or malformed", ep.ID)
		}
		if n := len(ep.Key.value()); n < MinKeyBytes {
			return cfgErr("endpoint %q: the application key is missing or shorter than %d bytes", ep.ID, MinKeyBytes)
		}
		seen := map[string]bool{}
		for _, p := range ep.Provisioning {
			if !methodRE.MatchString(p) || seen[p] {
				return cfgErr("endpoint %q: provisioning operation %q is malformed or repeated", ep.ID, truncate(p, 40))
			}
			seen[p] = true
		}
	}
	checkBinding := func(where string, b Binding) error {
		switch {
		case byID[b.Endpoint] == nil:
			return cfgErr("%s maps a method of the unknown endpoint %q", where, truncate(b.Endpoint, 40))
		case !methodRE.MatchString(b.Method) || reservedMethod(b.Method):
			return cfgErr("%s maps the method name %q, which is malformed or reserved", where, truncate(b.Method, 40))
		case b.Timeout < 0:
			return cfgErr("%s has a negative timeout", where)
		}
		targets := map[string]bool{}
		for from, to := range b.Arguments {
			if !argumentRE.MatchString(from) || !argumentRE.MatchString(to) || targets[to] {
				return cfgErr("%s renames argument %q to %q, which is malformed or used twice", where, truncate(from, 40), truncate(to, 40))
			}
			targets[to] = true
		}
		return nil
	}
	seen := map[string]bool{}
	for _, cp := range c.Capabilities {
		if cp.ID == "" || strings.ContainsAny(cp.ID, "* \t\r\n") || seen[cp.ID] {
			return cfgErr("a capability ID is empty, malformed or declared twice")
		}
		seen[cp.ID] = true
		for op, b := range cp.Operations {
			if _, ok := op.Result(); !ok {
				return cfgErr("capability %q maps %q, which is not an operation that returns a result", cp.ID, op)
			}
			if err := checkBinding("capability "+cp.ID+" "+string(op), b); err != nil {
				return err
			}
		}
		for typ, b := range cp.Resolve {
			if typ == "" || strings.ContainsAny(typ, "* \t\r\n") {
				return cfgErr("capability %q resolves a malformed entity type", cp.ID)
			}
			if err := checkBinding("capability "+cp.ID+" resolver of "+typ, b); err != nil {
				return err
			}
		}
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = adapters.DefaultMaxResponseBytes
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return nil
}

// checkAddress accepts the two local forms and nothing else.
func checkAddress(network, address string) error {
	switch network {
	case NetworkUnix:
		if !absoluteSocketPath(address) || len(address) > maxSocketPath || strings.ContainsRune(address, 0) {
			return fmt.Errorf("a Unix socket path must be absolute and at most %d bytes", maxSocketPath)
		}
	case NetworkTCP:
		_, _, err := loopbackAddress(address)
		return err
	default:
		return fmt.Errorf("the network %q is not %q or %q", network, NetworkUnix, NetworkTCP)
	}
	return nil
}

// loopbackAddress splits host:port and requires the host to be loopback: a
// package cannot point the adapter at another machine.
func loopbackAddress(address string) (host string, port int, err error) {
	h, p, serr := net.SplitHostPort(address)
	if serr != nil {
		return "", 0, errors.New("a TCP address must be host:port")
	}
	port, perr := strconv.Atoi(p)
	if perr != nil || port < 1 || port > 65535 {
		return "", 0, errors.New("the TCP port is out of range")
	}
	ip := net.ParseIP(h)
	if !(strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback())) {
		return "", 0, errors.New("a TCP address must be loopback (127.0.0.1, [::1] or localhost)")
	}
	return strings.ToLower(h), port, nil
}

// AdapterConfig is the content of the adapter configuration file of a native
// endpoint (the file the manifest's adapter_config names). It is deliberately
// small and refuses unknown members, so a setting a newer package relies on is
// never silently ignored. It never holds the application key: only a reference
// to where the operator keeps it.
//
//	server: ledger-bridge       # optional; when present it must be the endpoint's ID
//	key:
//	  id: k1                    # the key id the bridge knows the key by
//	  ref: file:/etc/alixai/keys/ledger.key   # env:NAME or file:ABSOLUTE_PATH
//	peer_uid: 1000              # optional; with OS peer credentials the bridge must run as this user
//	management:
//	  provisioning: [create_tenant, seed, cleanup]   # the audit's disposable-tenant operations
type AdapterConfig struct {
	Server string `yaml:"server"`
	Key    struct {
		ID  string `yaml:"id"`
		Ref string `yaml:"ref"`
	} `yaml:"key"`
	PeerUID    *int `yaml:"peer_uid"`
	Management struct {
		Provisioning []string `yaml:"provisioning"`
	} `yaml:"management"`
}

// ParseAdapterConfig reads an adapter configuration for the endpoint endpointID.
// It is strict: unknown members, a second document, a server that is not the
// endpoint, a missing or malformed key id or reference, a key that is not a
// reference, a malformed or repeated provisioning name are errors.
func ParseAdapterConfig(raw []byte, endpointID string) (AdapterConfig, error) {
	var c AdapterConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return AdapterConfig{}, cfgErr("the adapter configuration of endpoint %q does not parse: %s", endpointID, yamlReason(err))
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return AdapterConfig{}, cfgErr("the adapter configuration of endpoint %q holds more than one document", endpointID)
	}
	if c.Server != "" && c.Server != endpointID {
		return AdapterConfig{}, cfgErr("the adapter configuration names server %q but configures endpoint %q", truncate(c.Server, 40), endpointID)
	}
	if !keyIDRE.MatchString(c.Key.ID) {
		return AdapterConfig{}, cfgErr("key.id is missing or malformed")
	}
	if err := CheckKeyRef(c.Key.Ref); err != nil {
		return AdapterConfig{}, cfgErr("key.ref: %v", err)
	}
	if c.PeerUID != nil && *c.PeerUID < 0 {
		return AdapterConfig{}, cfgErr("peer_uid is negative")
	}
	seen := map[string]bool{}
	for _, p := range c.Management.Provisioning {
		if !methodRE.MatchString(p) || seen[p] {
			return AdapterConfig{}, cfgErr("management.provisioning holds the malformed or repeated name %q", truncate(p, 40))
		}
		seen[p] = true
	}
	sort.Strings(c.Management.Provisioning)
	return c, nil
}

// yamlReason keeps a YAML error short and free of the document's own text.
func yamlReason(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 160)
}

// absoluteSocketPath reports whether p is an absolute socket path. The manifest
// writes a Unix socket as /path, which on Windows is rooted on the current drive
// without being absolute to Go's filepath, so both forms count.
func absoluteSocketPath(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/")
}
