package box

import (
	"context"
	"fmt"
	"strings"
)

// AdminShell is a short-lived root container for the human: the box image,
// with the same workspace and agent home mounted, separate from the box
// (where root has no capabilities). It is removed when it exits.
type AdminShell struct {
	Box       string
	Workspace string // full host path
	Image     string
	Net       bool     // connect to the gateway's outside network, for downloads
	TTY       bool     // the terminal is a real console
	Command   []string // empty for an interactive bash
}

// AdminName is the admin container's name; one at a time per box.
func AdminName(box string) string { return "ezbx-" + box + "-admin" }

// Args is the docker run command. Root keeps Docker's normal capabilities
// (so apt and chown work) but never --privileged, and there is no Docker
// socket. Its home is /root, so nothing the agent left in its own home (a
// .bashrc, say) runs as root.
func (a AdminShell) Args() []string {
	n := NamesFor(a.Box)
	args := []string{"run", "--rm", "-i"}
	if a.TTY {
		args = append(args, "-t")
	}
	args = append(args, labels(a.Box)...)
	network := "none"
	if a.Net {
		network = n.Outside
	}
	args = append(args, "--label", "ezbx.role=admin", "--name", AdminName(a.Box), "--hostname", "ezbx-admin",
		"--user", "0:0", "--security-opt", "no-new-privileges", "--pids-limit", "512",
		"--network", network, "-e", "HOME=/root", "-w", "/workspace",
		"-v", a.Workspace+":/workspace", "-v", n.Home+":"+agentHome)
	if len(a.Command) == 0 {
		return append(args, "--entrypoint", "bash", a.Image)
	}
	return append(append(args, "--entrypoint", a.Command[0], a.Image), a.Command[1:]...)
}

// HomeBackupArgs packs the agent's home volume as a tar stream on standard
// output: read-only, no network, as root so every file can be read.
func HomeBackupArgs(box, image string) []string {
	return []string{"run", "--rm", "--label", ManagedLabel, "--label", "ezbx.box=" + box, "--network", "none",
		"--user", "0:0", "--security-opt", "no-new-privileges", "-v", NamesFor(box).Home + ":" + agentHome + ":ro",
		"--entrypoint", "tar", image, "--numeric-owner", "-C", agentHome, "-cf", "-", "."}
}

// HomeRestoreArgs unpacks a tar stream from standard input into the agent's
// home volume, keeping owners and modes.
func HomeRestoreArgs(box, image string) []string {
	return []string{"run", "--rm", "-i", "--label", ManagedLabel, "--label", "ezbx.box=" + box, "--network", "none",
		"--user", "0:0", "--security-opt", "no-new-privileges", "-v", NamesFor(box).Home + ":" + agentHome,
		"--entrypoint", "tar", image, "--numeric-owner", "-C", agentHome, "-xpf", "-"}
}

// Exists reports whether a container, network or volume with this exact name
// exists, refusing one EZBox didn't make.
func (r *Runner) Exists(ctx context.Context, kind, name string) (bool, error) {
	f, err := r.lookup(ctx, kind, name)
	if err != nil || !f.exists {
		return false, err
	}
	if !f.managed {
		return false, fmt.Errorf("a %s named %s exists, but EZBox didn't create it, so EZBox won't touch it", kind, name)
	}
	return true, nil
}

// RemoveVolume removes the box's home volume, if it is EZBox's.
func (r *Runner) RemoveVolume(ctx context.Context, name string) (bool, error) {
	ok, err := r.Exists(ctx, "volume", name)
	if err != nil || !ok {
		return false, err
	}
	if _, err := r.run(ctx, "volume", "rm", name); err != nil {
		return false, fmt.Errorf("removing the volume %s: %w", name, err)
	}
	r.say("removed volume %s", name)
	return true, nil
}

// HomeVolumeArgs creates the agent's home volume for a box.
func HomeVolumeArgs(box string) []string {
	return Plan{Box: box, Names: NamesFor(box)}.HomeVolumeArgs()
}

// LocalImage is an EZBox image on this computer.
type LocalImage struct {
	ID, Name, Size string // Name is repository:tag, or <none>:<none>
}

// Images lists every image labelled ezbx.managed, with its size.
func (r *Runner) Images(ctx context.Context) ([]LocalImage, error) {
	out, err := r.run(ctx, "image", "ls", "--filter", "label="+ManagedLabel, "--format", "{{.ID}}|{{.Repository}}:{{.Tag}}|{{.Size}}")
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}
	var list []LocalImage
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) == 3 && f[0] != "" {
			list = append(list, LocalImage{ID: f[0], Name: f[1], Size: f[2]})
		}
	}
	return list, nil
}

// RemoveImage removes one image, by name, or by ID when it has none.
func (r *Runner) RemoveImage(ctx context.Context, img LocalImage) error {
	ref := img.Name
	if strings.Contains(ref, "<none>") {
		ref = img.ID
	}
	if _, err := r.run(ctx, "image", "rm", ref); err != nil {
		return fmt.Errorf("removing %s: %w", ref, err)
	}
	r.say("removed image %s", ref)
	return nil
}
