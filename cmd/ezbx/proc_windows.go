//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// prepareTree is a no-op on Windows. The tree is found by parent process
// when it is killed.
func prepareTree(*exec.Cmd) {}

// killTree ends a process and everything it started. An engine's command is
// often a .cmd shim, and killing only cmd.exe would leave the real program
// running. taskkill is called by its full path so nothing on PATH can stand
// in for it.
func killTree(p *os.Process) error {
	taskkill := filepath.Join(os.Getenv("SystemRoot"), "System32", "taskkill.exe")
	if err := exec.Command(taskkill, "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run(); err != nil {
		return p.Kill()
	}
	return nil
}
