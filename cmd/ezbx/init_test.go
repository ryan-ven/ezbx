package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteConfigKeepsThePrevious(t *testing.T) {
	file := filepath.Join(t.TempDir(), "new", "ezbx.toml")
	if err := writeConfig(file, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file + ".previous"); err == nil {
		t.Error("nothing to keep the first time")
	}
	if err := writeConfig(file, "second"); err != nil {
		t.Fatal(err)
	}
	now, _ := os.ReadFile(file)
	prev, _ := os.ReadFile(file + ".previous")
	if string(now) != "second" || string(prev) != "first" {
		t.Errorf("now %q, previous %q", now, prev)
	}
	if _, err := os.Stat(file + ".new"); err == nil {
		t.Error("temporary file left behind")
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{file, file + ".previous"} {
			if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %o", f, st.Mode().Perm())
			}
		}
		if st, _ := os.Stat(filepath.Dir(file)); st.Mode().Perm() != 0o700 {
			t.Errorf("folder mode %o", st.Mode().Perm())
		}
	}
}

func TestEnvName(t *testing.T) {
	for s, want := range map[string]bool{"STRATA_API_KEY": true, "_K": true, "k1": true, "": false, "1K": false, "sk-abc": false, "A B": false} {
		if envName(s) != want {
			t.Errorf("%q: want %v", s, want)
		}
	}
}

func TestInitRefusesBadFlags(t *testing.T) {
	t.Setenv("EZBX_HOME", t.TempDir())
	for name, args := range map[string][]string{
		"url without engine": {"--url", "http://127.0.0.1:1"},
		"key value":          {"--key-env", "sk-abc def"},
		"extra word":         {"now"},
	} {
		if code := run(append([]string{"init"}, args...), devNull{}, devNull{}); code != 2 {
			t.Errorf("%s: exit %d", name, code)
		}
	}
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }
