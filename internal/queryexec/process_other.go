//go:build !unix

package queryexec

import (
	"os"
	"os/exec"
)

func setProcessGroup(command *exec.Cmd) {}
func terminateProcessGroup(pid int)     { killProcessGroup(pid) }
func killProcessGroup(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}
