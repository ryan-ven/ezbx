package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Runner runs docker. Docker captures a command's output; Stream lets a long
// command (a build) print straight to the person's terminal.
type Runner struct {
	Docker func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
	Stream func(ctx context.Context, stdin []byte, args ...string) error
	Out    io.Writer
	Now    func() time.Time
}

// ErrNotBuilt means the image for these inputs isn't on this computer.
type ErrNotBuilt struct{ Tag string }

func (e ErrNotBuilt) Error() string {
	return fmt.Sprintf("the image %s isn't built yet. Run ezbx build (it needs the internet once)", e.Tag)
}

// Check reports whether the spec's image is on this computer and was built
// from these inputs. An image with the right name but another inputs label
// was made some other way, and is refused.
func (r *Runner) Check(ctx context.Context, s Spec) error {
	out, err := r.Docker(ctx, nil, "image", "inspect", "--format", "{{index .Config.Labels \""+InputsLabel+"\"}}", s.Tag())
	if err != nil {
		return ErrNotBuilt{Tag: s.Tag()}
	}
	if got := strings.TrimSpace(string(out)); got != s.Inputs {
		return fmt.Errorf("the image %s wasn't built by ezbx build from these files (its %s label is %q). Remove it, then run ezbx build", s.Tag(), InputsLabel, got)
	}
	return nil
}

// Built is what one build produced, as recorded in images.json.
type Built struct {
	Tag      string            `json:"tag"`
	ID       string            `json:"id"`
	Inputs   string            `json:"inputs"`
	Built    string            `json:"built"`              // RFC 3339
	Previous string            `json:"previous,omitempty"` // the tag this one replaced
	Snapshot string            `json:"debianSnapshot,omitempty"`
	Packages map[string]string `json:"debianPackages,omitempty"`
	Tools    []string          `json:"tools,omitempty"`
	Harness  map[string]string `json:"harnesses,omitempty"` // id -> version reported by the program
}

// Build builds the image, then reads back what it got. The build context
// goes to docker on standard input, so nothing is written to disk.
func (r *Runner) Build(ctx context.Context, s Spec) (Built, error) {
	tar, err := s.Context()
	if err != nil {
		return Built{}, err
	}
	fmt.Fprintf(r.Out, "building %s (%s image)\n", s.Tag(), s.Kind)
	if err := r.Stream(ctx, tar, s.BuildArgs()...); err != nil {
		return Built{}, fmt.Errorf("building %s failed (see the output above): %w", s.Tag(), err)
	}
	if err := r.Check(ctx, s); err != nil {
		return Built{}, fmt.Errorf("after the build: %w", err)
	}
	return r.Observe(ctx, s)
}

// Observe records an image that is on this computer: its ID and, for the box,
// the Debian snapshot and package versions and the harness versions it got.
func (r *Runner) Observe(ctx context.Context, s Spec) (Built, error) {
	b := Built{Tag: s.Tag(), Inputs: s.Inputs, Built: r.now().UTC().Format(time.RFC3339), Tools: s.Tools.IDs()}
	out, err := r.Docker(ctx, nil, "image", "inspect", "--format", "{{.Id}}", s.Tag())
	if err != nil {
		return Built{}, fmt.Errorf("reading the image ID of %s: %w", s.Tag(), err)
	}
	b.ID = strings.TrimSpace(string(out))
	if s.Kind != "box" {
		return b, nil
	}
	// A throwaway container with no network and no privileges prints the
	// facts: the snapshot line, then package=version lines, then Pi's version.
	pkgs := append([]string{"ca-certificates", "git"}, s.Tools.Debian...)
	script := `sed -n 's|^URIs: http://snapshot\.debian\.org/archive/debian/||p' /etc/apt/sources.list.d/debian.sources | head -n 1
dpkg-query -W -f '${Package}=${Version}\n' "$@"
printf 'pi=%s\n' "$(pi --version 2>/dev/null)"`
	args := append([]string{"run", "--rm", "--label", ManagedLabel, "--network", "none", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--entrypoint", "sh", s.Tag(), "-c", script, "sh"}, pkgs...)
	out, err = r.Docker(ctx, nil, args...)
	if err != nil {
		return Built{}, fmt.Errorf("reading the versions in %s: %w", s.Tag(), err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	b.Snapshot = strings.TrimRight(strings.TrimSpace(lines[0]), "/")
	b.Packages = map[string]string{}
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		switch {
		case !ok:
		case k == "pi":
			b.Harness = map[string]string{"pi": v}
		default:
			b.Packages[k] = v
		}
	}
	return b, nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Record is images.json: what was last built on this computer, by kind.
type Record struct {
	Version int              `json:"version"`
	Images  map[string]Built `json:"images"`
}

// RecordFile is where the record lives: next to ezbx.toml.
func RecordFile(configDir string) string { return filepath.Join(configDir, "images.json") }

// ReadRecord reads the record; a missing file is an empty record.
func ReadRecord(file string) (Record, error) {
	rec := Record{Version: 1, Images: map[string]Built{}}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("%s: %w", file, err)
	}
	if rec.Images == nil {
		rec.Images = map[string]Built{}
	}
	return rec, nil
}

// Put stores a build, remembering the tag it replaces (kept by prune).
func (rec *Record) Put(kind string, b Built) {
	if old, ok := rec.Images[kind]; ok && old.Tag != b.Tag {
		b.Previous = old.Tag
	} else if ok {
		b.Previous = old.Previous
	}
	rec.Images[kind] = b
}

// Write saves the record through a temporary file, so a crash never leaves
// half a file.
func (rec Record) Write(file string) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// Keep lists the tags prune must leave: the current ones and the ones they
// replaced.
func (rec Record) Keep(current ...Spec) map[string]bool {
	keep := map[string]bool{}
	for _, s := range current {
		keep[s.Tag()] = true
		if b, ok := rec.Images[s.Kind]; ok {
			if b.Tag == s.Tag() && b.Previous != "" {
				keep[b.Previous] = true
			} else if b.Tag != s.Tag() {
				keep[b.Tag] = true // the last build becomes the one to go back to
			}
		}
	}
	return keep
}

// Old lists EZBox's own gateway and box images that prune may remove: those
// labelled ezbx.managed, with a name, not kept, and not used by any
// container (running or stopped). The used ones are listed apart.
func (r *Runner) Old(ctx context.Context, keep map[string]bool) (old, inUse []string, err error) {
	out, err := r.Docker(ctx, nil, "image", "ls", "--filter", "label="+ManagedLabel, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, nil, fmt.Errorf("listing images: %w", err)
	}
	used, err := r.Docker(ctx, nil, "container", "ls", "--all", "--format", "{{.Image}}")
	if err != nil {
		return nil, nil, fmt.Errorf("listing containers: %w", err)
	}
	usedBy := map[string]bool{}
	for _, line := range strings.Split(string(used), "\n") {
		usedBy[strings.TrimSpace(line)] = true
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		tag := strings.TrimSpace(line)
		repo, version, _ := strings.Cut(tag, ":")
		switch {
		case (repo != GatewayRepo && repo != BoxRepo) || version == "" || version == "<none>" || keep[tag]:
		case usedBy[tag]:
			inUse = append(inUse, tag)
		default:
			old = append(old, tag)
		}
	}
	sort.Strings(old)
	sort.Strings(inUse)
	return old, inUse, nil
}

// Remove removes one image by name. Docker refuses an image a container
// still uses, and that refusal is returned.
func (r *Runner) Remove(ctx context.Context, tag string) error {
	if _, err := r.Docker(ctx, nil, "image", "rm", tag); err != nil {
		return fmt.Errorf("removing %s: %w", tag, err)
	}
	return nil
}
