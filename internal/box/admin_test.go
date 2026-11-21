package box

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestAdminShellArgs(t *testing.T) {
	a := AdminShell{Box: "t", Workspace: "/ws1", Image: "ezbx-box:abc"}
	args := a.Args()
	line := strings.Join(args, " ")
	for _, w := range [][]string{
		{"run", "--rm", "-i"}, {"--label", "ezbx.managed=true"}, {"--label", "ezbx.box=t"}, {"--label", "ezbx.role=admin"},
		{"--name", "ezbx-t-admin"}, {"--user", "0:0"}, {"--security-opt", "no-new-privileges"}, {"--pids-limit", "512"},
		{"--network", "none"}, {"-e", "HOME=/root"}, {"-w", "/workspace"},
		{"-v", "/ws1:/workspace"}, {"-v", "ezbx-t-home:/home/agent"}, {"--entrypoint", "bash", "ezbx-box:abc"},
	} {
		if !has(args, w...) {
			t.Errorf("missing %v in %s", w, line)
		}
	}
	// Root keeps Docker's normal capabilities, but never more, and never the
	// Docker socket; no terminal flag without a console.
	for _, never := range []string{"--privileged", "docker.sock", "--cap-add", "-t", "ezbx-t-inside", "ezbx-t-outside"} {
		if has(args, never) || strings.Contains(line, "docker.sock") {
			t.Errorf("must not have %q: %s", never, line)
		}
	}
	if args[len(args)-1] != "ezbx-box:abc" {
		t.Error("an interactive shell runs bash with nothing after the image")
	}

	a.Net, a.TTY, a.Command = true, true, []string{"apt-get", "install", "-y", "x"}
	args = a.Args()
	if !has(args, "--network", "ezbx-t-outside") || has(args, "--network", "none") || !has(args, "-i", "-t") {
		t.Errorf("--net and a terminal: %v", args)
	}
	if !has(args, "--entrypoint", "apt-get", "ezbx-box:abc", "install", "-y", "x") {
		t.Errorf("a command runs in place of bash: %v", args)
	}
}

func TestHomeBackupAndRestoreArgs(t *testing.T) {
	b := HomeBackupArgs("t", "img")
	for _, w := range [][]string{{"--network", "none"}, {"-v", "ezbx-t-home:/home/agent:ro"}, {"--label", "ezbx.managed=true"},
		{"--entrypoint", "tar", "img", "--numeric-owner", "-C", "/home/agent", "-cf", "-", "."}} {
		if !has(b, w...) {
			t.Errorf("backup: missing %v in %v", w, b)
		}
	}
	if has(b, "-i") {
		t.Error("the backup reads nothing from standard input")
	}
	r := HomeRestoreArgs("t", "img")
	for _, w := range [][]string{{"run", "--rm", "-i"}, {"--network", "none"}, {"-v", "ezbx-t-home:/home/agent"},
		{"--entrypoint", "tar", "img", "--numeric-owner", "-C", "/home/agent", "-xpf", "-"}} {
		if !has(r, w...) {
			t.Errorf("restore: missing %v in %v", w, r)
		}
	}
	if v := HomeVolumeArgs("t"); !has(v, "volume", "create") || !has(v, "--label", "ezbx.managed=true") || v[len(v)-1] != "ezbx-t-home" {
		t.Errorf("volume: %v", v)
	}
}

func TestExistsAndRemoveVolume(t *testing.T) {
	f := newFake()
	r := runner(f)
	ctx := context.Background()
	if ok, err := r.Exists(ctx, "volume", "ezbx-t-home"); ok || err != nil {
		t.Error("missing", ok, err)
	}
	if ok, err := r.RemoveVolume(ctx, "ezbx-t-home"); ok || err != nil {
		t.Error("removing a missing volume is not an error", ok, err)
	}
	f.volumes["ezbx-t-home"] = false // someone else's
	if _, err := r.Exists(ctx, "volume", "ezbx-t-home"); err == nil || !strings.Contains(err.Error(), "didn't create it") {
		t.Error(err)
	}
	if _, err := r.RemoveVolume(ctx, "ezbx-t-home"); err == nil || f.did("volume", "rm") {
		t.Error("a volume EZBox didn't make must never be removed")
	}
	f.volumes["ezbx-t-home"] = true
	if ok, err := r.RemoveVolume(ctx, "ezbx-t-home"); !ok || err != nil || !f.did("volume", "rm", "ezbx-t-home") {
		t.Error(ok, err)
	}
	// An exact name only: a volume whose name merely starts the same is ignored.
	f.volumes["ezbx-t-home-old"] = true
	if ok, _ := r.Exists(ctx, "volume", "ezbx-t-home"); ok {
		t.Error("ezbx-t-home-old is not ezbx-t-home")
	}
}

func TestImages(t *testing.T) {
	var calls []string
	r := &Runner{Out: io.Discard, Docker: func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		calls = append(calls, line)
		if strings.HasPrefix(line, "image ls") {
			return []byte("aaa|ezbx-box:7a60|726MB\nbbb|<none>:<none>|13MB\n\n"), nil
		}
		if line == "image rm ezbx-box:7a60" {
			return nil, errors.New("in use")
		}
		return nil, nil
	}}
	imgs, err := r.Images(context.Background())
	if err != nil || len(imgs) != 2 || imgs[0] != (LocalImage{"aaa", "ezbx-box:7a60", "726MB"}) {
		t.Fatalf("%v %v", imgs, err)
	}
	if !strings.Contains(calls[0], "--filter label=ezbx.managed=true") {
		t.Error("only EZBox's own images may be listed", calls)
	}
	if err := r.RemoveImage(context.Background(), imgs[1]); err != nil || calls[len(calls)-1] != "image rm bbb" {
		t.Error("an image with no name is removed by its ID", calls)
	}
	if err := r.RemoveImage(context.Background(), imgs[0]); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Error(err)
	}
}
