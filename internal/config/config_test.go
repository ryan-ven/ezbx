package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `
version = 1

[box.main]
workspace = '~/ezbx/workspace'
memory    = "8G"
cpus      = 2.5
network   = "allow"
allow     = ["registry.npmjs.org", "*.example.com"]
harnesses = ["pi"]

[engine]
use = "strata"

[engine.strata]
url     = "http://127.0.0.1:18080"
key_env = "STRATA_API_KEY"
host    = "localhost:18080"

[models."big-model"]
reasoning  = true
max_tokens = 8192

[gateway]
log = false
`

func TestParseGood(t *testing.T) {
	c, err := Parse(good)
	if err != nil {
		t.Fatal(err)
	}
	name, b := c.Only()
	if name != "main" || b.Workspace != "~/ezbx/workspace" || b.Memory != "8g" || b.CPUsFlag() != "2.5" || b.PIDs != DefaultPIDs {
		t.Errorf("box: %s %+v", name, b)
	}
	if b.Network != "allow" || len(b.Allow) != 2 || b.Harnesses[0] != "pi" {
		t.Errorf("box: %+v", b)
	}
	o := c.Engine.Overrides["strata"]
	if c.Engine.Use != "strata" || o.URL != "http://127.0.0.1:18080" || o.KeyEnv != "STRATA_API_KEY" || o.Host != "localhost:18080" {
		t.Errorf("engine: %+v", c.Engine)
	}
	m := c.Models["big-model"]
	if m.Reasoning == nil || !*m.Reasoning || m.MaxTokens != 8192 {
		t.Errorf("model: %+v", m)
	}
	if c.LogOn() {
		t.Error("log should be off")
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse("version = 1\n[box.main]\nworkspace = '/w'\nharnesses = [\"pi\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	_, b := c.Only()
	if b.Memory != "" || b.PIDs != DefaultPIDs || b.Network != "off" || b.CPUsFlag() != "" {
		t.Errorf("defaults: %+v", b)
	}
	if c.Engine.Use != "auto" || !c.LogOn() {
		t.Errorf("engine %q, log %v", c.Engine.Use, c.LogOn())
	}
	if b.Tools != nil {
		t.Errorf("no tools key means not set: %#v", b.Tools)
	}
	c, err = Parse("version = 1\n[box.main]\nworkspace = '/w'\nharnesses = [\"pi\"]\ntools = ['pack:coding', 'ffmpeg']\n")
	if _, b = c.Only(); err != nil || strings.Join(b.Tools, ",") != "pack:coding,ffmpeg" {
		t.Errorf("tools: %v %v", b.Tools, err)
	}
}

// The shipped example must stay loadable, apart from the read-only folder,
// which is refused until masking is built.
func TestExampleFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "ezbx.toml.example"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(string(data))
	if err == nil || !strings.Contains(err.Error(), "readonly folders are not supported yet") {
		t.Fatalf("example: %v", err)
	}
}

func TestRefused(t *testing.T) {
	base := "version = 1\n[box.main]\nworkspace = '/w'\nharnesses = [\"pi\"]\n"
	cases := map[string]struct{ text, want string }{
		"typo at top":      {"verison = 2\n" + base, "unknown setting: verison"},
		"typo in box":      {strings.Replace(base, "workspace", "netwrok = 'on'\nworkspace", 1), "unknown setting: box.main.netwrok"},
		"typo in engine":   {base + "[engine.strata]\nurll = 'http://x'\n", "unknown setting: engine.strata.urll"},
		"typo in model":    {base + "[models.m]\nreasonin = true\n", "unknown setting: models.m.reasonin"},
		"typo in gateway":  {base + "[gateway]\nlogs = true\n", "unknown setting: gateway.logs"},
		"wrong version":    {strings.Replace(base, "version = 1", "version = 2", 1), "version must be 1"},
		"no box":           {"version = 1\n", "no box"},
		"two boxes":        {base + "[box.other]\nworkspace = '/x'\nharnesses = ['pi']\n", "this version runs one box"},
		"bad box name":     {strings.Replace(base, "box.main", "box.Main", 1), "box name"},
		"no workspace":     {strings.Replace(base, "workspace = '/w'", "", 1), "workspace is required"},
		"bad memory":       {strings.Replace(base, "workspace", "memory = 'lots'\nworkspace", 1), "memory"},
		"bad network":      {strings.Replace(base, "workspace", "network = 'open'\nworkspace", 1), "network"},
		"bad allow host":   {strings.Replace(base, "workspace", "allow = ['192.0.2.1:80']\nworkspace", 1), "allow"},
		"allow address":    {strings.Replace(base, "workspace", "allow = ['192.0.2.1']\nworkspace", 1), "allow"},
		"no harness":       {strings.Replace(base, `harnesses = ["pi"]`, "harnesses = []", 1), "harnesses is empty"},
		"readonly":         {base + "[[box.main.readonly]]\npath = '/p'\n", "readonly folders are not supported yet"},
		"key not env name": {base + "[engine.strata]\nkey_env = 'sk-abc def'\n", "NAME of an environment variable"},
		"url without http": {base + "[engine.strata]\nurl = '127.0.0.1:18080'\n", "must start with http"},
		"engine not table": {base + "[engine]\nstrata = 'http://x'\n", "must be a table"},
		"few pids":         {strings.Replace(base, "workspace", "pids = 10\nworkspace", 1), "pids"},
		"bad toml":         {base + "memory = \n", "line"},
		"negative context": {base + "[models.m]\ncontext = -1\n", "negative"},
		"bad tool":         {strings.Replace(base, "workspace", "tools = ['FFmpeg']\nworkspace", 1), "tools \"FFmpeg\""},
		"bad pack prefix":  {strings.Replace(base, "workspace", "tools = ['packs:coding']\nworkspace", 1), "tools \"packs:coding\""},
		"tool with path":   {strings.Replace(base, "workspace", "tools = ['../x']\nworkspace", 1), "tools \"../x\""},
		"skip a path":      {base + "[box.main.snapshots]\nskip = ['a/node_modules']\n", "not a path"},
		"skip backslash":   {base + "[box.main.snapshots]\nskip = ['a\\\\b']\n", "not a path"},
		"skip dot-dot":     {base + "[box.main.snapshots]\nskip = ['..']\n", "not a path"},
		"skip bad pattern": {base + "[box.main.snapshots]\nskip = ['[x']\n", "syntax error"},
		"bad max_file":     {base + "[box.main.snapshots]\nmax_file = 'huge'\n", "max_file"},
		"negative keep":    {base + "[box.main.snapshots]\nkeep = -1\n", "keep can't be negative"},
		"snapshot typo":    {base + "[box.main.snapshots]\nkeeep = 3\n", "unknown setting: box.main.snapshots.keeep"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestHostName(t *testing.T) {
	good := []string{"github.com", "registry.npmjs.org", "*.example.com", "a-b.example.co.uk", "1password.com", "x.y2k"}
	bad := []string{"", "com", "*.com", "*", "10.0.0.1", "192.0.2.1", "1.2.3", "example.com:443", "Example.com", "-x.com", "x-.com", "http://x.com", "x..com", "*.*.com", "::1", "[::1]"}
	for _, h := range good {
		if !HostName(h) {
			t.Errorf("%q should be accepted", h)
		}
	}
	for _, h := range bad {
		if HostName(h) {
			t.Errorf("%q should be refused", h)
		}
	}
}
