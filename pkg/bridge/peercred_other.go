//go:build !linux

package bridge

import "net"

// peerCredentials: only Linux reports the peer of a Unix socket here; elsewhere
// the application key is the only control.
func peerCredentials(net.Conn) (PeerCredentials, bool) { return PeerCredentials{}, false }
