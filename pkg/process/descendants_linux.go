//go:build linux

package process

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func prepareDescendants() error { return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) }

// After the unreaped leader exits, detached descendants are adopted here. This
// guardian has no other child waiters, so their PIDs remain owned until reaped.
func stopDescendants(leader int) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		tasks, err := os.ReadDir("/proc/self/task")
		if err != nil {
			return err
		}
		children := map[int]bool{}
		for _, task := range tasks {
			data, err := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "children"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			for _, field := range strings.Fields(string(data)) {
				pid, err := strconv.Atoi(field)
				if err == nil && pid > 0 && pid != leader {
					children[pid] = true
				}
			}
		}
		if len(children) == 0 {
			return nil
		}
		for pid := range children {
			if err = unix.Kill(pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
				return err
			}
			var status unix.WaitStatus
			for {
				_, err = unix.Wait4(pid, &status, unix.WNOHANG, nil)
				if err == unix.EINTR {
					continue
				}
				break
			}
			if err != nil && err != unix.ECHILD {
				return err
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("detached descendants have not stopped")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
