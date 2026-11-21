// Package tools loads the tool catalogue (catalog/tools/*.json and
// catalog/packs/*.json) and turns a box's choice, such as
// ["pack:coding", "ffmpeg"], into the Debian packages and commands its image
// needs. See docs/build.md.
package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Tool is one program that can be added to the box image.
type Tool struct {
	Schema      int               `json:"schema"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Debian      []string          `json:"debian"`   // Debian package names
	Commands    []string          `json:"commands"` // must exist after the install
	Links       map[string]string `json:"links"`    // extra command name -> the command it runs
	Verified    Verified          `json:"verified"`
}

// Verified records what has actually been tested, as for harnesses.
type Verified struct {
	Status     string `json:"status"` // verified | unverified
	ObservedOn string `json:"observedOn"`
	Notes      string `json:"notes"`
}

// Pack is a named list of tools.
type Pack struct {
	Schema      int      `json:"schema"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tools       []string `json:"tools"`
}

// Catalog is every tool and pack.
type Catalog struct {
	Tools map[string]Tool
	Packs map[string]Pack
}

// PackPrefix marks a pack in a box's tools list: "pack:coding".
const PackPrefix = "pack:"

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	debianPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,62}$`)
	commandPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	datePattern    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Load reads and checks the whole catalogue. Unknown fields are refused.
func Load(fsys fs.FS) (*Catalog, error) {
	c := &Catalog{Tools: map[string]Tool{}, Packs: map[string]Pack{}}
	toolFiles, err := fs.Glob(fsys, "tools/*.json")
	if err != nil {
		return nil, err
	}
	for _, f := range toolFiles {
		var t Tool
		if err := decode(fsys, f, &t); err != nil {
			return nil, err
		}
		if err := t.check(strings.TrimSuffix(path.Base(f), ".json")); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		c.Tools[t.ID] = t
	}
	packFiles, err := fs.Glob(fsys, "packs/*.json")
	if err != nil {
		return nil, err
	}
	for _, f := range packFiles {
		var p Pack
		if err := decode(fsys, f, &p); err != nil {
			return nil, err
		}
		if err := p.check(strings.TrimSuffix(path.Base(f), ".json"), c.Tools); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		c.Packs[p.ID] = p
	}
	return c, nil
}

func decode(fsys fs.FS, name string, v any) error {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (t Tool) check(file string) error {
	switch {
	case t.Schema != 1:
		return fmt.Errorf("schema must be 1")
	case !idPattern.MatchString(t.ID):
		return fmt.Errorf("id %q must be lowercase letters, digits and dashes", t.ID)
	case t.ID != file:
		return fmt.Errorf("id %q must match the file name", t.ID)
	case t.Name == "" || t.Description == "":
		return fmt.Errorf("name and description are required")
	case len(t.Debian) == 0:
		return fmt.Errorf("debian must name at least one package")
	case len(t.Commands) == 0:
		return fmt.Errorf("commands must name at least one command, so the build can check the install")
	}
	for _, p := range t.Debian {
		if !debianPattern.MatchString(p) {
			return fmt.Errorf("%q is not a Debian package name", p)
		}
	}
	for _, c := range t.Commands {
		if !commandPattern.MatchString(c) {
			return fmt.Errorf("%q is not a command name", c)
		}
	}
	for link, target := range t.Links {
		if !commandPattern.MatchString(link) || !commandPattern.MatchString(target) {
			return fmt.Errorf("link %q -> %q: both must be plain command names", link, target)
		}
		if !contains(t.Commands, link) {
			return fmt.Errorf("link %q must also be listed in commands, so the build checks it", link)
		}
	}
	switch t.Verified.Status {
	case "verified":
		if !datePattern.MatchString(t.Verified.ObservedOn) || t.Verified.Notes == "" {
			return fmt.Errorf("a verified tool needs observedOn (YYYY-MM-DD) and notes saying what was checked")
		}
	case "unverified":
	default:
		return fmt.Errorf("verified.status must be verified or unverified")
	}
	return nil
}

func (p Pack) check(file string, all map[string]Tool) error {
	switch {
	case p.Schema != 1:
		return fmt.Errorf("schema must be 1")
	case !idPattern.MatchString(p.ID):
		return fmt.Errorf("id %q must be lowercase letters, digits and dashes", p.ID)
	case p.ID != file:
		return fmt.Errorf("id %q must match the file name", p.ID)
	case p.Name == "" || p.Description == "":
		return fmt.Errorf("name and description are required")
	case len(p.Tools) == 0:
		return fmt.Errorf("a pack needs at least one tool")
	}
	for _, id := range p.Tools {
		if _, ok := all[id]; !ok {
			return fmt.Errorf("unknown tool %q", id)
		}
	}
	return nil
}

// Selection is what a box's image gets: the tools, sorted by id, and what
// they add up to.
type Selection struct {
	Tools    []Tool
	Debian   []string // sorted, no repeats
	Commands []string // sorted, no repeats
	Links    []string // "name=target", sorted
	Chosen   []string // ids from the box's own choice, without the harnesses' tools, sorted
}

// IDs lists the chosen tools' ids.
func (s Selection) IDs() []string {
	out := make([]string, len(s.Tools))
	for i, t := range s.Tools {
		out[i] = t.ID
	}
	return out
}

// Resolve expands a box's tools list (tool ids and "pack:" names) plus the
// tools its harnesses need. Unknown names and unverified tools are refused.
func (c *Catalog) Resolve(chosen, needed []string) (Selection, error) {
	ids := map[string]bool{}
	for _, item := range chosen {
		if name, ok := strings.CutPrefix(item, PackPrefix); ok {
			p, ok := c.Packs[name]
			if !ok {
				return Selection{}, fmt.Errorf("unknown tool pack %q. Packs: %s", name, strings.Join(c.PackIDs(), ", "))
			}
			for _, id := range p.Tools {
				ids[id] = true
			}
			continue
		}
		if _, ok := c.Tools[item]; !ok {
			hint := ""
			if _, isPack := c.Packs[item]; isPack {
				hint = fmt.Sprintf(" (for the pack, write %q)", PackPrefix+item)
			}
			return Selection{}, fmt.Errorf("unknown tool %q%s. Tools: %s", item, hint, strings.Join(c.ToolIDs(), ", "))
		}
		ids[item] = true
	}
	var s Selection
	isNeeded := map[string]bool{}
	for _, id := range needed {
		if _, ok := c.Tools[id]; !ok {
			return Selection{}, fmt.Errorf("a harness needs the tool %q, which isn't in the catalogue", id)
		}
		isNeeded[id] = true
	}
	for _, id := range sortedKeys(ids) {
		if !isNeeded[id] {
			s.Chosen = append(s.Chosen, id)
		}
	}
	for id := range isNeeded {
		ids[id] = true
	}

	debs, cmds, links := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, id := range sortedKeys(ids) {
		t := c.Tools[id]
		if t.Verified.Status != "verified" {
			return Selection{}, fmt.Errorf("the tool %q isn't verified yet, so it can't be built into a box", id)
		}
		s.Tools = append(s.Tools, t)
		for _, p := range t.Debian {
			debs[p] = true
		}
		for _, cmd := range t.Commands {
			cmds[cmd] = true
		}
		for name, target := range t.Links {
			links[name+"="+target] = true
		}
	}
	s.Debian, s.Commands, s.Links = sortedKeys(debs), sortedKeys(cmds), sortedKeys(links)
	return s, nil
}

// ToolIDs lists every tool, sorted.
func (c *Catalog) ToolIDs() []string { return sortedKeys(c.Tools) }

// PackIDs lists every pack, sorted.
func (c *Catalog) PackIDs() []string { return sortedKeys(c.Packs) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
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
