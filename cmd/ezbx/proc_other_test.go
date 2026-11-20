//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A command that times out must not leave a child it started running. The
// script starts a background child that writes a marker after 3 seconds.
func TestTimeoutEndsTheWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "still-running.txt")
	script := filepath.Join(dir, "engine.sh")
	body := "#!/bin/sh\n(sleep 3; echo late > '" + marker + "') &\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := runCommandFor(context.Background(), []string{script}, 500*time.Millisecond); err == nil {
		t.Fatal("the command should have timed out")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("runCommandFor took %v to give up", took)
	}

	time.Sleep(4 * time.Second) // longer than the child's own wait
	if _, err := os.Stat(marker); err == nil {
		t.Error("the script's child kept running after the time limit and wrote its marker")
	}
}
