//go:build unix

package queryexec

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(command *exec.Cmd) { command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func terminateProcessGroup(pid int)     { _ = syscall.Kill(-pid, syscall.SIGTERM) }
func killProcessGroup(pid int)          { _ = syscall.Kill(-pid, syscall.SIGKILL) }
