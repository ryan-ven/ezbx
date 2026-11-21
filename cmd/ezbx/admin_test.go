package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/paths"
)

func targetWith(t *testing.T, ws string, dirs paths.Dirs) target {
	t.Helper()
	cfg, err := config.Parse("version = 1\n[box.main]\nworkspace = '" + ws + "'\nharnesses = ['pi']\n")
	if err != nil {
		t.Fatal(err)
	}
	return target{name: "main", cfg: cfg, dirs: dirs}
}

// Destroy removes the state folder only when it can't hold or sit inside
// the workspace.
func TestStateDirFor(t *testing.T) {
	dir := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(dir, "cfg"), State: filepath.Join(dir, "state")}
	got, err := stateDirFor(targetWith(t, filepath.Join(dir, "ws"), dirs))
	if err != nil || got != filepath.Join(dirs.State, "main") {
		t.Fatalf("%s %v", got, err)
	}
	for name, ws := range map[string]string{
		"workspace inside the state folder": filepath.Join(dirs.State, "main", "ws"),
		"state folder inside the workspace": dir,
		"the same folder":                   filepath.Join(dirs.State, "main"),
	} {
		if _, err := stateDirFor(targetWith(t, ws, dirs)); err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A name that would lead out of the state root is refused (the name
	// rules block it already; this guard stands in front of a folder delete).
	for _, name := range []string{"..", "."} {
		if _, err := stateDirFor(target{name: name, dirs: dirs}); err == nil {
			t.Errorf("box name %q must be refused", name)
		}
	}
	// Without a config the workspace isn't known, but the folder still has
	// to be inside the state root.
	if got, err := stateDirFor(target{name: "main", dirs: dirs}); err != nil || got != filepath.Join(dirs.State, "main") {
		t.Errorf("%s %v", got, err)
	}
}

func TestCheckTar(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "./.pi/agent/models.json", Mode: 0o600, Size: 2})
	tw.Write([]byte("{}"))
	tw.Close()
	good := filepath.Join(dir, "good.tar")
	os.WriteFile(good, buf.Bytes(), 0o600)
	if err := checkTar(good); err != nil {
		t.Error(err)
	}
	// Cut inside a file's content, and cut exactly between files (where
	// Go's reader alone sees a normal end).
	for _, n := range []int{514, 1024, 600} {
		cut := filepath.Join(dir, "cut.tar")
		os.WriteFile(cut, buf.Bytes()[:n], 0o600)
		if err := checkTar(cut); err == nil {
			t.Errorf("a copy cut at %d bytes must be caught", n)
		}
	}
	empty := filepath.Join(dir, "empty.tar")
	os.WriteFile(empty, make([]byte, 1024), 0o600)
	if err := checkTar(empty); err == nil || !strings.Contains(err.Error(), "holds nothing") {
		t.Error(err)
	}
	if err := checkTar(filepath.Join(dir, "missing.tar")); err == nil {
		t.Error("a missing file must be caught")
	}
	// Cut inside a file of zeros: the end looks like tar's end marker, so
	// only reading the content in full catches it.
	buf.Reset()
	tw = tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "./zeros.bin", Mode: 0o600, Size: 4096})
	tw.Write(make([]byte, 4096))
	tw.Close()
	zeros := filepath.Join(dir, "zeros.tar")
	os.WriteFile(zeros, buf.Bytes()[:512+3072], 0o600)
	if err := checkTar(zeros); err == nil {
		t.Error("a copy cut inside a file of zeros must be caught")
	}
}

// Destroy keeps images other containers use, found by name or by ID, but
// not ones only this box's own containers use.
func TestOthersImages(t *testing.T) {
	out := "ezbx-t-box|ezbx-box:aaa\n" +
		"ezbx-t-gateway|ezbx-gateway:bbb\n" +
		"ezbx-main-box|51255bc32fbe\n" + // its image's name moved on, so Docker shows the ID
		"ezbx-main-gateway|ezbx-gateway:ccc\n"
	used := othersImages(out, "ezbx-t-box", "ezbx-t-gateway", "ezbx-t-admin")
	for _, k := range []string{"ezbx-gateway:ccc", "51255bc32fbe"} {
		if !used[k] {
			t.Errorf("%s is used by another box", k)
		}
	}
	for _, k := range []string{"ezbx-box:aaa", "ezbx-gateway:bbb"} {
		if used[k] {
			t.Errorf("%s is only this box's own", k)
		}
	}
}

func TestBackupsPrune(t *testing.T) {
	dir := t.TempDir()
	tg := target{name: "main", dirs: paths.Dirs{State: filepath.Join(dir, "state")}}
	ids := []string{"20260101-000001", "20260101-000002", "20260101-000003", "20260101-000004", "20260101-000005", "20260101-000006", "20260101-000007-2"}
	for _, id := range ids {
		os.MkdirAll(filepath.Join(backupsDir(tg), id), 0o700)
	}
	os.MkdirAll(filepath.Join(backupsDir(tg), "not-a-backup"), 0o700)
	if got := listBackups(tg); strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Errorf("list: %v", got)
	}
	// The newest 5 stay, and so does one named to keep (the one restored from).
	pruneBackups(tg, ids[0])
	if got := strings.Join(listBackups(tg), ","); got != strings.Join(append([]string{ids[0]}, ids[2:]...), ",") {
		t.Errorf("after prune: %s", got)
	}
	if _, err := os.Stat(filepath.Join(backupsDir(tg), "not-a-backup")); err != nil {
		t.Error("prune touches only backup folders")
	}
	for _, bad := range []string{"../x", "20260101", "latest", "20260101-000001/.."} {
		if backupID.MatchString(bad) {
			t.Errorf("%q must not be a backup id", bad)
		}
	}
}
