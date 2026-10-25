package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// CoreConfig holds the sections every AlixAI process has. Module-owned sections are
// registered by their modules and are not part of this struct.
type CoreConfig struct {
	DataDir DataDir
	Server  ServerConfig
	Log     LogConfig
}

// Lite-profile defaults.
const (
	DefaultListen   = "127.0.0.1:7470"
	DefaultDataDir  = "data"
	DefaultLogLevel = "info"
)

// DataDir is the `data_dir` section: the directory under which the database,
// document index and every other piece of local state lives. A relative path
// is relative to the working directory of the process. It is never created
// by config loading.
type DataDir string

// String returns the directory as a plain string.
func (d DataDir) String() string { return string(d) }

// Validate implements the section validation hook.
func (d DataDir) Validate() error {
	if strings.TrimSpace(string(d)) == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsRune(string(d), 0) {
		return fmt.Errorf("must not contain NUL")
	}
	return nil
}

// ServerConfig is the `server` section.
//
// Validation here is syntactic only. Whether a non-loopback Listen address is
// acceptable (it requires TLS) is a policy the gateway enforces when it binds;
// the default is loopback.
type ServerConfig struct {
	// Listen is the TCP listen address as host:port.
	Listen string `yaml:"listen"`
	// UnixSocket is an optional unix domain socket path for local IPC.
	UnixSocket string `yaml:"unix_socket"`
}

// Validate implements the section validation hook.
func (c ServerConfig) Validate() error {
	if err := validateHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	if strings.ContainsRune(c.UnixSocket, 0) {
		return fmt.Errorf("unix_socket must not contain NUL")
	}
	if c.UnixSocket != "" && strings.TrimSpace(c.UnixSocket) == "" {
		return fmt.Errorf("unix_socket must not be blank")
	}
	return nil
}

func validateHostPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("want host:port")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("port %q is not a number in 0-65535", port)
	}
	if host == "" {
		return nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if !validHostname(host) {
		return fmt.Errorf("host %q is not an IP address or hostname", host)
	}
	return nil
}

func validHostname(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			if !ok {
				return false
			}
		}
	}
	return true
}

// LogConfig is the `log` section.
type LogConfig struct {
	// Level is debug, info, warn or error (case-insensitive).
	Level string `yaml:"level"`
	// RedactKeys lists extra attribute keys whose values are always redacted,
	// on top of the built-in token/secret/password/... matching. Matching is
	// case-insensitive and ignores "_" and "-".
	RedactKeys []string `yaml:"redact_keys"`
}

// SlogLevel parses Level.
func (c LogConfig) SlogLevel() (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(c.Level)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("level %q: want debug, info, warn or error", c.Level)
}

// Validate implements the section validation hook.
func (c LogConfig) Validate() error {
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	for i, k := range c.RedactKeys {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("redact_keys[%d] must not be blank", i)
		}
	}
	return nil
}

func coreDefaults() CoreConfig {
	return CoreConfig{
		DataDir: DefaultDataDir,
		Server:  ServerConfig{Listen: DefaultListen},
		Log:     LogConfig{Level: DefaultLogLevel},
	}
}
