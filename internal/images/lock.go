package images

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/internal/tools"
)

var (
	argImage  = regexp.MustCompile(`^ARG ([A-Z][A-Z0-9_]*_IMAGE)=(\S+)$`)
	fromLine  = regexp.MustCompile(`(?i)^FROM\s+(\S+)(\s+AS\s+(\S+))?\s*$`)
	digestRef = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)
	argRef    = regexp.MustCompile(`^\$\{([A-Z][A-Z0-9_]*_IMAGE)\}$`)
)

// BaseImages reads a Dockerfile's base images and checks that each FROM is
// pinned by digest: either directly, through an ARG NAME_IMAGE=...@sha256:...
// default, or by naming an earlier stage (or scratch, which is empty).
func BaseImages(src fs.FS, dockerfile string) ([][2]string, error) {
	data, err := fs.ReadFile(src, dockerfile)
	if err != nil {
		return nil, err
	}
	args := map[string]string{}
	stages := map[string]bool{"scratch": true}
	var bases [][2]string
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if m := argImage.FindStringSubmatch(line); m != nil {
			if !digestRef.MatchString(m[2]) {
				return nil, fmt.Errorf("%s line %d: %s must be pinned by digest (name:tag@sha256:...)", dockerfile, n+1, m[1])
			}
			args[m[1]] = m[2]
			bases = append(bases, [2]string{m[1], m[2]})
			continue
		}
		m := fromLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref := m[1]
		switch {
		case stages[strings.ToLower(ref)]:
		case digestRef.MatchString(ref):
			bases = append(bases, [2]string{"FROM", ref})
		case argRef.MatchString(ref):
			if _, ok := args[argRef.FindStringSubmatch(ref)[1]]; !ok {
				return nil, fmt.Errorf("%s line %d: %s has no pinned ARG default above it", dockerfile, n+1, ref)
			}
		default:
			return nil, fmt.Errorf("%s line %d: FROM %s is not pinned by digest", dockerfile, n+1, ref)
		}
		if m[3] != "" {
			stages[strings.ToLower(m[3])] = true
		}
	}
	return bases, nil
}

// Lock writes ezbx.lock: a readable list of every pinned input of the
// images. It is generated, so it can't drift from the files it describes,
// and a test keeps the committed copy current.
func Lock(src fs.FS, hs []harness.Manifest, cat *tools.Catalog) (string, error) {
	var b strings.Builder
	b.WriteString(`# ezbx.lock: the pinned inputs of the EZBox images.
#
# Generated from the Dockerfiles, the harness folders and the tool catalogue,
# so don't edit it by hand. After changing those, regenerate it with
#   go test ./internal/images -run TestLockFile -update
# and the tests fail while it is out of date.
#
# The box image's Debian packages come from the dated snapshot its base image
# was made from, so the base image's digest also fixes their versions.
# ezbx build records the versions it got in images.json, next to ezbx.toml.
version = 1
`)
	for _, img := range []struct{ kind, file string }{{"gateway", "docker/gateway.Dockerfile"}, {"box", "docker/box.Dockerfile"}} {
		bases, err := BaseImages(src, img.file)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n[images.%s]\ndockerfile = %s\n", img.kind, strconv.Quote(img.file))
		for _, kv := range bases {
			fmt.Fprintf(&b, "%s = %s\n", kv[0], strconv.Quote(kv[1]))
		}
	}

	sorted := append([]harness.Manifest(nil), hs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, m := range sorted {
		fmt.Fprintf(&b, "\n[harnesses.%s]\ntype = %s\npackage = %s\nversion = %s\nverified = %s\n",
			m.ID, strconv.Quote(m.Install.Type), strconv.Quote(m.Install.Package), strconv.Quote(m.Install.Version), strconv.Quote(m.Verified.Status))
		if m.Install.Lockfile != "" {
			file := "harnesses/" + m.ID + "/" + m.Install.Lockfile
			sum, err := fileSHA256(src, file)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "lockfile = %s\nlockfile_sha256 = %s\n", strconv.Quote(file), strconv.Quote(sum))
		}
		fmt.Fprintf(&b, "tools = %s\n", tomlList(m.Tools))
	}

	for _, id := range cat.ToolIDs() {
		t := cat.Tools[id]
		fmt.Fprintf(&b, "\n[tools.%s]\ndebian = %s\ncommands = %s\nverified = %s\n",
			id, tomlList(t.Debian), tomlList(t.Commands), strconv.Quote(t.Verified.Status))
	}
	for _, id := range cat.PackIDs() {
		fmt.Fprintf(&b, "\n[packs.%s]\ntools = %s\n", id, tomlList(cat.Packs[id].Tools))
	}
	return b.String(), nil
}

func fileSHA256(src fs.FS, name string) (string, error) {
	data, err := fs.ReadFile(src, name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func tomlList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}
