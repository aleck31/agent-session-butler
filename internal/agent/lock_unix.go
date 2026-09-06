//go:build !windows

package agent

import "syscall"

// pidAlive reports whether a process with the given PID is currently running.
// signal 0 is an existence check — it doesn't actually signal the process.
// A crashed agent leaves a stale lock, so we verify the PID rather than
// trusting mere file existence.
//
// pid <= 0 is rejected up front: Kill treats 0 as "my whole process group" and
// negatives as a group/broadcast, all of which succeed and would report a
// malformed lock file as live — permanently blocking the session from cleanup.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
