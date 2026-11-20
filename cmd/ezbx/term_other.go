//go:build !windows

package main

import "os"

// isTerminal reports whether f is a terminal. /dev/null is a character
// device too, so it is ruled out by name.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(st, null) {
		return false
	}
	return true
}
