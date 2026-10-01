//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
	"time"
)

// killGroupOnCancel gives the command its own process group and kills the whole group on timeout:
// wrappers such as the toolbox shim run the real tool as a child, which killing the wrapper misses.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
}
