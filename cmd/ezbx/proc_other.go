//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// prepareTree starts the command in its own process group, so killTree can
// end it together with anything it started.
func prepareTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree ends the whole process group.
func killTree(p *os.Process) error {
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return p.Kill()
	}
	return nil
}
