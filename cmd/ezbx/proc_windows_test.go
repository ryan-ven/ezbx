//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A .cmd shim that times out must not leave the program it started running.
// The shim starts a second cmd.exe that waits about 4 seconds and then writes
// a marker file. If only the shim is killed, the marker still appears.
func TestTimeoutEndsTheWholeProcessTree(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "still-running.txt")
	shim := filepath.Join(dir, "engine.cmd")
	script := "@echo off\r\ncmd /c \"ping -n 5 127.0.0.1 >nul & echo late > \"" + marker + "\"\"\r\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := runCommandFor(context.Background(), []string{shim}, 500*time.Millisecond); err == nil {
		t.Fatal("the command should have timed out")
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("runCommandFor took %v to give up", took)
	}

	time.Sleep(6 * time.Second) // longer than the child's own wait
	if _, err := os.Stat(marker); err == nil {
		t.Error("the shim's child kept running after the time limit and wrote its marker")
	}
}
