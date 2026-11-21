package tools

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ryan-ven/ezbx/catalog"
)

// The real catalogue loads, every pack names real tools, and every tool a
// pack offers is verified (a pack must never offer something that can't
// be built).
func TestRealCatalog(t *testing.T) {
	c, err := Load(catalog.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"coding", "web", "media"} {
		if _, ok := c.Packs[want]; !ok {
			t.Errorf("pack %s is missing", want)
		}
	}
	for id, p := range c.Packs {
		for _, tool := range p.Tools {
			if c.Tools[tool].Verified.Status != "verified" {
				t.Errorf("pack %s offers %s, which isn't verified", id, tool)
			}
		}
	}
	if _, err := c.Resolve([]string{"pack:coding", "pack:web", "pack:media"}, []string{"fd", "ripgrep"}); err != nil {
		t.Error(err)
	}
}

const good = `{"schema": 1, "id": "ID", "name": "N", "description": "D", "debian": ["pkg-a"], "commands": ["cmd-a"],
  "verified": {"status": "verified", "observedOn": "2026-01-02", "notes": "checked"}}`

func load(files map[string]string) (*Catalog, error) {
	fsys := fstest.MapFS{}
	for k, v := range files {
		fsys[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return Load(fsys)
}

func tool(id string, change func(string) string) string {
	s := strings.Replace(good, "ID", id, 1)
	if change != nil {
		s = change(s)
	}
	return s
}

func TestLoadRefuses(t *testing.T) {
	repl := func(old, new string) func(string) string {
		return func(s string) string { return strings.Replace(s, old, new, 1) }
	}
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"unknown field":    {map[string]string{"tools/a.json": tool("a", repl(`"name"`, `"extra": 1, "name"`))}, "unknown field"},
		"id vs file":       {map[string]string{"tools/a.json": tool("b", nil)}, "match the file name"},
		"bad id":           {map[string]string{"tools/A.json": tool("A", nil)}, "lowercase"},
		"schema":           {map[string]string{"tools/a.json": tool("a", repl(`"schema": 1`, `"schema": 2`))}, "schema"},
		"no packages":      {map[string]string{"tools/a.json": tool("a", repl(`["pkg-a"]`, `[]`))}, "at least one package"},
		"bad package":      {map[string]string{"tools/a.json": tool("a", repl(`"pkg-a"`, `"pkg a; rm -rf /"`))}, "not a Debian package"},
		"no commands":      {map[string]string{"tools/a.json": tool("a", repl(`["cmd-a"]`, `[]`))}, "at least one command"},
		"bad command":      {map[string]string{"tools/a.json": tool("a", repl(`"cmd-a"`, `"$(x)"`))}, "not a command"},
		"bad link":         {map[string]string{"tools/a.json": tool("a", repl(`"verified"`, `"links": {"x": "../y"}, "verified"`))}, "plain command names"},
		"link unchecked":   {map[string]string{"tools/a.json": tool("a", repl(`"verified"`, `"links": {"x": "cmd-a"}, "verified"`))}, "also be listed in commands"},
		"verified, bare":   {map[string]string{"tools/a.json": tool("a", repl(`"notes": "checked"`, `"notes": ""`))}, "needs observedOn"},
		"verified, date":   {map[string]string{"tools/a.json": tool("a", repl(`2026-01-02`, `soon`))}, "needs observedOn"},
		"bad status":       {map[string]string{"tools/a.json": tool("a", repl(`"status": "verified"`, `"status": "maybe"`))}, "verified or unverified"},
		"pack, bad tool":   {map[string]string{"tools/a.json": tool("a", nil), "packs/p.json": `{"schema": 1, "id": "p", "name": "P", "description": "D", "tools": ["zzz"]}`}, `unknown tool "zzz"`},
		"pack, empty":      {map[string]string{"packs/p.json": `{"schema": 1, "id": "p", "name": "P", "description": "D", "tools": []}`}, "at least one tool"},
		"pack, id vs file": {map[string]string{"tools/a.json": tool("a", nil), "packs/p.json": `{"schema": 1, "id": "q", "name": "P", "description": "D", "tools": ["a"]}`}, "match the file name"},
	}
	for name, tc := range cases {
		if _, err := load(tc.files); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}

func TestResolve(t *testing.T) {
	c, err := load(map[string]string{
		"tools/a.json": tool("a", func(s string) string {
			return strings.Replace(strings.Replace(s, `["cmd-a"]`, `["cmd-a", "alias"]`, 1), `"verified"`, `"links": {"alias": "cmd-a"}, "verified"`, 1)
		}),
		"tools/b.json": tool("b", func(s string) string { return strings.Replace(s, `"pkg-a"`, `"pkg-a", "pkg-b"`, 1) }),
		"tools/c.json": tool("c", func(s string) string {
			return strings.Replace(s, `"status": "verified", "observedOn": "2026-01-02", "notes": "checked"`, `"status": "unverified", "observedOn": "", "notes": ""`, 1)
		}),
		"packs/p.json": `{"schema": 1, "id": "p", "name": "P", "description": "D", "tools": ["b", "a"]}`,
		"packs/b.json": `{"schema": 1, "id": "b", "name": "B", "description": "D", "tools": ["b"]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Resolve([]string{"pack:p", "a"}, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.IDs(), ","); got != "a,b" {
		t.Errorf("ids %s", got)
	}
	if got := strings.Join(s.Debian, " "); got != "pkg-a pkg-b" {
		t.Errorf("debian %q (sorted, no repeats)", got)
	}
	if got := strings.Join(s.Commands, " "); got != "alias cmd-a" {
		t.Errorf("commands %q", got)
	}
	if got := strings.Join(s.Links, " "); got != "alias=cmd-a" {
		t.Errorf("links %q", got)
	}
	// b came from the pack too, but a harness needs it, so it isn't among
	// the box's own choices.
	if got := strings.Join(s.Chosen, ","); got != "a" {
		t.Errorf("chosen %q", got)
	}
	if s, _ := c.Resolve([]string{"b"}, nil); strings.Join(s.Chosen, ",") != "b" {
		t.Errorf("chosen without harness tools: %q", s.Chosen)
	}
	for _, tc := range []struct {
		chosen, needed []string
		want           string
	}{
		{[]string{"zzz"}, nil, `unknown tool "zzz"`},
		{[]string{"b", "p"}, nil, `for the pack, write "pack:p"`},
		{[]string{"pack:zzz"}, nil, `unknown tool pack "zzz"`},
		{[]string{"c"}, nil, `"c" isn't verified`},
		{nil, []string{"zzz"}, `a harness needs the tool "zzz"`},
	} {
		if _, err := c.Resolve(tc.chosen, tc.needed); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v %v: got %v, want %q", tc.chosen, tc.needed, err, tc.want)
		}
	}
	// Nothing chosen and nothing needed is an empty selection, not an error.
	if s, err := c.Resolve(nil, nil); err != nil || len(s.Tools) != 0 || len(s.Debian) != 0 {
		t.Errorf("%+v %v", s, err)
	}
}
