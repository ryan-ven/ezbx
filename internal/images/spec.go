// Package images works out, builds and records EZBox's two images, the
// gateway and the box, from the files carried inside the ezbx program. See
// docs/build.md.
//
// Each image is named after a hash of everything that goes into it (its
// build files and build arguments), so a different input always gives a
// different tag, and ezbx up can tell whether the image it needs was built.
package images

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/internal/tools"
)

// Names and labels.
const (
	GatewayRepo  = "ezbx-gateway"
	BoxRepo      = "ezbx-box"
	InputsLabel  = "ezbx.inputs" // the full hash of the inputs
	KindLabel    = "ezbx.image"  // gateway | box
	ToolsLabel   = "ezbx.tools"  // the box's tool ids, separated by commas
	ManagedLabel = "ezbx.managed=true"
	tagLength    = 12 // hex digits of the hash in the tag
)

// Spec is one image: what goes in, and the name that follows from it.
type Spec struct {
	Kind       string // gateway | box
	Repo       string
	Dockerfile string      // path inside the build files
	Args       [][2]string // build arguments, sorted by name
	Labels     [][2]string // extra labels, sorted by name
	Inputs     string      // sha256 of the build files and arguments, in hex
	Tools      tools.Selection

	src   fs.FS
	files []string // sorted paths in src
}

// Tag is the image's name: the repository and the start of the hash.
func (s Spec) Tag() string { return s.Repo + ":" + s.Inputs[:tagLength] }

// Gateway is the gateway image: the ezbx binary compiled from the whole of
// the carried source (the program embeds its own build files, so they all
// have to be there for it to compile).
func Gateway(src fs.FS) (Spec, error) {
	var files []string
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return Spec{}, err
	}
	s := Spec{Kind: "gateway", Repo: GatewayRepo, Dockerfile: "docker/gateway.Dockerfile", src: src, files: files}
	return s.finish()
}

// Box is the box image: the harnesses' install files and the chosen tools.
// Every verified harness is built in, so each one's own tools are added to
// the chosen ones. The box Dockerfile installs Pi only, so any other
// harness is refused until it has an install step there.
func Box(src fs.FS, hs []harness.Manifest, cat *tools.Catalog, chosen []string) (Spec, error) {
	files := []string{"docker/box.Dockerfile"}
	var needed []string
	for _, m := range hs {
		if m.Verified.Status != "verified" {
			continue
		}
		if m.ID != "pi" {
			return Spec{}, fmt.Errorf("the box image can't install %s yet: docker/box.Dockerfile has an install step for Pi only", m.Name)
		}
		dir := "harnesses/" + m.ID + "/"
		files = append(files, dir+m.Install.Script)
		if m.Install.Lockfile != "" {
			files = append(files, dir+m.Install.Lockfile)
		}
		if m.Install.Type == "npm" {
			files = append(files, dir+"package.json")
		}
		needed = append(needed, m.Tools...)
	}
	sel, err := cat.Resolve(chosen, needed)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{
		Kind: "box", Repo: BoxRepo, Dockerfile: "docker/box.Dockerfile", Tools: sel, src: src, files: files,
		Args: [][2]string{
			{"DEBIAN_PACKAGES", strings.Join(sel.Debian, " ")},
			{"TOOL_COMMANDS", strings.Join(sel.Commands, " ")},
			{"TOOL_LINKS", strings.Join(sel.Links, " ")},
		},
		Labels: [][2]string{{ToolsLabel, strings.Join(sel.IDs(), ",")}},
	}
	return s.finish()
}

// finish sorts the inputs and works out the hash.
func (s Spec) finish() (Spec, error) {
	sort.Strings(s.files)
	s.files = dedupe(s.files)
	if !contains(s.files, s.Dockerfile) {
		return Spec{}, fmt.Errorf("internal error: %s is not among the build files", s.Dockerfile)
	}
	sort.Slice(s.Args, func(i, j int) bool { return s.Args[i][0] < s.Args[j][0] })
	sort.Slice(s.Labels, func(i, j int) bool { return s.Labels[i][0] < s.Labels[j][0] })
	ctx, err := s.Context()
	if err != nil {
		return Spec{}, err
	}
	h := sha256.New()
	fmt.Fprintf(h, "ezbx image inputs 1\x00%s\x00%s\x00", s.Kind, s.Dockerfile)
	h.Write(ctx)
	for _, a := range s.Args {
		fmt.Fprintf(h, "\x00arg\x00%s\x00%s", a[0], a[1])
	}
	for _, l := range s.Labels {
		fmt.Fprintf(h, "\x00label\x00%s\x00%s", l[0], l[1])
	}
	s.Inputs = hex.EncodeToString(h.Sum(nil))
	return s, nil
}

// Files lists the build files, sorted.
func (s Spec) Files() []string { return append([]string(nil), s.files...) }

// Context is the build context as a tar stream. It is the same bytes every
// time for the same files: sorted, with fixed times, owners and modes.
func (s Spec) Context() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, p := range s.files {
		data, err := fs.ReadFile(s.src, p)
		if err != nil {
			return nil, fmt.Errorf("build file %s: %w", p, err)
		}
		hdr := &tar.Header{
			Name: p, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0),
			Typeflag: tar.TypeReg, Format: tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// BuildArgs is the docker command that builds the image from a context on
// standard input.
func (s Spec) BuildArgs() []string {
	a := []string{"build", "-f", s.Dockerfile, "-t", s.Tag(),
		"--label", ManagedLabel, "--label", InputsLabel + "=" + s.Inputs, "--label", KindLabel + "=" + s.Kind}
	for _, l := range s.Labels {
		a = append(a, "--label", l[0]+"="+l[1])
	}
	for _, b := range s.Args {
		a = append(a, "--build-arg", b[0]+"="+b[1])
	}
	return append(a, "-")
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
