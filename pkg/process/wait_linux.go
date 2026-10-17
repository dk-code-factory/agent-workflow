//go:build linux

package process

import "golang.org/x/sys/unix"

// Observe exit without reaping: the leader PID remains reserved until its
// entire process group has been signaled, so it cannot identify another group.
func waitLeaderExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		return err
	}
}
