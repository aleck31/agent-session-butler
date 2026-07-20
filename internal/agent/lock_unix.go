//go:build !windows

package agent

import "syscall"

// pidAlive reports whether a process with the given PID is currently running.
// signal 0 is an existence check — it doesn't actually signal the process.
// A crashed agent leaves a stale lock, so we verify the PID rather than
// trusting mere file existence.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
