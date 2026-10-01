//go:build windows

package agent

import (
	"os/exec"
	"time"
)

// killGroupOnCancel cannot reach grandchildren here; WaitDelay at least stops waiting on their pipes.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = time.Second
}
