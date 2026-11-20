package paths

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func env(m map[string]string) Env { return func(k string) string { return m[k] } }

func TestFind(t *testing.T) {
	home := filepath.Join(string(filepath.Separator)+"home", "example")
	local := filepath.Join(string(filepath.Separator) + "local")
	root := t.TempDir()
	abs := func(p ...string) string { return filepath.Join(append([]string{root}, p...)...) }
	cases := []struct {
		name, goos string
		env        map[string]string
		config, st string
	}{
		{"windows", "windows", map[string]string{"LOCALAPPDATA": local}, filepath.Join(local, "ezbx"), filepath.Join(local, "ezbx", "state")},
		{"windows without LOCALAPPDATA", "windows", nil, filepath.Join(home, "AppData", "Local", "ezbx"), filepath.Join(home, "AppData", "Local", "ezbx", "state")},
		{"windows ignores roaming", "windows", map[string]string{"LOCALAPPDATA": local, "APPDATA": abs("roaming")}, filepath.Join(local, "ezbx"), filepath.Join(local, "ezbx", "state")},
		{"mac", "darwin", nil, filepath.Join(home, "Library", "Application Support", "ezbx"), filepath.Join(home, "Library", "Application Support", "ezbx", "state")},
		{"linux", "linux", nil, filepath.Join(home, ".config", "ezbx"), filepath.Join(home, ".local", "state", "ezbx")},
		{"linux xdg", "linux", map[string]string{"XDG_CONFIG_HOME": abs("c"), "XDG_STATE_HOME": abs("s")}, abs("c", "ezbx"), abs("s", "ezbx")},
		{"linux relative xdg ignored", "linux", map[string]string{"XDG_STATE_HOME": "rel"}, filepath.Join(home, ".config", "ezbx"), filepath.Join(home, ".local", "state", "ezbx")},
		{"EZBX_HOME wins", "windows", map[string]string{"EZBX_HOME": abs("h"), "LOCALAPPDATA": local}, abs("h"), abs("h", "state")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Find(tc.goos, env(tc.env), home)
			if err != nil {
				t.Fatal(err)
			}
			if d.Config != tc.config || d.State != tc.st {
				t.Errorf("got %+v, want config %s state %s", d, tc.config, tc.st)
			}
		})
	}
	if _, err := Find("linux", env(map[string]string{"EZBX_HOME": "relative"}), home); err == nil {
		t.Error("a relative EZBX_HOME should be refused")
	}
	if _, err := Find("linux", env(nil), ""); err == nil {
		t.Error("no home and no XDG folders should be an error")
	}
}

func TestConfigFile(t *testing.T) {
	d := Dirs{Config: filepath.Join("c")}
	if got := d.ConfigFile("flag.toml", env(map[string]string{"EZBX_CONFIG": "env.toml"})); got != "flag.toml" {
		t.Error("flag should win:", got)
	}
	if got := d.ConfigFile("", env(map[string]string{"EZBX_CONFIG": "env.toml"})); got != "env.toml" {
		t.Error("env should be next:", got)
	}
	if got := d.ConfigFile("", env(nil)); got != filepath.Join("c", "ezbx.toml") {
		t.Error("then the config folder:", got)
	}
}

func TestExpandHome(t *testing.T) {
	h := filepath.Join(string(filepath.Separator)+"home", "x")
	cases := map[string]string{
		"~":      h,
		"~/a/b":  filepath.Join(h, "a", "b"),
		`~\a`:    filepath.Join(h, "a"),
		"~other": "~other",
		"/abs":   "/abs",
		"a/~/b":  "a/~/b",
	}
	for in, want := range cases {
		if got := ExpandHome(in, h); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestWithin(t *testing.T) {
	r := string(filepath.Separator)
	// On Windows, use the drive of the temp folder rather than a written-in one.
	vol := filepath.VolumeName(t.TempDir())
	if runtime.GOOS == "windows" {
		r = vol + `\`
	}
	a := filepath.Join(r, "a")
	cases := []struct {
		path, dir string
		want      bool
	}{
		{a, a, true},
		{filepath.Join(a, "b"), a, true},
		{filepath.Join(r, "ab"), a, false},
		{r, a, false},
		{filepath.Join(r, "..a"), r, true},
	}
	for _, tc := range cases {
		if got := Within(tc.path, tc.dir); got != tc.want {
			t.Errorf("Within(%q, %q) = %v", tc.path, tc.dir, got)
		}
	}
	if runtime.GOOS == "windows" {
		if !Within(strings.ToLower(vol)+`\A\b`, strings.ToUpper(vol)+`\a`) {
			t.Error("Windows paths differ only in case here")
		}
		other := string(rune(vol[0])+1) + vol[1:] // the next drive letter
		if Within(other+`\a`, vol+`\a`) {
			t.Error("different drives")
		}
	}
}
