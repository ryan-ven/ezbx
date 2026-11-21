package setup

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/harness"
)

// Plan is what the review screen shows and what apply does.
type Plan struct {
	Result     Result
	Old        *Answers // the existing config, if any
	ConfigFile string
	StateDir   string
	Images     map[string]bool // image name -> already on this computer
	Harnesses  []harness.Manifest
	NoStart    bool // --no-start
	BoxUp      bool // the box's containers exist already
	KeyEnvSet  bool // the engine's key variable has a value
	Workspace  bool // the workspace folder exists already
}

// Changed reports whether the config file would be written.
func (p Plan) Changed() bool { return p.Old == nil || len(Changes(*p.Old, p.Result.Answers)) > 0 }

// MissingImages lists the images that aren't on this computer.
func (p Plan) MissingImages() []string {
	var out []string
	for _, img := range []string{box.GatewayImage, box.BoxImage} {
		if !p.Images[img] {
			out = append(out, img)
		}
	}
	return out
}

// Start reports whether apply will run ezbx up, and if not, why.
func (p Plan) Start() (bool, string) {
	switch {
	case p.NoStart:
		return false, "--no-start was given"
	case p.Result.Engine == nil:
		return false, "no engine was chosen; start it, then run ezbx up"
	case len(p.Result.Engine.Models) == 0:
		return false, "the engine lists no models; load one, then run ezbx up"
	case len(p.MissingImages()) > 0:
		return false, "images are missing (see above); build them, then run ezbx up"
	case p.BoxUp && p.Changed():
		return false, "the box is up; to apply the changes, run ezbx down, then ezbx up"
	case p.BoxUp:
		return false, "the box is already up"
	case p.keyEnv() != "" && !p.KeyEnvSet:
		return false, "the key variable " + p.keyEnv() + " is empty; set it, then run ezbx up"
	}
	return true, ""
}

func (p Plan) keyEnv() string {
	if p.Result.Engine == nil {
		return ""
	}
	return p.Result.Answers.Overrides[p.Result.Engine.ID].KeyEnv
}

// Review is the screen shown before anything changes.
func (p Plan) Review() string {
	a := p.Result.Answers
	var b strings.Builder
	row := func(k, v string) { fmt.Fprintf(&b, "%-13s %s\n", k, v) }
	b.WriteString("\nEZBox plan (nothing has been changed yet)\n\n")

	if e := p.Result.Engine; e != nil {
		use := e.Name + " at " + e.URL
		if a.Engine == "auto" {
			use += ", found again at every ezbx up"
		}
		row("Engine", fmt.Sprintf("%s, %d model(s)", use, len(e.Models)))
		var models []string
		for _, m := range e.Models {
			t := m.ID
			if r := a.Models[m.ID].Reasoning; r != nil && *r {
				t += " (reasoning)"
			}
			models = append(models, t)
		}
		if len(models) > 0 {
			row("Models", strings.Join(models, ", "))
		}
		if k := p.keyEnv(); k != "" {
			state := "set"
			if !p.KeyEnvSet {
				state = "EMPTY"
			}
			row("Engine key", "from the environment variable "+k+" ("+state+")")
		}
	} else {
		row("Engine", a.Engine+" (not running now)")
	}
	var hs []string
	for _, id := range a.Harnesses {
		for _, m := range p.Harnesses {
			if m.ID == id {
				hs = append(hs, fmt.Sprintf("%s %s (%s)", m.Name, m.Install.Version, m.Verified.Status))
			}
		}
	}
	row("Harnesses", strings.Join(hs, ", "))
	ws := a.Workspace
	if !p.Workspace {
		ws += "   (will be created)"
	}
	row("Workspace", ws)
	row("Read-only", "none (not supported yet)")
	net := a.Network
	if a.Network == "allow" {
		allow := append([]string(nil), a.Allow...)
		sort.Strings(allow)
		net += ": " + strings.Join(append(allow, "(and the harnesses' own hosts)"), ", ")
	}
	row("Network", net)
	row("Memory", a.Memory)

	b.WriteString("\nWill create or change\n")
	switch {
	case p.Old == nil:
		fmt.Fprintf(&b, "  config file   %s (new)\n", p.ConfigFile)
	case p.Changed():
		fmt.Fprintf(&b, "  config file   %s (the current one is kept as %s.previous)\n", p.ConfigFile, p.ConfigFile)
		for _, c := range Changes(*p.Old, a) {
			fmt.Fprintf(&b, "                %s\n", c)
		}
	default:
		fmt.Fprintf(&b, "  config file   %s (no changes)\n", p.ConfigFile)
	}
	fmt.Fprintf(&b, "  state folder  %s (admin token, logs, approved hosts)\n", p.StateDir)
	if miss := p.MissingImages(); len(miss) > 0 {
		fmt.Fprintf(&b, "  images        MISSING: %s. Build them from a clone of the repository:\n", strings.Join(miss, ", "))
		for _, img := range miss {
			name := strings.TrimSuffix(strings.TrimPrefix(img, "ezbx-"), ":dev")
			fmt.Fprintf(&b, "                  docker build -f docker/%s.Dockerfile -t %s .\n", name, img)
		}
	} else {
		fmt.Fprintf(&b, "  images        %s and %s are on this computer\n", box.GatewayImage, box.BoxImage)
	}
	if ok, why := p.Start(); ok {
		b.WriteString("  then          ezbx up (2 networks, 1 volume, 2 containers), then ezbx doctor\n")
	} else {
		fmt.Fprintf(&b, "  then          nothing is started: %s\n", why)
	}
	return b.String()
}
