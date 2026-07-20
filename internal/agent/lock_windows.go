//go:build windows

package agent

import "golang.org/x/sys/windows"

// pidAlive reports whether a process with the given PID is currently running.
// Opens the process with minimal rights; a failure (typically the PID is gone)
// means not alive. A crashed agent leaves a stale lock, so we verify the PID
// rather than trusting mere file existence.
func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	// STILL_ACTIVE (259) means the process has not exited.
	return code == 259
}
