package ezbx

import (
	"io/fs"
	"os/exec"
	"strings"
	"testing"
)

// Nothing git ignores may be carried inside the program: those are private
// files (agent notes, local configs, keys). This needs a git checkout, so it
// is skipped elsewhere.
func TestSourceHoldsNoIgnoredFiles(t *testing.T) {
	var files []string
	err := fs.WalkDir(Source, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"go.mod", "source.go", "docker/box.Dockerfile", "docker/gateway.Dockerfile", "harnesses/pi/package-lock.json", "catalog/packs/coding.json"} {
		found := false
		for _, f := range files {
			found = found || f == must
		}
		if !found {
			t.Errorf("%s is not carried", must)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		t.Skip("not a git checkout")
	}
	cmd := exec.Command("git", "check-ignore", "--no-index", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(files, "\n") + "\n")
	out, err := cmd.Output()
	// check-ignore exits 1 when nothing is ignored.
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Errorf("git-ignored files are carried inside the program:\n%s", out)
}
