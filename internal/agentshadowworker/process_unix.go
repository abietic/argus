//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package agentshadowworker

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessGroup(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil &&
		!errors.Is(err, syscall.ESRCH) {
		return
	}
}

func killProcessGroup(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil &&
		!errors.Is(err, syscall.ESRCH) {
		return
	}
}
