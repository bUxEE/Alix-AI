//go:build !linux

package native

import "net"

// peerCreds: only Linux reports the peer of a Unix socket here; elsewhere the
// application key is the only control.
func peerCreds(net.Conn) (PeerCreds, bool) { return PeerCreds{}, false }
