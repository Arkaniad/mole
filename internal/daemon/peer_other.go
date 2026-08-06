//go:build !linux

package daemon

import "net"

// peerUID is unavailable on this platform.
//
// Reported as unsupported rather than as an error, so the daemon still runs with
// the socket's file permissions as its control. Refusing every connection on a
// platform without SO_PEERCRED would be a denial dressed as security.
func peerUID(net.Conn) (uid uint32, ok bool, err error) { return 0, false, nil }
