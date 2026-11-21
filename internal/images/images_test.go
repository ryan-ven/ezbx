package images

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ryan-ven/ezbx"
	"github.com/ryan-ven/ezbx/catalog"
	"github.com/ryan-ven/ezbx/harnesses"
	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/internal/tools"
)

var update = flag.Bool("update", false, "rewrite ezbx.lock at the repository root")

const pinned = "@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func boxFS() fstest.MapFS {
	return fstest.MapFS{
		"docker/box.Dockerfile":          {Data: []byte("ARG NODE_IMAGE=node:1" + pinned + "\nFROM ${NODE_IMAGE}\n")},
		"harnesses/pi/install.sh":        {Data: []byte("npm ci\n")},
		"harnesses/pi/package.json":      {Data: []byte("{}\n")},
		"harnesses/pi/package-lock.json": {Data: []byte("{\"lock\":1}\n")},
		"harnesses/pi/manifest.json":     {Data: []byte("not read by the box spec\n")},
	}
}

func pi() harness.Manifest {
	return harness.Manifest{ID: "pi", Name: "Pi", Tools: []string{"fd"},
		Install:  harness.Install{Type: "npm", Package: "p", Version: "1.0.0", Script: "install.sh", Lockfile: "package-lock.json"},
		Verified: harness.Verified{Status: "verified"}}
}

func testCatalog(t *testing.T) *tools.Catalog {
	t.Helper()
	tool := func(id, deb, cmds, extra string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(`{"schema": 1, "id": "` + id + `", "name": "` + id + `", "description": "d", "debian": [` + deb + `], "commands": [` + cmds + `],` + extra +
			` "verified": {"status": "verified", "observedOn": "2026-01-02", "notes": "checked"}}`)}
	}
	c, err := tools.Load(fstest.MapFS{
		"tools/fd.json":     tool("fd", `"fd-find"`, `"fdfind", "fd"`, ` "links": {"fd": "fdfind"},`),
		"tools/jq.json":     tool("jq", `"jq"`, `"jq"`, ""),
		"packs/coding.json": {Data: []byte(`{"schema": 1, "id": "coding", "name": "C", "description": "d", "tools": ["jq"]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var tagRE = regexp.MustCompile(`^ezbx-(gateway|box):[0-9a-f]{12}$`)

func TestBoxSpec(t *testing.T) {
	cat := testCatalog(t)
	s, err := Box(boxFS(), []harness.Manifest{pi()}, cat, []string{"pack:coding"})
	if err != nil {
		t.Fatal(err)
	}
	if !tagRE.MatchString(s.Tag()) || !strings.HasPrefix(s.Tag(), "ezbx-box:") || len(s.Inputs) != 64 {
		t.Errorf("tag %s inputs %s", s.Tag(), s.Inputs)
	}
	if got := strings.Join(s.Files(), " "); got != "docker/box.Dockerfile harnesses/pi/install.sh harnesses/pi/package-lock.json harnesses/pi/package.json" {
		t.Errorf("files: %s", got)
	}
	want := [][2]string{{"DEBIAN_PACKAGES", "fd-find jq"}, {"TOOL_COMMANDS", "fd fdfind jq"}, {"TOOL_LINKS", "fd=fdfind"}}
	for i, a := range want {
		if s.Args[i] != a {
			t.Errorf("arg %d: %v, want %v", i, s.Args[i], a)
		}
	}
	if s.Labels[0] != [2]string{ToolsLabel, "fd,jq"} {
		t.Errorf("labels %v", s.Labels)
	}

	// The same inputs give the same name; anything that goes in changes it.
	same, _ := Box(boxFS(), []harness.Manifest{pi()}, cat, []string{"jq"})
	if same.Inputs != s.Inputs {
		t.Error("pack:coding and jq are the same tools, so the same image")
	}
	other, _ := Box(boxFS(), []harness.Manifest{pi()}, cat, nil)
	if other.Inputs == s.Inputs {
		t.Error("other tools must give another image")
	}
	changed := boxFS()
	changed["harnesses/pi/package-lock.json"] = &fstest.MapFile{Data: []byte("{\"lock\":2}\n")}
	if c, _ := Box(changed, []harness.Manifest{pi()}, cat, []string{"jq"}); c.Inputs == s.Inputs {
		t.Error("a changed lockfile must give another image")
	}
	changed = boxFS()
	changed["harnesses/pi/manifest.json"] = &fstest.MapFile{Data: []byte("notes changed\n")}
	if c, _ := Box(changed, []harness.Manifest{pi()}, cat, []string{"jq"}); c.Inputs != s.Inputs {
		t.Error("the manifest isn't built in, so changing its notes must not rebuild the box")
	}

	// Unverified harnesses aren't built in; a verified one without an
	// install step in the Dockerfile is refused; unknown tools are refused.
	wip := pi()
	wip.ID, wip.Verified.Status = "wip", "unverified"
	if s2, err := Box(boxFS(), []harness.Manifest{pi(), wip}, cat, []string{"jq"}); err != nil || s2.Inputs != s.Inputs {
		t.Errorf("unverified harness: %v", err)
	}
	other2 := pi()
	other2.ID, other2.Name = "other", "Other"
	if _, err := Box(boxFS(), []harness.Manifest{other2}, cat, nil); err == nil || !strings.Contains(err.Error(), "install step for Pi only") {
		t.Error(err)
	}
	if _, err := Box(boxFS(), []harness.Manifest{pi()}, cat, []string{"zzz"}); err == nil || !strings.Contains(err.Error(), `unknown tool "zzz"`) {
		t.Error(err)
	}
}

// The hash covers the build arguments and the labels each on their own, and
// doesn't depend on the order the files come in.
func TestInputsCoverArgsLabelsAndNotOrder(t *testing.T) {
	src := boxFS()
	base := Spec{Kind: "box", Repo: BoxRepo, Dockerfile: "docker/box.Dockerfile", src: src,
		files:  []string{"docker/box.Dockerfile", "harnesses/pi/install.sh"},
		Args:   [][2]string{{"A", "1"}},
		Labels: [][2]string{{"L", "x"}}}
	a, err := base.finish()
	if err != nil {
		t.Fatal(err)
	}
	args := base
	args.Args = [][2]string{{"A", "2"}}
	if b, _ := args.finish(); b.Inputs == a.Inputs {
		t.Error("a different build argument alone must change the hash")
	}
	labels := base
	labels.Labels = [][2]string{{"L", "y"}}
	if b, _ := labels.finish(); b.Inputs == a.Inputs {
		t.Error("a different label alone must change the hash")
	}
	order := base
	order.files = []string{"harnesses/pi/install.sh", "docker/box.Dockerfile", "harnesses/pi/install.sh"}
	if b, err := order.finish(); err != nil || b.Inputs != a.Inputs || len(b.Files()) != 2 {
		t.Errorf("file order and repeats must not matter: %v %v", b.Files(), err)
	}
}

func TestGatewaySpec(t *testing.T) {
	src := fstest.MapFS{
		"go.mod":                    {Data: []byte("module x\n")},
		"cmd/ezbx/main.go":          {Data: []byte("package main\n")},
		"docker/gateway.Dockerfile": {Data: []byte("FROM scratch\n")},
	}
	s, err := Gateway(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Tag(), "ezbx-gateway:") || !tagRE.MatchString(s.Tag()) {
		t.Error(s.Tag())
	}
	if got := strings.Join(s.Files(), " "); got != "cmd/ezbx/main.go docker/gateway.Dockerfile go.mod" {
		t.Error(got)
	}
	src["cmd/ezbx/main.go"] = &fstest.MapFile{Data: []byte("package main // changed\n")}
	if s2, _ := Gateway(src); s2.Inputs == s.Inputs {
		t.Error("any source change must give another gateway image")
	}
	delete(src, "docker/gateway.Dockerfile")
	if _, err := Gateway(src); err == nil {
		t.Error("a missing Dockerfile must be refused")
	}
}

// The build context is the same bytes every time: sorted names, fixed
// times and modes, and the real content.
func TestContext(t *testing.T) {
	s, err := Box(boxFS(), []harness.Manifest{pi()}, testCatalog(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Context()
	b, _ := s.Context()
	if !bytes.Equal(a, b) {
		t.Fatal("not repeatable")
	}
	tr := tar.NewReader(bytes.NewReader(a))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		data, _ := io.ReadAll(tr)
		if h.Mode != 0o644 || !h.ModTime.Equal(time.Unix(0, 0)) || h.Uid != 0 || h.Typeflag != tar.TypeReg {
			t.Errorf("%s: %+v", h.Name, h)
		}
		if h.Name == "harnesses/pi/package-lock.json" && string(data) != "{\"lock\":1}\n" {
			t.Errorf("content %q", data)
		}
	}
	if strings.Join(names, " ") != strings.Join(s.Files(), " ") {
		t.Errorf("%v", names)
	}
}

func TestBuildArgs(t *testing.T) {
	s, _ := Box(boxFS(), []harness.Manifest{pi()}, testCatalog(t), []string{"jq"})
	a := strings.Join(s.BuildArgs(), " ")
	for _, w := range []string{
		"build -f docker/box.Dockerfile -t " + s.Tag(),
		"--label ezbx.managed=true", "--label ezbx.inputs=" + s.Inputs, "--label ezbx.image=box", "--label ezbx.tools=fd,jq",
		"--build-arg DEBIAN_PACKAGES=fd-find jq", "--build-arg TOOL_LINKS=fd=fdfind",
	} {
		if !strings.Contains(a, w) {
			t.Errorf("missing %q in %s", w, a)
		}
	}
	if args := s.BuildArgs(); args[len(args)-1] != "-" {
		t.Error("the context must come from standard input")
	}
}

func TestBaseImages(t *testing.T) {
	check := func(text string) ([][2]string, error) {
		return BaseImages(fstest.MapFS{"d": {Data: []byte(text)}}, "d")
	}
	got, err := check("ARG GO_IMAGE=golang:1" + pinned + "\nFROM ${GO_IMAGE} AS build\nFROM build AS two\nFROM scratch\nFROM alpine:3" + pinned + "\n")
	if err != nil || len(got) != 2 || got[0] != [2]string{"GO_IMAGE", "golang:1" + pinned} || got[1][0] != "FROM" {
		t.Fatalf("%v %v", got, err)
	}
	for text, want := range map[string]string{
		"FROM golang:1.27.4\n":                     "not pinned by digest",
		"FROM golang:1.27.4 AS build\n":            "not pinned by digest",
		"ARG GO_IMAGE=golang:1.27.4\nFROM x\n":     "GO_IMAGE must be pinned",
		"FROM ${NODE_IMAGE}\n":                     "no pinned ARG default",
		"FROM later\nFROM scratch AS later\n":      "not pinned by digest", // a stage only counts after it is named
		"ARG GO_IMAGE=golang:1@sha256:abc\nFROM x": "must be pinned",
	} {
		if _, err := check(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
}

// Every Dockerfile in the repository starts from images pinned by digest.
func TestRealDockerfilesArePinned(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "docker", "*.Dockerfile"))
	if err != nil || len(files) < 3 {
		t.Fatalf("%v %v", files, err)
	}
	for _, f := range files {
		if _, err := BaseImages(ezbx.Source, "docker/"+filepath.Base(f)); err != nil {
			t.Error(err)
		}
	}
}

// ezbx.lock is generated from the files, and must be current. Regenerate it
// with: go test ./internal/images -run TestLockFile -update
func TestLockFile(t *testing.T) {
	hs, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := tools.Load(catalog.FS)
	if err != nil {
		t.Fatal(err)
	}
	text, err := Lock(ezbx.Source, hs, cat)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if _, err := toml.Decode(text, &parsed); err != nil {
		t.Fatalf("the lock isn't valid TOML: %v\n%s", err, text)
	}
	for _, want := range []string{`GO_IMAGE = "golang:`, `NODE_IMAGE = "node:`, "[harnesses.pi]", `lockfile_sha256 = "`, `tools = ["fd", "ripgrep"]`, "[tools.ripgrep]", "[packs.coding]"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	file := filepath.Join("..", "..", "ezbx.lock")
	if *update {
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != text {
		t.Error("ezbx.lock is out of date. Regenerate it: go test ./internal/images -run TestLockFile -update")
	}
}

// fakeDocker answers the docker commands the runner uses.
type fakeDocker struct {
	images  map[string]string // tag -> inputs label
	ls      string
	used    string // images containers use, one per line
	run     string // output of docker run
	calls   []string
	stdin   []byte
	removed []string
	failRm  map[string]bool
}

func (f *fakeDocker) docker(_ context.Context, _ []byte, args ...string) ([]byte, error) {
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	switch {
	case strings.HasPrefix(line, "image inspect --format {{.Id}}"):
		if _, ok := f.images[args[len(args)-1]]; !ok {
			return nil, errors.New("No such image")
		}
		return []byte("sha256:feed\n"), nil
	case strings.HasPrefix(line, "image inspect"):
		label, ok := f.images[args[len(args)-1]]
		if !ok {
			return nil, errors.New("No such image")
		}
		return []byte(label + "\n"), nil
	case strings.HasPrefix(line, "run "):
		return []byte(f.run), nil
	case strings.HasPrefix(line, "image ls"):
		return []byte(f.ls), nil
	case line == "container ls --all --format {{.Image}}":
		return []byte(f.used), nil
	case strings.HasPrefix(line, "image rm"):
		if f.failRm[args[2]] {
			return nil, errors.New("image is being used by a container")
		}
		f.removed = append(f.removed, args[2])
		return nil, nil
	}
	return nil, errors.New("unexpected: " + line)
}

func (f *fakeDocker) runner() *Runner {
	return &Runner{Docker: f.docker, Out: io.Discard, Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		Stream: func(_ context.Context, stdin []byte, args ...string) error {
			f.calls = append(f.calls, "STREAM "+strings.Join(args, " "))
			f.stdin = stdin
			// The build makes the image with the labels it was given.
			for i, a := range args {
				if a == "-t" {
					for _, l := range args {
						if v, ok := strings.CutPrefix(l, InputsLabel+"="); ok {
							f.images[args[i+1]] = v
						}
					}
				}
			}
			return nil
		}}
}

func TestCheck(t *testing.T) {
	s, _ := Box(boxFS(), []harness.Manifest{pi()}, testCatalog(t), nil)
	f := &fakeDocker{images: map[string]string{}}
	var nb ErrNotBuilt
	if err := f.runner().Check(context.Background(), s); !errors.As(err, &nb) || !strings.Contains(err.Error(), "Run ezbx build") {
		t.Errorf("missing: %v", err)
	}
	f.images[s.Tag()] = "something-else"
	if err := f.runner().Check(context.Background(), s); err == nil || errors.As(err, &nb) || !strings.Contains(err.Error(), "wasn't built by ezbx build") {
		t.Errorf("wrong label: %v", err)
	}
	f.images[s.Tag()] = s.Inputs
	if err := f.runner().Check(context.Background(), s); err != nil {
		t.Error(err)
	}
}

func TestBuildAndObserve(t *testing.T) {
	s, _ := Box(boxFS(), []harness.Manifest{pi()}, testCatalog(t), []string{"jq"})
	f := &fakeDocker{images: map[string]string{},
		run: "20261104T212339Z\nca-certificates=20230311\ngit=1:2.39.5-0+deb12u2\nfd-find=8.6.0-3\njq=1.6-2.1+deb12u3\npi=1.5.0\n"}
	b, err := f.runner().Build(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := s.Context()
	if !bytes.Equal(f.stdin, want) {
		t.Error("the build context must go to docker on standard input")
	}
	if !strings.HasPrefix(f.calls[0], "STREAM build -f docker/box.Dockerfile") {
		t.Error(f.calls)
	}
	if b.Tag != s.Tag() || b.ID != "sha256:feed" || b.Inputs != s.Inputs || b.Built != "2026-01-02T03:04:05Z" || b.Snapshot != "20261104T212339Z" ||
		b.Packages["fd-find"] != "8.6.0-3" || b.Packages["git"] != "1:2.39.5-0+deb12u2" || b.Harness["pi"] != "1.5.0" || strings.Join(b.Tools, ",") != "fd,jq" {
		t.Errorf("%+v", b)
	}
	// The facts come from a container with no network and no privileges.
	var run string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "run ") {
			run = c
		}
	}
	for _, w := range []string{"--rm", "--network none", "--cap-drop ALL", "--security-opt no-new-privileges", "--label ezbx.managed=true", " ca-certificates git fd-find jq"} {
		if !strings.Contains(run, w) {
			t.Errorf("missing %q in %s", w, run)
		}
	}

	// The gateway records only its ID.
	gw, _ := Gateway(fstest.MapFS{"docker/gateway.Dockerfile": {Data: []byte("FROM scratch\n")}})
	b, err = f.runner().Build(context.Background(), gw)
	if err != nil || b.ID != "sha256:feed" || b.Packages != nil {
		t.Errorf("%+v %v", b, err)
	}

	// A build that doesn't leave the right image is an error.
	bad := &fakeDocker{images: map[string]string{}}
	r := bad.runner()
	r.Stream = func(context.Context, []byte, ...string) error { return nil }
	if _, err := r.Build(context.Background(), s); err == nil || !strings.Contains(err.Error(), "after the build") {
		t.Error(err)
	}
	r.Stream = func(context.Context, []byte, ...string) error { return errors.New("exit status 1") }
	if _, err := r.Build(context.Background(), s); err == nil || !strings.Contains(err.Error(), "failed (see the output above)") {
		t.Error(err)
	}
}

func TestRecord(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cfg", "images.json")
	rec, err := ReadRecord(file)
	if err != nil || len(rec.Images) != 0 {
		t.Fatal(rec, err)
	}
	rec.Put("box", Built{Tag: "ezbx-box:111"})
	rec.Put("box", Built{Tag: "ezbx-box:222"})
	rec.Put("box", Built{Tag: "ezbx-box:222", ID: "again"}) // the same tag keeps what it replaced
	if b := rec.Images["box"]; b.Previous != "ezbx-box:111" || b.ID != "again" {
		t.Errorf("%+v", b)
	}
	if err := rec.Write(file); err != nil {
		t.Fatal(err)
	}
	back, err := ReadRecord(file)
	if err != nil || back.Images["box"].Previous != "ezbx-box:111" {
		t.Fatal(back, err)
	}
	if _, err := os.Stat(file + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary file must be gone")
	}
	if err := os.WriteFile(file, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(file); err == nil || !strings.Contains(err.Error(), file) {
		t.Error(err)
	}
}

func TestKeepAndOld(t *testing.T) {
	box := Spec{Kind: "box", Repo: BoxRepo, Inputs: "222222222222aaaa"}
	gw := Spec{Kind: "gateway", Repo: GatewayRepo, Inputs: "999999999999bbbb"}
	rec := Record{Images: map[string]Built{
		"box":     {Tag: "ezbx-box:222222222222", Previous: "ezbx-box:111111111111"},
		"gateway": {Tag: "ezbx-gateway:888888888888"}, // not the current one yet
	}}
	keep := rec.Keep(box, gw)
	for _, k := range []string{"ezbx-box:222222222222", "ezbx-box:111111111111", "ezbx-gateway:999999999999", "ezbx-gateway:888888888888"} {
		if !keep[k] {
			t.Errorf("must keep %s: %v", k, keep)
		}
	}
	f := &fakeDocker{ls: strings.Join([]string{
		"ezbx-box:222222222222", "ezbx-box:111111111111", "ezbx-box:000000000000", "ezbx-box:dev",
		"ezbx-gateway:999999999999", "ezbx-gateway:888888888888", "ezbx-gateway:dev",
		"ezbx-probe:dev", "<none>:<none>", "ezbx-boxer:1", "other:1",
	}, "\n") + "\n", used: "ezbx-box:dev\nnode:24\n", failRm: map[string]bool{"ezbx-box:dev": true}}
	old, inUse, err := f.runner().Old(context.Background(), keep)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(old, " "); got != "ezbx-box:000000000000 ezbx-gateway:dev" {
		t.Errorf("old: %s", got)
	}
	if got := strings.Join(inUse, " "); got != "ezbx-box:dev" {
		t.Errorf("an image a container uses is kept and named: %s", got)
	}
	if !strings.Contains(f.calls[0], "--filter label=ezbx.managed=true") {
		t.Error("only EZBox's own images may be listed", f.calls)
	}
	if err := f.runner().Remove(context.Background(), "ezbx-box:dev"); err == nil || !strings.Contains(err.Error(), "used by a container") {
		t.Error(err)
	}
	if err := f.runner().Remove(context.Background(), "ezbx-box:000000000000"); err != nil || f.removed[0] != "ezbx-box:000000000000" {
		t.Error(err, f.removed)
	}
}
