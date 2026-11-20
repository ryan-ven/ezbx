package box

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ryan-ven/ezbx/internal/harness"
)

func testPlan(t *testing.T) Plan {
	dir := t.TempDir()
	return Plan{
		Box: "t", Names: NamesFor("t"),
		Workspace: filepath.Join(dir, "workspace"), StateDir: filepath.Join(dir, "state", "t"), LogOn: true,
		Upstream: "http://host.docker.internal:18080", EngineID: "strata", EngineName: "Strata",
		Network: "allow", Allow: []string{"registry.npmjs.org"},
		Memory: "4g", PIDs: 512, Harnesses: []string{"pi"},
		HomeFiles: []harness.File{
			{Path: ".pi/agent/models.json", Mode: 0o600, Data: []byte(`{"m":1}`)},
			{Path: ".pi/agent/settings.json", Mode: 0o600, Data: []byte(`{"s":1}`)},
		},
	}
}

// has reports whether args holds the given words next to each other.
func has(args []string, words ...string) bool {
	for i := 0; i+len(words) <= len(args); i++ {
		ok := true
		for j, w := range words {
			ok = ok && args[i+j] == w
		}
		if ok {
			return true
		}
	}
	return false
}

func noPublish(t *testing.T, args []string) {
	t.Helper()
	for _, a := range args {
		if a == "-p" || a == "-P" || strings.HasPrefix(a, "--publish") || strings.Contains(a, "docker.sock") || a == "--privileged" || a == "--cap-add" {
			t.Errorf("must not appear: %q in %v", a, args)
		}
	}
}

func TestNetworkArgs(t *testing.T) {
	p := testPlan(t)
	in := p.InsideNetworkArgs()
	for _, w := range [][]string{
		{"--internal"}, {"-o", "com.docker.network.bridge.gateway_mode_ipv4=isolated"},
		{"-o", "com.docker.network.enable_ipv6=false"}, {"--label", "ezbx.managed=true"}, {"--label", "ezbx.box=t"},
	} {
		if !has(in, w...) {
			t.Errorf("inside network lacks %v: %v", w, in)
		}
	}
	if in[len(in)-1] != "ezbx-t-inside" {
		t.Error("name:", in)
	}
	out := p.OutsideNetworkArgs()
	if has(out, "--internal") || !has(out, "--label", "ezbx.managed=true") || out[len(out)-1] != "ezbx-t-outside" {
		t.Error("outside network:", out)
	}
	if v := p.HomeVolumeArgs(); !has(v, "--label", "ezbx.managed=true") || v[len(v)-1] != "ezbx-t-home" {
		t.Error("volume:", v)
	}
}

func TestGatewayArgs(t *testing.T) {
	p := testPlan(t)
	p.KeyEnv = "ENGINE_KEY"
	p.UpstreamHost = "localhost:18080"
	a := p.GatewayArgs()
	for _, w := range [][]string{
		{"--name", "ezbx-t-gateway"}, {"--network", "ezbx-t-outside"},
		{"--read-only"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"}, {"--user", "65532:65532"},
		{"--label", "ezbx.managed=true"},
		{"-v", p.TokenFile() + ":/run/ezbx/admin-token:ro"},
		{"-v", p.LogDir() + ":/var/log/ezbx"},
		{"-e", "ENGINE_KEY"},
		{GatewayImage, "gateway", "serve", "--listen", "0.0.0.0:8000"},
		{"--upstream", "http://host.docker.internal:18080"}, {"--engine", "strata"},
		{"--upstream-host", "localhost:18080"}, {"--key-env", "ENGINE_KEY"},
		{"--proxy", "0.0.0.0:3128"}, {"--network", "allow"}, {"--allow", "registry.npmjs.org"},
		{"--admin", "127.0.0.1:8099"}, {"--admin-token-file", "/run/ezbx/admin-token"},
		{"--log", "/var/log/ezbx/gateway.log"},
	} {
		if !has(a, w...) {
			t.Errorf("gateway lacks %v, in\n%v", w, a)
		}
	}
	noPublish(t, a)
	// The key's value is never in the command line: "-e NAME" only.
	for _, x := range a {
		if strings.HasPrefix(x, "ENGINE_KEY=") {
			t.Error("key value in command line")
		}
	}

	p.LogOn, p.KeyEnv, p.UpstreamHost, p.Allow = false, "", "", nil
	a = p.GatewayArgs()
	for _, x := range a {
		if x == "--log" || x == "--key-env" || x == "--upstream-host" || x == "--allow" || strings.Contains(x, "/var/log/ezbx") || x == "-e" {
			t.Errorf("unexpected %q when off: %v", x, a)
		}
	}
}

func TestBoxArgs(t *testing.T) {
	p := testPlan(t)
	p.CPUs = "2"
	a := p.BoxArgs()
	for _, w := range [][]string{
		{"--name", "ezbx-t-box"}, {"--network", "ezbx-t-inside"},
		{"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"},
		{"--pids-limit", "512"}, {"--memory", "4g"}, {"--cpus", "2"},
		{"--label", "ezbx.managed=true"},
		{"-e", "HTTPS_PROXY=http://gateway:3128"}, {"-e", "http_proxy=http://gateway:3128"}, {"-e", "NO_PROXY=gateway"},
		{"-v", p.Workspace + ":/workspace"}, {"-v", "ezbx-t-home:/home/agent"},
		{BoxImage, "sleep", "infinity"},
	} {
		if !has(a, w...) {
			t.Errorf("box lacks %v, in\n%v", w, a)
		}
	}
	noPublish(t, a)
	nets, mounts := 0, 0
	for i, x := range a {
		if x == "--network" {
			nets++
		}
		if x == "-v" {
			mounts++
		}
		// The state folder holds the admin token: never in the box.
		if strings.Contains(x, p.StateDir) {
			t.Errorf("state folder in box args at %d: %q", i, x)
		}
	}
	if nets != 1 || mounts != 2 {
		t.Errorf("box must have one network and two mounts, got %d and %d", nets, mounts)
	}
	p.CPUs = ""
	if has(p.BoxArgs(), "--cpus") {
		t.Error("--cpus without a limit")
	}
}

func TestHomeTar(t *testing.T) {
	p := testPlan(t)
	data, err := p.HomeTar()
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(data))
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
		if h.Uid != 1000 || h.Gid != 1000 {
			t.Errorf("%s owned by %d:%d", h.Name, h.Uid, h.Gid)
		}
		if h.Typeflag == tar.TypeDir && h.Mode != 0o700 {
			t.Errorf("%s mode %o", h.Name, h.Mode)
		}
		if h.Typeflag == tar.TypeReg {
			body, _ := io.ReadAll(tr)
			if h.Mode != 0o600 || len(body) == 0 {
				t.Errorf("%s mode %o, %d bytes", h.Name, h.Mode, len(body))
			}
		}
	}
	want := ".pi/ .pi/agent/ .pi/agent/models.json .pi/agent/settings.json"
	if strings.Join(names, " ") != want {
		t.Errorf("got %v, want %s", names, want)
	}
	for _, bad := range []string{"../x", "/etc/passwd", ".pi/../../x", "a//b", "."} {
		p.HomeFiles = []harness.File{{Path: bad, Mode: 0o600}}
		if _, err := p.HomeTar(); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if got := p.HomeCopyArgs(); strings.Join(got, " ") != "cp -a - ezbx-t-box:/home/agent" {
		t.Error(got)
	}
}

func TestGatewayUpstream(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:18080":     "http://host.docker.internal:18080",
		"http://localhost:18080/":    "http://host.docker.internal:18080",
		"http://[::1]:18080":         "http://host.docker.internal:18080",
		"http://127.0.0.1":           "http://host.docker.internal:80",
		"http://192.0.2.10:18080":    "http://192.0.2.10:18080",
		"https://engine.example:443": "https://engine.example:443",
	}
	for in, want := range cases {
		got, err := GatewayUpstream(in)
		if err != nil || got != want {
			t.Errorf("%s: got %s %v, want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"ftp://x", "127.0.0.1:80", ""} {
		if _, err := GatewayUpstream(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestCheckWorkspace(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "example")
	state := filepath.Join(home, "appdata", "ezbx")
	sys := filepath.Join(root, "windows")
	ok := []string{filepath.Join(home, "ezbx", "workspace"), filepath.Join(root, "work")}
	for _, ws := range ok {
		if err := CheckWorkspace(ws, home, []string{state}, []string{sys}); err != nil {
			t.Errorf("%s: %v", ws, err)
		}
	}
	vol := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		vol = filepath.VolumeName(root) + `\`
	}
	bad := map[string]string{
		vol:                            "root of a drive",
		home:                           "home folder",
		filepath.Dir(home):             "home folder",
		state:                          "EZBox's own folder",
		filepath.Join(state, "x"):      "EZBox's own folder",
		filepath.Join(home, "appdata"): "EZBox's own folder",
		filepath.Join(sys, "system32"): "system folder",
		"relative":                     "full path",
	}
	for ws, want := range bad {
		err := CheckWorkspace(ws, home, []string{state}, []string{sys})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", ws, err, want)
		}
	}
}

func TestAllowList(t *testing.T) {
	m := []harness.Manifest{{Network: harness.Network{Domains: []string{"api.example.com", "registry.npmjs.org"}}}}
	got := AllowList([]string{"Registry.npmjs.org", "github.com"}, m)
	if strings.Join(got, ",") != "api.example.com,github.com,registry.npmjs.org" {
		t.Error(got)
	}
}

func TestEnsureToken(t *testing.T) {
	f := filepath.Join(t.TempDir(), "admin-token")
	made, err := EnsureToken(f)
	if err != nil || !made {
		t.Fatal(made, err)
	}
	data, _ := os.ReadFile(f)
	if b, err := hex.DecodeString(string(data)); err != nil || len(b) != 32 {
		t.Errorf("token %d chars, %v", len(data), err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %o", st.Mode().Perm())
		}
	}
	made, err = EnsureToken(f)
	again, _ := os.ReadFile(f)
	if err != nil || made || string(again) != string(data) {
		t.Error("an existing token must be kept")
	}
	os.WriteFile(f, []byte("short"), 0o600)
	if _, err := EnsureToken(f); err == nil {
		t.Error("a short token must be refused")
	}
}

// fakeDocker answers like the docker command for a few resources.
type fakeDocker struct {
	containers map[string]bool // name -> managed
	networks   map[string]bool
	volumes    map[string]bool
	insideJSON string
	health     []string // answers to the health check, in turn
	calls      [][]string
	stdin      []byte
	fail       string // fail any command whose words start with this
}

func newFake() *fakeDocker {
	return &fakeDocker{containers: map[string]bool{}, networks: map[string]bool{}, volumes: map[string]bool{},
		insideJSON: `[{"Internal":true,"EnableIPv6":false,"Options":{"com.docker.network.bridge.gateway_mode_ipv4":"isolated","com.docker.network.enable_ipv6":"false"}}]`,
		health:     []string{"true|starting", "true|healthy"}}
}

func list(m map[string]bool) []byte {
	var b strings.Builder
	for n, managed := range m {
		b.WriteString(n + "|")
		if managed {
			b.WriteString("true")
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func (f *fakeDocker) run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	line := strings.Join(args, " ")
	if f.fail != "" && strings.HasPrefix(line, f.fail) {
		return nil, errors.New("boom")
	}
	switch {
	case line == "version --format {{.Server.Version}}":
		return []byte("29.9.3\n"), nil
	case strings.HasPrefix(line, "image inspect"):
		return []byte("sha256:x"), nil
	case strings.HasPrefix(line, "container ls"):
		return list(f.containers), nil
	case strings.HasPrefix(line, "network ls"):
		return list(f.networks), nil
	case strings.HasPrefix(line, "volume ls"):
		return list(f.volumes), nil
	case strings.HasPrefix(line, "network inspect"):
		return []byte(f.insideJSON), nil
	case strings.HasPrefix(line, "network create"):
		f.networks[args[len(args)-1]] = true
	case strings.HasPrefix(line, "volume create"):
		f.volumes[args[len(args)-1]] = true
	case args[0] == "create":
		for i, a := range args {
			if a == "--name" {
				f.containers[args[i+1]] = true
			}
		}
	case strings.HasPrefix(line, "container inspect"):
		h := f.health[0]
		if len(f.health) > 1 {
			f.health = f.health[1:]
		}
		return []byte(h), nil
	case args[0] == "cp":
		f.stdin = stdin
	case strings.HasPrefix(line, "logs"):
		return []byte("cannot read the admin token file\n"), nil
	case strings.HasPrefix(line, "container rm"):
		delete(f.containers, args[len(args)-1])
	case strings.HasPrefix(line, "network rm"):
		delete(f.networks, args[len(args)-1])
	}
	return nil, nil
}

func (f *fakeDocker) did(words ...string) bool {
	for _, c := range f.calls {
		if len(c) >= len(words) && has(c[:len(words)], words...) {
			return true
		}
	}
	return false
}

func runner(f *fakeDocker) *Runner {
	return &Runner{Docker: f.run, Out: io.Discard, Wait: time.Second, Sleep: func(time.Duration) {}}
}

func TestUpCreatesEverything(t *testing.T) {
	f := newFake()
	p := testPlan(t)
	if err := runner(f).Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range f.calls {
		switch {
		case c[0] == "create" || c[0] == "start" || c[0] == "cp" || (len(c) > 1 && (c[1] == "create" || c[1] == "connect")):
			order = append(order, c[0]+" "+c[len(c)-1])
		}
	}
	want := []string{
		"network ezbx-t-inside", "network ezbx-t-outside", "volume ezbx-t-home",
		"create " + p.GatewayArgs()[len(p.GatewayArgs())-1], "network ezbx-t-gateway", "start ezbx-t-gateway",
		"create infinity", "cp ezbx-t-box:/home/agent", "start ezbx-t-box",
	}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Errorf("order:\n%s\nwant:\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
	if len(f.stdin) == 0 {
		t.Error("no harness config was copied")
	}
	if _, err := os.Stat(p.TokenFile()); err != nil {
		t.Error("no token:", err)
	}
	for _, d := range []string{p.Workspace, p.LogDir()} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Error("missing folder", d)
		}
	}
}

func TestUpRefusesWhatItDidNotMake(t *testing.T) {
	isolated := `"com.docker.network.bridge.gateway_mode_ipv4":"isolated"`
	inside := func(j string) func(f *fakeDocker) {
		return func(f *fakeDocker) { f.networks["ezbx-t-inside"] = true; f.insideJSON = j }
	}
	cases := map[string]struct {
		set  func(f *fakeDocker)
		want string
	}{
		"foreign gateway":        {func(f *fakeDocker) { f.containers["ezbx-t-gateway"] = false }, "didn't create it"},
		"foreign box":            {func(f *fakeDocker) { f.containers["ezbx-t-box"] = false }, "didn't create it"},
		"foreign network":        {func(f *fakeDocker) { f.networks["ezbx-t-inside"] = false }, "didn't create it"},
		"foreign volume":         {func(f *fakeDocker) { f.volumes["ezbx-t-home"] = false }, "didn't create it"},
		"already up":             {func(f *fakeDocker) { f.containers["ezbx-t-box"] = true }, "already up"},
		"inside not isolated":    {inside(`[{"Internal":true,"Options":{}}]`), "isn't isolated"},
		"inside not internal":    {inside(`[{"Internal":false,"Options":{` + isolated + `}}]`), "isn't isolated"},
		"inside has ipv6":        {inside(`[{"Internal":true,"EnableIPv6":true,"Options":{` + isolated + `}}]`), "isn't isolated"},
		"inside has ipv6 option": {inside(`[{"Internal":true,"Options":{` + isolated + `,"com.docker.network.enable_ipv6":"true"}}]`), "isn't isolated"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			tc.set(f)
			before := len(f.containers)
			if err := runner(f).Up(context.Background(), testPlan(t)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if f.did("create") || f.did("start") || f.did("container", "rm") || f.did("network", "rm") || len(f.containers) != before {
				t.Errorf("up changed containers: %v", f.calls)
			}
		})
	}
}

func TestUpReusesItsOwnNetworks(t *testing.T) {
	f := newFake()
	f.networks["ezbx-t-inside"], f.networks["ezbx-t-outside"] = true, true
	f.insideJSON = `[{"Internal":true,"EnableIPv6":false,"Options":{"com.docker.network.bridge.gateway_mode_ipv4":"isolated"}}]`
	// The outside network answers with the same JSON in this fake, so make it
	// look like a normal network only when asked about it.
	p := testPlan(t)
	r := runner(f)
	inner := f.run
	r.Docker = func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "network" && args[1] == "inspect" && args[2] == "ezbx-t-outside" {
			return []byte(`[{"Internal":false}]`), nil
		}
		return inner(ctx, stdin, args...)
	}
	if err := r.Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if f.did("network", "create") {
		t.Error("existing networks should be kept")
	}
}

func TestUpGatewayFails(t *testing.T) {
	for name, health := range map[string][]string{
		"stopped":   {"false|"},
		"unhealthy": {"true|starting", "true|unhealthy"},
		"slow":      {"true|starting"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			f.health = health
			err := runner(f).Up(context.Background(), testPlan(t))
			if err == nil || !strings.Contains(err.Error(), "cannot read the admin token file") || !strings.Contains(err.Error(), "ezbx down") {
				t.Fatalf("got %v", err)
			}
			if f.did("start", "ezbx-t-box") {
				t.Error("the box must not start without a healthy gateway")
			}
			checks := 0
			for _, c := range f.calls {
				if len(c) > 1 && c[0] == "container" && c[1] == "inspect" {
					checks++
				}
			}
			if name == "unhealthy" && checks != 2 {
				t.Errorf("should stop at the first unhealthy answer, asked %d times", checks)
			}
		})
	}
}

func TestUpOldDocker(t *testing.T) {
	f := newFake()
	r := runner(f)
	inner := f.run
	r.Docker = func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		if args[0] == "version" {
			return []byte("27.5.1"), nil
		}
		return inner(ctx, stdin, args...)
	}
	if err := r.Up(context.Background(), testPlan(t)); err == nil || !strings.Contains(err.Error(), "28 or later") {
		t.Fatal(err)
	}
}

func TestDown(t *testing.T) {
	f := newFake()
	for _, n := range []string{"ezbx-t-gateway", "ezbx-t-box"} {
		f.containers[n] = true
	}
	f.networks["ezbx-t-inside"], f.networks["ezbx-t-outside"] = true, true
	f.volumes["ezbx-t-home"] = true
	f.containers["ezbx-t-gateway-old"] = true // a name that only starts the same
	if err := runner(f).Down(context.Background(), "t"); err != nil {
		t.Fatal(err)
	}
	if len(f.networks) != 0 || len(f.containers) != 1 || !f.containers["ezbx-t-gateway-old"] {
		t.Errorf("left: %v %v", f.containers, f.networks)
	}
	if !f.volumes["ezbx-t-home"] || f.did("volume", "rm") {
		t.Error("the home volume must be kept")
	}
	// The box goes before the networks it is attached to.
	first := ""
	for _, c := range f.calls {
		if len(c) > 1 && c[1] == "rm" {
			first = c[len(c)-1]
			break
		}
	}
	if first != "ezbx-t-box" {
		t.Error("removed first:", first)
	}
}

func TestDownLeavesForeignAlone(t *testing.T) {
	f := newFake()
	f.containers["ezbx-t-box"] = false
	f.containers["ezbx-t-gateway"] = true
	f.networks["ezbx-t-inside"] = false
	err := runner(f).Down(context.Background(), "t")
	if err == nil || !strings.Contains(err.Error(), "container ezbx-t-box") || !strings.Contains(err.Error(), "network ezbx-t-inside") {
		t.Fatal(err)
	}
	if _, ok := f.containers["ezbx-t-box"]; !ok {
		t.Error("removed a container EZBox didn't make")
	}
	if _, ok := f.networks["ezbx-t-inside"]; !ok {
		t.Error("removed a network EZBox didn't make")
	}
	if _, ok := f.containers["ezbx-t-gateway"]; ok {
		t.Error("its own gateway should still be removed")
	}
}

func TestDownNothing(t *testing.T) {
	if err := runner(newFake()).Down(context.Background(), "t"); err != nil {
		t.Fatal(err)
	}
}

// Docker's name filter matches parts of names, so look-alikes must not count.
func TestLookAlikeNames(t *testing.T) {
	look := func(f *fakeDocker) {
		f.containers["ezbx-t-gateway-old"] = true
		f.containers["ezbx-t-box2"] = false
		f.networks["ezbx-t-inside-old"] = false
		f.volumes["ezbx-t-home-old"] = false
	}
	f := newFake()
	look(f)
	if err := runner(f).Down(context.Background(), "t"); err != nil {
		t.Fatal("down:", err)
	}
	if f.did("container", "rm") || f.did("network", "rm") {
		t.Error("down removed a look-alike:", f.calls)
	}
	f = newFake()
	look(f)
	if err := runner(f).Up(context.Background(), testPlan(t)); err != nil {
		t.Fatal("up:", err)
	}
	if !f.did("network", "create") || !f.did("volume", "create") {
		t.Error("up should create its own networks and volume beside look-alikes")
	}
}
