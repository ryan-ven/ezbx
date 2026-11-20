// Package adapter loads and checks the engine adapter files
// (providers/<id>/adapter.json). See docs/engine-adapters.md.
package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// Adapter describes one inference engine.
type Adapter struct {
	Schema       int            `json:"schema"`
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Detect       Detect         `json:"detect"`
	OpenAI       OpenAI         `json:"openai"`
	Models       Models         `json:"models"`
	Auth         Auth           `json:"auth"`
	Capabilities []string       `json:"capabilities"`
	Control      map[string]any `json:"control"`
	Verified     Verified       `json:"verified"`
}

// Detect says how to find the engine.
type Detect struct {
	HintPorts []int   `json:"hintPorts"`
	CLI       *CLI    `json:"cli"`
	Probes    []Probe `json:"probes"`
}

// CLI is a command that lists running servers, and the path to their URLs.
type CLI struct {
	Command []string `json:"command"`
	Reads   string   `json:"reads"`
}

// Probe is one request used to recognise the engine.
type Probe struct {
	Path     string `json:"path"`
	Expect   Expect `json:"expect"`
	Optional bool   `json:"optional"`
}

// Expect lists the JSON fields a reply must contain.
type Expect struct {
	HasFields []string `json:"hasFields"`
}

// OpenAI holds the paths of the OpenAI-style API.
type OpenAI struct {
	BasePath   string `json:"basePath"`
	ModelsPath string `json:"modelsPath"`
	ChatPath   string `json:"chatPath"`
}

// Models says where to read model facts from the model list.
// A nil entry means the engine doesn't report that fact.
type Models struct {
	ID            string  `json:"id"`
	ContextWindow *string `json:"contextWindow"`
	Input         *string `json:"input"`
	Loaded        *string `json:"loaded"`
}

// Auth describes the engine's key handling. It never holds a key.
type Auth struct {
	Mode   string  `json:"mode"`
	KeyEnv *string `json:"keyEnv"`
	Note   string  `json:"note"`
}

// Verified records what has actually been tested.
type Verified struct {
	Status string  `json:"status"`
	On     *string `json:"on"`
	Notes  string  `json:"notes"`
}

// LoadFS reads every <folder>/adapter.json in fsys, checks each one and
// returns them sorted by id. Unknown fields are errors, so typos are caught.
func LoadFS(fsys fs.FS) ([]Adapter, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}

	var out []Adapter
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name() + "/adapter.json"
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		var a Adapter
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := a.validate(entry.Name()); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, a)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (a Adapter) validate(folder string) error {
	switch {
	case a.Schema != 1:
		return fmt.Errorf("unsupported schema %d", a.Schema)
	case a.ID == "" || a.ID != folder:
		return fmt.Errorf("id %q must match its folder %q", a.ID, folder)
	case a.Name == "":
		return fmt.Errorf("name is required")
	case !strings.HasPrefix(a.OpenAI.ModelsPath, "/"):
		return fmt.Errorf("openai.modelsPath must start with /")
	case a.Models.ID == "":
		return fmt.Errorf("models.id is required")
	}

	required := 0
	for _, p := range a.Detect.Probes {
		if !strings.HasPrefix(p.Path, "/") {
			return fmt.Errorf("probe path %q must start with /", p.Path)
		}
		if !p.Optional {
			required++
		}
	}
	if required == 0 {
		return fmt.Errorf("at least one required probe is needed")
	}
	if a.Detect.CLI != nil && (len(a.Detect.CLI.Command) == 0 || a.Detect.CLI.Reads == "") {
		return fmt.Errorf("detect.cli needs a command and a reads path")
	}
	return nil
}
