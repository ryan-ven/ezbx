package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryan-ven/ezbx/internal/config"
)

func TestParseInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b := fs.String("box", "", "")
	words, rest, err := parseInterleaved(fs, []string{"allow", "--box", "x", "github.com", "--", "--box", "y", "-p"})
	if err != nil || *b != "x" || strings.Join(words, " ") != "allow github.com" || strings.Join(rest, " ") != "--box y -p" {
		t.Errorf("words %v rest %v box %q err %v", words, rest, *b, err)
	}
	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("box", "", "")
	if _, _, err := parseInterleaved(fs, []string{"on", "--bogus"}); err == nil {
		t.Error("unknown flags are refused")
	}
}

func cfgWith(t *testing.T, harnesses string) *config.Config {
	c, err := config.Parse("version = 1\n[box.t]\nworkspace = '/w'\nharnesses = " + harnesses + "\n")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestShellArgs(t *testing.T) {
	tg := target{name: "t", cfg: cfgWith(t, `["pi"]`)}
	got, err := shellArgs(tg, "", nil, true)
	if err != nil || strings.Join(got, " ") != "exec -i -t -w /workspace ezbx-t-box bash" {
		t.Errorf("%v %v", got, err)
	}
	got, err = shellArgs(tg, "pi", []string{"-p", "hi"}, false)
	if err != nil || strings.Join(got, " ") != "exec -i -w /workspace -e PI_OFFLINE=1 -e PI_SKIP_VERSION_CHECK=1 ezbx-t-box pi -p hi" {
		t.Errorf("%v %v", got, err)
	}
	for _, a := range got {
		if a == "-u" || a == "--user" || a == "--privileged" || strings.HasPrefix(a, "--user=") {
			t.Errorf("shell must run as the box's own user: %v", got)
		}
	}
	if _, err := shellArgs(tg, "nope", nil, true); err == nil {
		t.Error("unknown harness")
	}
	if _, err := shellArgs(tg, "", []string{"x"}, true); err == nil {
		t.Error("words after -- without a harness")
	}
	tg.cfg = cfgWith(t, `["other"]`)
	if _, err := shellArgs(tg, "pi", nil, true); err == nil || !strings.Contains(err.Error(), "doesn't have Pi") {
		t.Errorf("a harness the box doesn't have: %v", err)
	}
	tg.cfg = nil // no config: any known harness
	if _, err := shellArgs(tg, "pi", nil, true); err != nil {
		t.Error(err)
	}
}

func TestEngineChoice(t *testing.T) {
	c, err := config.Parse("version = 1\n[box.t]\nworkspace = '/w'\nharnesses = ['pi']\n[engine.strata]\nkey_env = 'K'\n")
	if err != nil {
		t.Fatal(err)
	}
	got, err := engineChoice(c, []string{"strata"}, "http://127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	if got.Engine.Use != "strata" || got.Engine.Overrides["strata"].URL != "http://127.0.0.1:18080" || got.Engine.Overrides["strata"].KeyEnv != "K" {
		t.Errorf("%+v", got.Engine)
	}
	if c.Engine.Use != "auto" || c.Engine.Overrides["strata"].URL != "" {
		t.Error("the loaded config must not change")
	}
	if _, err := engineChoice(c, nil, "http://127.0.0.1:18080"); err == nil {
		t.Error("--url without an id")
	}
	if _, err := engineChoice(c, []string{"strata"}, "127.0.0.1:18080"); err == nil {
		t.Error("--url without http")
	}
}

func TestGatewayAllowFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "hosts")
	os.WriteFile(bad, []byte("github.com\n10.0.0.1\n"), 0o600)
	for name, file := range map[string]string{"missing": filepath.Join(dir, "none"), "broken": bad} {
		var errOut strings.Builder
		if code := run([]string{"gateway", "serve", "--upstream", "http://127.0.0.1:1", "--allow-file", file}, io.Discard, &errOut); code != 2 {
			t.Errorf("%s: exit %d (%s)", name, code, errOut.String())
		}
	}
}

func TestResolveBox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EZBX_HOME", home)
	t.Setenv("EZBX_CONFIG", "")
	tg, err := resolveBox("", "")
	if err != nil || tg.name != "main" || tg.cfg != nil {
		t.Errorf("no config: %+v %v", tg, err)
	}
	os.WriteFile(filepath.Join(home, "ezbx.toml"), []byte("version = 1\n[box.work]\nworkspace = '/w'\nharnesses = ['pi']\n"), 0o600)
	if tg, err = resolveBox("", ""); err != nil || tg.name != "work" || tg.cfg == nil {
		t.Errorf("config: %+v %v", tg, err)
	}
	if tg, _ = resolveBox("", "other"); tg.name != "other" {
		t.Error("--box wins")
	}
	if _, err = resolveBox("", "Bad Name"); err == nil {
		t.Error("bad box name")
	}
	if _, err = resolveBox(filepath.Join(home, "missing.toml"), ""); err == nil {
		t.Error("a named config must load")
	}
}

func TestWithAllowFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(f, []byte("# saved\npypi.org\n"), 0o600)
	got, err := withAllowFile([]string{"github.com"}, f)
	if err != nil || strings.Join(got, ",") != "github.com,pypi.org" {
		t.Errorf("%v %v", got, err)
	}
	if got, _ := withAllowFile([]string{"github.com"}, ""); strings.Join(got, ",") != "github.com" {
		t.Error(got)
	}
}
