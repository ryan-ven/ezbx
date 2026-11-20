// Package harness loads harness manifests (harnesses/<id>/manifest.json) and
// renders their config files from the engine's model list. See
// docs/harness-manifest.md.
package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Manifest describes one harness.
type Manifest struct {
	Schema   int               `json:"schema"`
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Install  Install           `json:"install"`
	Binary   string            `json:"binary"`
	Runtime  string            `json:"runtime"`
	Env      map[string]string `json:"env"`
	Config   []ConfigFile      `json:"config"`
	Launch   Launch            `json:"launch"`
	Autonomy Autonomy          `json:"autonomy"`
	Network  Network           `json:"network"`
	Secrets  []string          `json:"secrets"`
	Verified Verified          `json:"verified"`
}

// Install says how the harness is installed.
type Install struct {
	Type     string `json:"type"`
	Package  string `json:"package"`
	Version  string `json:"version"`
	Script   string `json:"script"`
	Lockfile string `json:"lockfile"`
}

// ConfigFile is one file to generate inside the box. Content is JSON with
// placeholders, rendered by Render.
type ConfigFile struct {
	Path    string          `json:"path"`
	Mode    string          `json:"mode"`
	Content json.RawMessage `json:"content"`
}

// Launch is how `ezbx shell <id>` starts the harness.
type Launch struct {
	Command []string `json:"command"`
	Cwd     string   `json:"cwd"`
}

// Autonomy is the harness's "skip approvals" flag, if it has one.
type Autonomy struct {
	Flag *string `json:"flag"`
	Note string  `json:"note"`
}

// Network lists the domains the harness needs in allow mode.
type Network struct {
	Domains  []string            `json:"domains"`
	Optional map[string][]string `json:"optional"`
}

// Verified records what has actually been tested.
type Verified struct {
	Version    string `json:"version"`
	Status     string `json:"status"`
	ObservedOn string `json:"observedOn"`
	Reverify   bool   `json:"reverify"`
	Notes      string `json:"notes"`
}

var (
	idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// An exact version: 1.2.3, optionally with a pre-release such as 1.2.3-beta.1.
	exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)
	envName      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	secretWords  = regexp.MustCompile(`(?i)(key|token|secret|password|passwd|credential)`)
)

// LoadFS reads every <folder>/manifest.json in fsys, checks each one and
// returns them sorted by id. Unknown fields are errors, so typos are caught.
func LoadFS(fsys fs.FS) ([]Manifest, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name() + "/manifest.json"
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		var m Manifest
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := m.validate(entry.Name()); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Find returns the manifest with the given id.
func Find(list []Manifest, id string) (Manifest, bool) {
	for _, m := range list {
		if m.ID == id {
			return m, true
		}
	}
	return Manifest{}, false
}

func (m Manifest) validate(folder string) error {
	switch {
	case m.Schema != 1:
		return fmt.Errorf("unsupported schema %d", m.Schema)
	case !idPattern.MatchString(m.ID) || m.ID != folder:
		return fmt.Errorf("id %q must be short lowercase and match its folder %q", m.ID, folder)
	case m.Name == "":
		return fmt.Errorf("name is required")
	case m.Kind != "coding" && m.Kind != "assistant":
		return fmt.Errorf("kind must be coding or assistant, not %q", m.Kind)
	case m.Runtime != "node" && m.Runtime != "python" && m.Runtime != "binary":
		return fmt.Errorf("runtime must be node, python or binary, not %q", m.Runtime)
	case m.Binary == "":
		return fmt.Errorf("binary is required")
	case len(m.Launch.Command) == 0:
		return fmt.Errorf("launch.command is required")
	case m.Verified.Status != "verified" && m.Verified.Status != "observed-in-prior-setup" && m.Verified.Status != "unverified":
		return fmt.Errorf("verified.status must be verified, observed-in-prior-setup or unverified")
	}

	// Versions are exact, so a rebuild gives the same box.
	switch m.Install.Type {
	case "npm", "pip", "binary", "script":
	default:
		return fmt.Errorf("install.type must be npm, pip, binary or script, not %q", m.Install.Type)
	}
	if !exactVersion.MatchString(m.Install.Version) {
		return fmt.Errorf("install.version %q must be an exact version such as 1.2.3, not a range or tag", m.Install.Version)
	}
	if m.Install.Type == "npm" && m.Install.Lockfile == "" {
		return fmt.Errorf("an npm harness needs install.lockfile, so everything it pulls in is pinned")
	}
	if m.Install.Script == "" {
		return fmt.Errorf("install.script is required")
	}

	// No secrets in a manifest: the gateway injects keys.
	for k, v := range m.Env {
		if !envName.MatchString(k) {
			return fmt.Errorf("env name %q is not a valid variable name", k)
		}
		if secretWords.MatchString(k) {
			return fmt.Errorf("env %q looks like a secret; secrets never go in a manifest", k)
		}
		if strings.Contains(v, "${") {
			return fmt.Errorf("env %q: placeholders are only allowed in config files", k)
		}
	}

	seen := map[string]bool{}
	for _, c := range m.Config {
		if _, err := homePath(c.Path); err != nil {
			return err
		}
		if seen[c.Path] {
			return fmt.Errorf("config path %q appears twice", c.Path)
		}
		seen[c.Path] = true
		if _, err := parseMode(c.Mode); err != nil {
			return fmt.Errorf("config %q: %w", c.Path, err)
		}
		if len(c.Content) == 0 || !json.Valid(c.Content) {
			return fmt.Errorf("config %q: content must be JSON", c.Path)
		}
	}
	return nil
}

// homePath turns "~/.tool/config.json" into ".tool/config.json", a path
// inside the agent's home. Anything that could leave the home is refused.
func homePath(p string) (string, error) {
	rel, ok := strings.CutPrefix(p, "~/")
	if !ok || rel == "" {
		return "", fmt.Errorf("config path %q must start with ~/", p)
	}
	if strings.ContainsAny(rel, "\\:\x00") {
		return "", fmt.Errorf("config path %q has a character that isn't allowed", p)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("config path %q must not contain empty, . or .. parts", p)
		}
	}
	return rel, nil
}

// parseMode reads an octal file mode such as "0600". Files are never made
// executable or writable by others.
func parseMode(s string) (uint32, error) {
	if len(s) != 4 || s[0] != '0' {
		return 0, fmt.Errorf("mode %q must be four octal digits such as 0600", s)
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q must be four octal digits such as 0600", s)
	}
	if n&0o111 != 0 || n&0o002 != 0 {
		return 0, fmt.Errorf("mode %q must not be executable or writable by others", s)
	}
	return uint32(n), nil
}
