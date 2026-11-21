package setup

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/detect"
	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/internal/tools"
)

const gib = int64(1 << 30)

func TestMemoryFor(t *testing.T) {
	cases := map[int64]string{0: "2g", 3 * gib: "2g", 4 * gib: "2g", 6 * gib: "3g", 10*gib + 5: "5g", 31*gib + gib/3: "8g", 100 * gib: "8g"}
	for in, want := range cases {
		if got := MemoryFor(in); got != want {
			t.Errorf("%d bytes: got %s, want %s", in, got, want)
		}
	}
}

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

func TestConfigTextRoundTrip(t *testing.T) {
	a := Answers{
		Box: "main", Workspace: `C:\Users\Example\my "work"` + "\tspace", Memory: "8g", CPUs: 2.5, PIDs: 1024,
		Network: "allow", Allow: []string{"registry.npmjs.org", "*.example.com"}, Harnesses: []string{"pi"},
		Tools:     []string{"pack:coding", "ffmpeg"},
		Engine:    "strata",
		Overrides: map[string]config.EngineURL{"strata": {URL: "http://127.0.0.1:53000", KeyEnv: "STRATA_KEY", Host: "localhost:53000"}, "ollama": {}},
		Models:    map[string]config.Model{"big-model": {Reasoning: yes(), MaxTokens: 8192, ContextWindow: 32768, Name: "Big"}, "small": {Reasoning: no()}, "empty": {}},
		Log:       no(),
	}
	text := ConfigText(a, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	c, err := config.Parse(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	back := FromConfig(c)
	if back.Workspace != a.Workspace || back.Memory != "8g" || back.CPUs != 2.5 || back.PIDs != 1024 || back.Network != "allow" ||
		strings.Join(back.Allow, ",") != "registry.npmjs.org,*.example.com" || back.Engine != "strata" || back.Log == nil || *back.Log {
		t.Errorf("box or engine: %+v", back)
	}
	if strings.Join(back.Tools, ",") != "pack:coding,ffmpeg" {
		t.Errorf("tools: %q", back.Tools)
	}
	if back.Overrides["strata"] != a.Overrides["strata"] {
		t.Errorf("override: %+v", back.Overrides)
	}
	if _, ok := back.Overrides["ollama"]; ok {
		t.Error("an empty override should be left out")
	}
	m := back.Models["big-model"]
	if m.Reasoning == nil || !*m.Reasoning || m.MaxTokens != 8192 || m.ContextWindow != 32768 || m.Name != "Big" {
		t.Errorf("model: %+v", m)
	}
	if s := back.Models["small"]; s.Reasoning == nil || *s.Reasoning {
		t.Error("an explicit reasoning = false must be kept")
	}
	if _, ok := back.Models["empty"]; ok {
		t.Error("an empty model should be left out")
	}
	if !strings.Contains(text, "written by ezbx init on 2026-01-02") || strings.Contains(text, "pids      = 512") {
		t.Error(text)
	}
	// Defaults stay out of the file.
	a.PIDs, a.CPUs, a.Log = 0, 0, nil
	text = ConfigText(a, time.Now())
	if strings.Contains(text, "pids") || strings.Contains(text, "cpus") || !strings.Contains(text, "log = true") {
		t.Error(text)
	}
	// No tools setting stays out; an empty one is written, and read back empty.
	a.Tools = nil
	if text = ConfigText(a, time.Now()); strings.Contains(text, "tools") {
		t.Error(text)
	}
	a.Tools = []string{}
	text = ConfigText(a, time.Now())
	if c, err := config.Parse(text); err != nil || !strings.Contains(text, "tools     = []") {
		t.Errorf("%v\n%s", err, text)
	} else if b := FromConfig(c); b.Tools == nil || len(b.Tools) != 0 {
		t.Errorf("an empty tools list must read back empty, not unset: %#v", b.Tools)
	}
}

func TestQuote(t *testing.T) {
	got := quote("a\"b\x01\x7fé\\")
	if got != `"a\"b\u0001\u007Fé\\"` {
		t.Error(got)
	}
}

func facts(t *testing.T) Facts {
	home := filepath.Join(t.TempDir(), "home")
	pi := harness.Manifest{ID: "pi", Name: "Pi", Install: harness.Install{Version: "1.5.0"}, Tools: []string{"fd"}, Verified: harness.Verified{Status: "verified"}}
	other := harness.Manifest{ID: "other", Name: "Other", Network: harness.Network{Domains: []string{"api.example.com"}}, Verified: harness.Verified{Status: "verified"}}
	return Facts{
		Engines: []detect.Engine{
			{ID: "strata", Name: "Strata", URL: "http://127.0.0.1:53000", Models: []detect.Model{{ID: "big-model"}}},
			{ID: "openai", Name: "OpenAI-compatible server", URL: "http://127.0.0.1:18080", Models: []detect.Model{{ID: "fake"}}},
		},
		Harnesses: []harness.Manifest{other, pi}, DockerMemory: 31 * gib, Home: home,
		Own:     []string{filepath.Join(home, "appdata", "ezbx")},
		Catalog: testCatalog(t),
	}
}

// testCatalog has two verified tools, one unverified, and one pack.
func testCatalog(t *testing.T) *tools.Catalog {
	tool := func(id, status string) *fstest.MapFile {
		v := `{"status": "unverified", "observedOn": "", "notes": ""}`
		if status == "verified" {
			v = `{"status": "verified", "observedOn": "2026-01-02", "notes": "checked"}`
		}
		return &fstest.MapFile{Data: []byte(`{"schema": 1, "id": "` + id + `", "name": "` + id + `", "description": "d", "debian": ["` + id + `"], "commands": ["` + id + `"], "verified": ` + v + `}`)}
	}
	c, err := tools.Load(fstest.MapFS{
		"tools/fd.json": tool("fd", "verified"), "tools/jq.json": tool("jq", "verified"), "tools/wip.json": tool("wip", "unverified"),
		"packs/coding.json": {Data: []byte(`{"schema": 1, "id": "coding", "name": "Coding", "description": "d", "tools": ["fd", "jq"]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func prompter(input string) (*Prompter, *strings.Builder) {
	var out strings.Builder
	return &Prompter{In: bufio.NewReader(strings.NewReader(input)), Out: &out}, &out
}

func TestGatherDefaults(t *testing.T) {
	f := facts(t)
	p, _ := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{}, p)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Answers
	if res.Engine == nil || res.Engine.ID != "strata" || a.Engine != "strata" || strings.Join(a.Harnesses, ",") != "pi" ||
		a.Workspace != filepath.Join(f.Home, "ezbx", "workspace") || a.Network != "off" || a.Memory != "8g" || a.Box != "main" {
		t.Errorf("%+v", a)
	}
	if _, ok := a.Models["big-model"]; ok {
		t.Error("reasoning defaults to no, so nothing is written")
	}
	if strings.Join(a.Tools, ",") != "pack:coding" {
		t.Errorf("a new config starts with the coding pack: %q", a.Tools)
	}
}

// An existing config keeps its tools, and one without a tools setting keeps
// none, unless a flag or an answer says otherwise.
func TestGatherToolsOfExistingConfig(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	base := "version = 1\n[box.main]\nworkspace = '" + ws + "'\nharnesses = ['pi']\n"
	for name, tc := range map[string]struct {
		setting string
		o       Options
		typed   string
		want    string // "<nil>" for not set
	}{
		"unset, default":   {"", Options{}, "", "<nil>"},
		"unset, typed":     {"", Options{}, "jq", "jq"},
		"set, default":     {"tools = ['jq']\n", Options{}, "", "jq"},
		"set, typed none":  {"tools = ['jq']\n", Options{}, "none", ""},
		"unset, flag none": {"", Options{Tools: []string{}}, "", ""},
		"set, flag":        {"tools = ['jq']\n", Options{Tools: []string{"pack:coding"}}, "", "pack:coding"},
	} {
		t.Run(name, func(t *testing.T) {
			f := facts(t)
			c, err := config.Parse(base + tc.setting)
			if err != nil {
				t.Fatal(err)
			}
			f.Existing = c
			// Answers in order: engine, reasoning, harness, tools, then the rest default.
			p, out := prompter("\n\n\n" + tc.typed + "\n\n\n\n")
			res, err := Gather(f, tc.o, p)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := strings.Join(res.Answers.Tools, ",")
			if res.Answers.Tools == nil {
				got = "<nil>"
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGatherTyped(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(t.TempDir(), "ws")
	// engine 2; reasoning: bad answer then y; harnesses 1,2; a bad workspace
	// (the home folder) then a good one; allow with hosts; memory.
	input := strings.Join([]string{"2", "maybe", "y", "1,2,1", "pack:nope", "wip", "pack:coding, jq, jq", f.Home, ws, "allow", "pypi.org, GitHub.com", "6G"}, "\n") + "\n"
	p, out := prompter(input)
	res, err := Gather(f, Options{}, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	a := res.Answers
	if res.Engine.ID != "openai" || a.Engine != "openai" || strings.Join(a.Harnesses, ",") != "other,pi" || a.Workspace != ws ||
		a.Network != "allow" || strings.Join(a.Allow, ",") != "pypi.org,github.com" || a.Memory != "6g" || strings.Join(a.Tools, ",") != "pack:coding,jq" {
		t.Errorf("%+v", a)
	}
	if r := a.Models["fake"].Reasoning; r == nil || !*r {
		t.Error("reasoning should be set")
	}
	for _, want := range []string{"Answer y or n.", "home folder", "api.example.com", `unknown tool pack "nope"`, `"wip" isn't verified`, "Always included for the harnesses: fd"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestGatherFlags(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(t.TempDir(), "ws")
	p, _ := prompter("")
	p.Auto = true
	o := Options{Engine: "strata", URL: "http://127.0.0.1:53000", KeyEnv: "K", Reasoning: true, Harnesses: []string{"pi"}, Workspace: ws, Network: "on", Memory: "3g"}
	res, err := Gather(f, o, p)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Answers
	if a.Engine != "strata" || a.Overrides["strata"].URL != o.URL || a.Overrides["strata"].KeyEnv != "K" || a.Network != "on" || a.Memory != "3g" || a.Workspace != ws {
		t.Errorf("%+v", a)
	}
	if r := a.Models["big-model"].Reasoning; r == nil || !*r {
		t.Error("--reasoning")
	}
	o = Options{Engine: "auto"}
	if res, _ = Gather(f, o, p); res.Answers.Engine != "auto" || res.Engine == nil {
		t.Errorf("--engine auto: %+v", res.Answers.Engine)
	}
	if res, _ = Gather(f, Options{Engine: "later"}, p); res.Engine != nil || res.Answers.Engine != "auto" {
		t.Error("--engine later")
	}
}

func TestGatherRefusals(t *testing.T) {
	f := facts(t)
	auto := func() *Prompter { p, _ := prompter(""); p.Auto = true; return p }
	cases := map[string]Options{
		"unknown harness":    {Harnesses: []string{"nope"}},
		"home as workspace":  {Workspace: f.Home},
		"relative workspace": {Workspace: "work"},
		"own folder":         {Workspace: f.Own[0]},
		"bad network":        {Network: "open"},
		"bad memory":         {Memory: "lots"},
	}
	for name, o := range cases {
		if _, err := Gather(f, o, auto()); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	p, _ := prompter("1\n")
	if _, err := Gather(f, Options{}, p); !errors.Is(err, ErrNoAnswer) {
		t.Errorf("input ending: %v", err)
	}
}

func TestGatherKeepsExisting(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(t.TempDir(), "ws")
	c, err := config.Parse("version = 1\n[box.work]\nworkspace = '" + ws + "'\nmemory = '5g'\ncpus = 3\nnetwork = 'allow'\nallow = ['pypi.org']\nharnesses = ['pi']\n" +
		"[engine]\nuse = 'openai'\n[engine.openai]\nhost = 'localhost:18080'\n[models.fake]\nreasoning = true\nmax_tokens = 4096\n[gateway]\nlog = false\n")
	if err != nil {
		t.Fatal(err)
	}
	f.Existing = c
	p, _ := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{}, p)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Answers
	if a.Box != "work" || a.Engine != "openai" || a.Workspace != ws || a.Memory != "5g" || a.CPUs != 3 || a.Network != "allow" ||
		strings.Join(a.Allow, ",") != "pypi.org" || a.Overrides["openai"].Host != "localhost:18080" || a.Log == nil || *a.Log {
		t.Errorf("%+v", a)
	}
	if m := a.Models["fake"]; m.Reasoning == nil || !*m.Reasoning || m.MaxTokens != 4096 {
		t.Errorf("model: %+v", m)
	}
	if ch := Changes(FromConfig(c), a); len(ch) != 0 {
		t.Errorf("taking every default must change nothing: %v", ch)
	}
	a.Network = "on"
	a.Models["fake"] = config.Model{Reasoning: no(), MaxTokens: 4096}
	ch := Changes(FromConfig(c), a)
	if len(ch) != 2 || !strings.Contains(ch[0], "network") || !strings.Contains(ch[1], "reasoning true") {
		t.Errorf("%v", ch)
	}
}

func TestGatherNoEngine(t *testing.T) {
	f := facts(t)
	f.Engines = nil
	p, out := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{}, p)
	if err != nil || res.Engine != nil || res.Answers.Engine != "auto" {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(out.String(), "none.") {
		t.Error(out.String())
	}
	if res, _ = Gather(f, Options{Engine: "strata"}, p); res.Answers.Engine != "strata" {
		t.Error("a named engine is written even if it isn't running yet")
	}
}

func plan(t *testing.T) Plan {
	f := facts(t)
	e := f.Engines[0]
	return Plan{
		Result:     Result{Engine: &e, Answers: Answers{Box: "main", Engine: "strata", Workspace: filepath.Join(f.Home, "ws"), Network: "off", Memory: "8g", Harnesses: []string{"pi"}}},
		ConfigFile: "ezbx.toml", StateDir: "state",
		Images:    []Image{{Tag: "ezbx-gateway:aaa", Present: true}, {Tag: "ezbx-box:bbb", Present: true}},
		Tools:     []string{"fd"},
		Harnesses: f.Harnesses,
	}
}

func TestPlanStart(t *testing.T) {
	cases := map[string]struct {
		change func(*Plan)
		want   string
	}{
		"ready":       {func(*Plan) {}, ""},
		"no-start":    {func(p *Plan) { p.NoStart = true }, "--no-start"},
		"no engine":   {func(p *Plan) { p.Result.Engine = nil }, "no engine"},
		"no models":   {func(p *Plan) { e := *p.Result.Engine; e.Models = nil; p.Result.Engine = &e }, "no models"},
		"image":       {func(p *Plan) { p.Images[1].Present = false }, ""}, // apply builds it first
		"up, changed": {func(p *Plan) { p.BoxUp = true }, "ezbx down, then ezbx up"},
		"up, same": {func(p *Plan) {
			p.BoxUp = true
			old := p.Result.Answers
			p.Old = &old
		}, "already up"},
		"key empty": {func(p *Plan) {
			p.Result.Answers.Overrides = map[string]config.EngineURL{"strata": {KeyEnv: "K"}}
		}, "K is empty"},
	}
	for name, tc := range cases {
		p := plan(t)
		tc.change(&p)
		ok, why := p.Start()
		if ok != (tc.want == "") || !strings.Contains(why, tc.want) {
			t.Errorf("%s: %v %q", name, ok, why)
		}
	}
}

func TestReview(t *testing.T) {
	p := plan(t)
	p.Images[0].Present = false
	p.Result.Answers.Tools = []string{"pack:coding"}
	p.Tools = []string{"fd", "jq"}
	text := p.Review()
	for _, want := range []string{"nothing has been changed yet", "Strata at http://127.0.0.1:53000", "Pi 1.5.0 (verified)", "(will be created)", "(new)",
		"Tools         fd, jq   (from pack:coding)", "images        ezbx build: ezbx-gateway:aaa (downloads", "then          ezbx up"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	old := p.Result.Answers
	old.Network = "on"
	old.Tools = nil
	p.Old = &old
	p.Images[0].Present = true
	text = p.Review()
	if !strings.Contains(text, "kept as ezbx.toml.previous") || !strings.Contains(text, "network    on -> off") || !strings.Contains(text, "ezbx up (2 networks") ||
		!strings.Contains(text, "tools      (not set) -> pack:coding") || !strings.Contains(text, "ezbx-gateway:aaa and ezbx-box:bbb (built already)") {
		t.Error(text)
	}
	// Not starting: the missing image is named, with how to build it.
	p.NoStart = true
	p.Images[1].Present = false
	if text = p.Review(); !strings.Contains(text, "not built yet: ezbx-box:bbb. ezbx build makes them") {
		t.Error(text)
	}
}

func TestPrompterAutoShowsAnswers(t *testing.T) {
	p, out := prompter("")
	p.Auto = true
	if v, err := p.YesNo("Ok?", false); err != nil || v {
		t.Fatal(v, err)
	}
	if !strings.Contains(out.String(), "Ok? (y/n) [n] n") {
		t.Error(out.String())
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func TestGatherNamedEngineNotRunning(t *testing.T) {
	f := facts(t)
	p, out := prompter("")
	p.Auto = true
	res, err := Gather(f, Options{Engine: "ollama"}, p)
	if err != nil || res.Engine != nil || res.Answers.Engine != "ollama" || !strings.Contains(out.String(), "not running now") {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
}

func TestGatherAllowHostsAreChecked(t *testing.T) {
	f := facts(t)
	ws := filepath.Join(t.TempDir(), "ws")
	input := strings.Join([]string{"1", "n", "2", "none", ws, "allow", "10.0.0.1, pypi.org", "pypi.org", "4g"}, "\n") + "\n"
	p, out := prompter(input)
	res, err := Gather(f, Options{}, p)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(res.Answers.Allow, ",") != "pypi.org" || !strings.Contains(out.String(), `"10.0.0.1" is not a host name`) {
		t.Errorf("%v\n%s", res.Answers.Allow, out)
	}
}
