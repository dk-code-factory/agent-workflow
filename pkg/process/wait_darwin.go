//go:build darwin

package process

import "golang.org/x/sys/unix"

func waitLeaderExit(pid int) error {
	queue, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(queue)
	event := unix.Kevent_t{}
	unix.SetKevent(&event, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	event.Fflags = unix.NOTE_EXIT
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(queue, []unix.Kevent_t{event}, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err == unix.ESRCH {
			return nil
		}
		if err != nil {
			return err
		}
		if n > 0 && events[0].Flags&unix.EV_ERROR != 0 {
			code := unix.Errno(events[0].Data)
			if code == unix.ESRCH {
				return nil
			}
			return code
		}
		if n > 0 {
			return nil
		}
	}
}
