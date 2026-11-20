//go:build windows

package main

import (
	"os"
	"syscall"
)

// isTerminal reports whether f is a console. Windows calls the NUL device a
// character device too, so only asking the console itself is reliable.
func isTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}
