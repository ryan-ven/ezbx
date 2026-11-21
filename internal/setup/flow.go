package setup

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/detect"
	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/internal/paths"
	"github.com/ryan-ven/ezbx/internal/tools"
)

// Facts are what the command found before asking anything.
type Facts struct {
	Engines      []detect.Engine    // running now
	Harnesses    []harness.Manifest // verified ones, offered
	Catalog      *tools.Catalog     // the tool catalogue
	DockerMemory int64              // bytes; 0 if unknown
	Home         string
	Own          []string // EZBox's config and state folders
	System       []string // folders that may never be the workspace
	Existing     *config.Config
}

// Options are the answers given as flags. Empty means "ask".
type Options struct {
	Engine    string // an id, "auto", or "later"
	URL       string // where the engine is, if detection misses it
	KeyEnv    string
	Reasoning bool // mark every model of the chosen engine as reasoning
	Harnesses []string
	Tools     []string // nil: ask; empty: none
	Workspace string
	Network   string
	Memory    string
}

// Result is what Gather decided.
type Result struct {
	Answers Answers
	Engine  *detect.Engine // nil when no engine was chosen ("later")
}

// Gather asks every question in order, using flags first, then the
// existing config, then what was detected, as defaults.
func Gather(f Facts, o Options, p *Prompter) (Result, error) {
	a := Answers{Box: "main", Network: "off", Engine: "auto", Overrides: map[string]config.EngineURL{}, Models: map[string]config.Model{}}
	if f.Existing != nil {
		a = FromConfig(f.Existing)
		if a.Overrides == nil {
			a.Overrides = map[string]config.EngineURL{}
		}
		if a.Models == nil {
			a.Models = map[string]config.Model{}
		}
	}
	var res Result

	// 1. Engine.
	eng, err := chooseEngine(f, o, &a, p)
	if err != nil {
		return res, err
	}
	res.Engine = eng
	if eng != nil {
		if err := askReasoning(*eng, o, &a, p); err != nil {
			return res, err
		}
	}

	// 2. Harnesses, then the tools built in beside them.
	if err := askHarnesses(f, o, &a, p); err != nil {
		return res, err
	}
	if err := askTools(f, o, &a, p); err != nil {
		return res, err
	}

	// 3. Workspace.
	def := o.Workspace
	if def == "" {
		def = a.Workspace
	}
	if def == "" {
		def = filepath.Join(f.Home, "ezbx", "workspace")
	}
	fmt.Fprintln(p.Out, "\nThe workspace is the only folder the agent can change. It is created if missing.")
	ws, err := p.Ask("Workspace folder", def, func(s string) error {
		_, err := cleanWorkspace(s, f)
		return err
	})
	if err != nil {
		return res, err
	}
	a.Workspace, _ = cleanWorkspace(ws, f)

	// 4. Network.
	if err := askNetwork(f, o, &a, p); err != nil {
		return res, err
	}

	// 5. Limits.
	def = o.Memory
	if def == "" {
		def = a.Memory
	}
	if def == "" {
		def = MemoryFor(f.DockerMemory)
	}
	fmt.Fprintf(p.Out, "\nThe memory limit keeps the box from taking over this computer. The default is half of Docker's memory%s, from 2g to 8g.\n", gbText(f.DockerMemory))
	mem, err := p.Ask("Memory limit", def, func(s string) error {
		if !config.MemoryValue(s) {
			return errors.New("Use a whole number and k, m or g, such as 4g.")
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	a.Memory = strings.ToLower(mem)
	res.Answers = a
	return res, nil
}

func gbText(b int64) string {
	if b <= 0 {
		return ""
	}
	return fmt.Sprintf(" (Docker has %.1f GB)", float64(b)/(1<<30))
}

// cleanWorkspace expands ~, makes the path full and checks it.
func cleanWorkspace(s string, f Facts) (string, error) {
	ws := paths.ExpandHome(strings.TrimSpace(s), f.Home)
	if !filepath.IsAbs(ws) {
		return "", fmt.Errorf("Give a full path, or one starting with ~.")
	}
	ws = filepath.Clean(ws)
	if err := box.CheckWorkspace(ws, f.Home, f.Own, f.System); err != nil {
		return "", err
	}
	return ws, nil
}

func chooseEngine(f Facts, o Options, a *Answers, p *Prompter) (*detect.Engine, error) {
	fmt.Fprintln(p.Out, "Inference engines found on this computer")
	if len(f.Engines) == 0 {
		fmt.Fprintln(p.Out, "  none. Start your engine and load a model, then run ezbx init again,")
		fmt.Fprintln(p.Out, "  or carry on: the config is written, and ezbx up works once the engine runs.")
	}
	for i, e := range f.Engines {
		fmt.Fprintf(p.Out, "  %d. %-26s %s, %d model(s)\n", i+1, e.Name, e.URL, len(e.Models))
	}
	if len(f.Engines) > 0 {
		fmt.Fprintln(p.Out, "  L. I'll start one later")
	}

	// The default: a flag, then the configured engine if it runs, then the
	// first one found.
	def := ""
	pick := func(id string) {
		for i, e := range f.Engines {
			if e.ID == id && def == "" {
				def = strconv.Itoa(i + 1)
			}
		}
	}
	switch o.Engine {
	case "":
		if a.Engine != "auto" {
			pick(a.Engine)
		}
		if def == "" && len(f.Engines) > 0 {
			def = "1"
		}
	case "later":
		def = "L"
	case "auto":
		if len(f.Engines) > 0 {
			def = "1"
		}
	default:
		pick(o.Engine)
		if def == "" {
			// Written anyway, so init can come before the engine starts;
			// the plan then says the box won't be started yet.
			fmt.Fprintf(p.Out, "  (%s is not running now.)\n", o.Engine)
			a.Engine = o.Engine
			return nil, nil
		}
	}
	if def == "" {
		def = "L"
	}
	if len(f.Engines) == 0 {
		a.Engine = keepOr(o.Engine, a.Engine)
		return nil, nil
	}
	ans, err := p.Ask("Use which?", def, func(s string) error {
		if strings.EqualFold(s, "l") {
			return nil
		}
		if n, err := strconv.Atoi(s); err != nil || n < 1 || n > len(f.Engines) {
			return fmt.Errorf("Type a number from 1 to %d, or L.", len(f.Engines))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(ans, "l") {
		a.Engine = keepOr(o.Engine, a.Engine)
		return nil, nil
	}
	n, _ := strconv.Atoi(ans)
	e := f.Engines[n-1]
	a.Engine = e.ID
	if o.Engine == "auto" {
		a.Engine = "auto" // detect again at every ezbx up
	}
	ov := a.Overrides[e.ID]
	if o.URL != "" {
		ov.URL = o.URL
	}
	if o.KeyEnv != "" {
		ov.KeyEnv = o.KeyEnv
	}
	if ov != (config.EngineURL{}) {
		a.Overrides[e.ID] = ov
	}
	return &e, nil
}

// keepOr is the engine to write when none was picked now: a named one from
// the flag, else what the config had, else auto.
func keepOr(flag, had string) string {
	if flag != "" && flag != "later" {
		return flag
	}
	if had != "" {
		return had
	}
	return "auto"
}

func askReasoning(e detect.Engine, o Options, a *Answers, p *Prompter) error {
	if len(e.Models) == 0 {
		fmt.Fprintf(p.Out, "\n%s lists no models yet. Load one before ezbx up.\n", e.Name)
		return nil
	}
	fmt.Fprintln(p.Out, "\nEngines don't say whether a model is a reasoning model (one that thinks before it")
	fmt.Fprintln(p.Out, "answers). A reasoning model needs this set, or its answers can be cut off.")
	for _, m := range e.Models {
		cur := a.Models[m.ID]
		def := o.Reasoning || (cur.Reasoning != nil && *cur.Reasoning)
		yes, err := p.YesNo("Is "+m.ID+" a reasoning model?", def)
		if err != nil {
			return err
		}
		if yes || cur.Reasoning != nil {
			v := yes
			cur.Reasoning = &v
		}
		if cur == (config.Model{}) {
			delete(a.Models, m.ID)
		} else {
			a.Models[m.ID] = cur
		}
	}
	return nil
}

func askHarnesses(f Facts, o Options, a *Answers, p *Prompter) error {
	if len(f.Harnesses) == 0 {
		return errors.New("internal error: no verified harness to offer")
	}
	fmt.Fprintln(p.Out, "\nHarnesses (the agent tools installed in the box)")
	index := map[string]int{}
	for i, m := range f.Harnesses {
		index[m.ID] = i + 1
		fmt.Fprintf(p.Out, "  %d. %-12s %s, %s\n", i+1, m.Name, m.Install.Version, m.Verified.Status)
	}
	want := o.Harnesses
	if len(want) == 0 {
		want = a.Harnesses
	}
	var nums []string
	for _, id := range want {
		if n, ok := index[id]; ok {
			nums = append(nums, strconv.Itoa(n))
		} else if len(o.Harnesses) > 0 {
			return fmt.Errorf("harness %q is not available (verified ones: %s)", id, harnessIDs(f.Harnesses))
		}
	}
	if len(nums) == 0 {
		nums = []string{strconv.Itoa(index["pi"])}
		if index["pi"] == 0 {
			nums = []string{"1"}
		}
	}
	var chosen []string
	_, err := p.Ask("Which? (numbers, separated by commas)", strings.Join(nums, ","), func(s string) error {
		chosen = nil
		seen := map[int]bool{}
		for _, part := range strings.Split(s, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(f.Harnesses) {
				return fmt.Errorf("Type numbers from 1 to %d, such as 1.", len(f.Harnesses))
			}
			if !seen[n] {
				seen[n] = true
				chosen = append(chosen, f.Harnesses[n-1].ID)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.Harnesses = chosen
	return nil
}

// DefaultTools is offered when the config doesn't choose tools yet.
var DefaultTools = []string{tools.PackPrefix + "coding"}

// askTools asks which tool packs and single tools go into the box image.
// "none" means only the tools the harnesses need themselves.
func askTools(f Facts, o Options, a *Answers, p *Prompter) error {
	if f.Catalog == nil {
		return errors.New("internal error: no tool catalogue")
	}
	fmt.Fprintln(p.Out, "\nTools built into the box (the agent can't install programs itself)")
	for _, id := range f.Catalog.PackIDs() {
		pk := f.Catalog.Packs[id]
		fmt.Fprintf(p.Out, "  %-14s %s\n  %-14s (%s)\n", tools.PackPrefix+id, pk.Description, "", strings.Join(pk.Tools, ", "))
	}
	fmt.Fprintf(p.Out, "  Single tools: %s\n", strings.Join(f.Catalog.ToolIDs(), ", "))
	var needed []string
	for _, m := range f.Harnesses {
		needed = append(needed, m.Tools...)
	}
	if len(needed) > 0 {
		fmt.Fprintf(p.Out, "  Always included for the harnesses: %s\n", strings.Join(needed, ", "))
	}

	// A new config starts with the coding pack. An existing one keeps what it
	// has, so taking every default changes nothing (a config from before
	// tools existed has none, and keeps none until a tool is chosen).
	def := DefaultTools
	switch {
	case o.Tools != nil:
		def = o.Tools
	case f.Existing != nil:
		def = a.Tools
	}
	var chosen []string
	_, err := p.Ask("Which? (packs and tools, separated by commas, or none)", toolsText(def), func(s string) error {
		chosen = []string{}
		if strings.EqualFold(strings.TrimSpace(s), "none") {
			return nil
		}
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" && !slices.Contains(chosen, part) {
				chosen = append(chosen, part)
			}
		}
		_, err := f.Catalog.Resolve(chosen, nil)
		return err
	})
	if err != nil {
		return err
	}
	if len(chosen) == 0 && a.Tools == nil && o.Tools == nil && f.Existing != nil {
		return nil // still not set, as before
	}
	a.Tools = chosen
	return nil
}

func toolsText(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ",")
}

func harnessIDs(list []harness.Manifest) string {
	var ids []string
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ", ")
}

func askNetwork(f Facts, o Options, a *Answers, p *Prompter) error {
	fmt.Fprintln(p.Out, "\nNetwork for the agent (the model always works):")
	fmt.Fprintln(p.Out, "  off    no internet")
	fmt.Fprintln(p.Out, "  allow  only the hosts you list, such as registry.npmjs.org")
	fmt.Fprintln(p.Out, "  on     the public internet; local addresses stay blocked")
	fmt.Fprintln(p.Out, "  Change it any time with: ezbx net off|allow|on")
	def := o.Network
	if def == "" {
		def = a.Network
	}
	mode, err := p.Ask("Network", def, func(s string) error {
		switch s {
		case "off", "allow", "on":
			return nil
		}
		return errors.New("Type off, allow or on.")
	})
	if err != nil {
		return err
	}
	a.Network = mode
	if mode != "allow" {
		return nil
	}
	var theirs []string
	for _, m := range f.Harnesses {
		for _, id := range a.Harnesses {
			if m.ID == id {
				theirs = append(theirs, m.Network.Domains...)
			}
		}
	}
	if len(theirs) > 0 {
		sort.Strings(theirs)
		fmt.Fprintln(p.Out, "  Allowed for the chosen harnesses anyway:", strings.Join(theirs, ", "))
	}
	ans, err := p.Ask("More hosts to allow (separated by commas, blank for none)", strings.Join(a.Allow, ","), func(s string) error {
		for _, h := range splitList(s) {
			if !config.HostName(h) {
				return fmt.Errorf("%q is not a host name such as registry.npmjs.org or *.example.com.", h)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.Allow = splitList(ans)
	return nil
}

func splitList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		h := strings.ToLower(strings.TrimSpace(part))
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// Changes lists what differs between the existing config and the answers,
// for the review screen.
func Changes(old, new Answers) []string {
	var out []string
	add := func(name, a, b string) {
		if a != b {
			if a == "" {
				a = "(not set)"
			}
			if b == "" {
				b = "(not set)"
			}
			out = append(out, fmt.Sprintf("%-10s %s -> %s", name, a, b))
		}
	}
	add("box", old.Box, new.Box)
	add("engine", old.Engine, new.Engine)
	add("harnesses", strings.Join(old.Harnesses, ","), strings.Join(new.Harnesses, ","))
	add("tools", toolsSetting(old.Tools), toolsSetting(new.Tools))
	add("workspace", old.Workspace, new.Workspace)
	add("network", old.Network, new.Network)
	add("allow", strings.Join(old.Allow, ","), strings.Join(new.Allow, ","))
	add("memory", old.Memory, new.Memory)
	for _, id := range sortedKeys(mergedKeys(old.Overrides, new.Overrides)) {
		add("engine."+id, fmt.Sprint(old.Overrides[id]), fmt.Sprint(new.Overrides[id]))
	}
	for _, id := range sortedKeys(mergedKeys(old.Models, new.Models)) {
		add(id, modelText(old.Models[id]), modelText(new.Models[id]))
	}
	return out
}

// toolsSetting tells "not set" (no tools key) from an empty list.
func toolsSetting(list []string) string {
	if list == nil {
		return ""
	}
	return toolsText(list)
}

func mergedKeys[V any](a, b map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range maps.Keys(a) {
		out[k] = true
	}
	for k := range maps.Keys(b) {
		out[k] = true
	}
	return out
}

func modelText(m config.Model) string {
	var parts []string
	if m.Reasoning != nil {
		parts = append(parts, "reasoning "+strconv.FormatBool(*m.Reasoning))
	}
	if m.MaxTokens > 0 {
		parts = append(parts, "max_tokens "+strconv.Itoa(m.MaxTokens))
	}
	if m.ContextWindow > 0 {
		parts = append(parts, "context "+strconv.Itoa(m.ContextWindow))
	}
	if m.Name != "" {
		parts = append(parts, "name "+m.Name)
	}
	return strings.Join(parts, ", ")
}
