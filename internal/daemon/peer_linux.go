//go:build linux

package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the connecting process's user id via SO_PEERCRED (§3.5).
//
// The kernel fills this in at connect time from the peer's real credentials, so
// unlike anything the peer sends it cannot be forged. Returns ok=false on a
// platform or connection type where it does not apply, which the caller treats
// as "fall back to the file mode" rather than as a denial.
func peerUID(c net.Conn) (uid uint32, ok bool, err error) {
	uc, isUnix := c.(*net.UnixConn)
	if !isUnix {
		return 0, false, nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false, fmt.Errorf("syscall conn: %w", err)
	}

	var cred *unix.Ucred
	var credErr error
	if cerr := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); cerr != nil {
		return 0, false, fmt.Errorf("control: %w", cerr)
	}
	if credErr != nil {
		return 0, false, fmt.Errorf("SO_PEERCRED: %w", credErr)
	}
	return cred.Uid, true, nil
}
