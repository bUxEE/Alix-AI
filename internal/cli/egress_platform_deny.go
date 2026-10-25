package cli

import (
	"net"
	"sync"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/metrics"
)

// The platform deny-list of the connector dialers (PT-001, master plan 16.1).
//
// A package declares the loopback port of its own application, and the transports put that port
// on the connector's allowlist. Nothing in a package may turn that into a way to reach the platform:
// the gateway's listen address, the operator admin interface, the metrics listener and the LocalAI
// base URL are refused by every dialer the transports build, even when the allowlist names the very
// host and port. The list is filled from the running configuration on every dial, so a configuration
// that is loaded after a transport was built, or an admin interface that binds its ephemeral port later
// (it writes its address to the endpoint file of the data directory), is denied from the moment it
// exists. platformDenyAddListener adds an address the configuration cannot know.
var platformDenyList = func() *egress.PlatformDeny {
	p := egress.NewPlatformDeny()
	p.AddSource(platformDenyConfigured)
	p.AddSource(platformDenyExtraAddrs)
	return p
}()

// platformDenyOption is the dialer option every transport adds.
func platformDenyOption() egress.Option { return egress.WithPlatformDeny(platformDenyList) }

var platformDenyExtra struct {
	mu    sync.Mutex
	addrs []string
}

// platformDenyAddListener denies the host:port of a listener the process bound (serve calls it for
// each listener it opens, through lastWireDenyListener, and lifts it when serve closes the listener).
func platformDenyAddListener(hostPort string) {
	if hostPort == "" {
		return
	}
	platformDenyExtra.mu.Lock()
	defer platformDenyExtra.mu.Unlock()
	for _, a := range platformDenyExtra.addrs {
		if a == hostPort {
			return
		}
	}
	platformDenyExtra.addrs = append(platformDenyExtra.addrs, hostPort)
}

// lastWireDenyListener denies the address a listener serve bound, as it is bound (an ephemeral
// port is known only now), and returns the function that lifts the denial once serve has closed the
// listener: the port is then the operating system's to hand to anyone, and a later listener of the
// process (a test, a second serve) must not inherit a refusal meant for a service that is gone.
func lastWireDenyListener(ln net.Listener) (release func()) {
	if ln == nil {
		return func() {}
	}
	addr := ln.Addr().String()
	platformDenyAddListener(addr)
	return func() { platformDenyRemoveListener(addr) }
}

func platformDenyRemoveListener(hostPort string) {
	platformDenyExtra.mu.Lock()
	defer platformDenyExtra.mu.Unlock()
	for i, a := range platformDenyExtra.addrs {
		if a == hostPort {
			platformDenyExtra.addrs = append(platformDenyExtra.addrs[:i:i], platformDenyExtra.addrs[i+1:]...)
			return
		}
	}
}

func platformDenyExtraAddrs() []string {
	platformDenyExtra.mu.Lock()
	defer platformDenyExtra.mu.Unlock()
	return append([]string(nil), platformDenyExtra.addrs...)
}

// platformDenyAdminCacheTTL bounds how often the admin endpoint file is read: a dial is frequent, a
// file read is not free, and a new address is denied within this delay of being written.
const platformDenyAdminCacheTTL = time.Second

// platformDenyAdminURL reads the base URL of the running admin interface from the endpoint file of the
// data directory. Tests replace it.
var platformDenyAdminURL = func(dataDir string) string {
	ep, err := gateway.ReadAdminEndpoint(dataDir)
	if err != nil {
		return ""
	}
	return ep.URL
}

var platformDenyAdminCache struct {
	mu  sync.Mutex
	dir string
	at  time.Time
	url string
}

func platformDenyAdmin(dataDir string) string {
	c := &platformDenyAdminCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir == dataDir && !c.at.IsZero() && time.Since(c.at) < platformDenyAdminCacheTTL {
		return c.url
	}
	c.dir, c.at, c.url = dataDir, time.Now(), platformDenyAdminURL(dataDir)
	return c.url
}

// platformDenyConfigured lists the destinations the configuration names: the gateway listen address,
// the metrics listener (denied whether or not it is enabled, because a package must not be able to
// probe the port an operator may switch on), the LocalAI base URL and the admin interface.
func platformDenyConfigured() []string {
	core := config.Core()
	out := []string{core.Server.Listen, metrics.FromConfig().Listen}
	ep := localaiSection.Endpoint
	if ep == "" {
		ep = localai.DefaultEndpoint
	}
	out = append(out, ep)
	if u := platformDenyAdmin(core.DataDir.String()); u != "" {
		out = append(out, u)
	}
	return out
}
