//go:build !unix

package gocompile

import "os/exec"

func configureCompileCommand(command *exec.Cmd) {
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Kill()
	}
}
