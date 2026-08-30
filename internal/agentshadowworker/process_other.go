//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package agentshadowworker

import (
	"os"
	"os/exec"
)

func configureProcessGroup(_ *exec.Cmd) {}

func terminateProcessGroup(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}

func killProcessGroup(pid int) { terminateProcessGroup(pid) }
