// Package setup is the logic of ezbx init: the questions, their defaults,
// the plan shown before anything changes, and the config file it writes.
// Everything with side effects (Docker, files, detection) stays in the
// command, so the decisions here can be tested on their own.
package setup

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/internal/config"
)

// Answers are the settings init writes to ezbx.toml.
type Answers struct {
	Box       string
	Workspace string // full path
	Memory    string
	CPUs      float64
	PIDs      int
	Network   string
	Allow     []string
	Harnesses []string
	Tools     []string                    // tool ids and "pack:" names; nil when not set
	Engine    string                      // an adapter id, or "auto"
	Overrides map[string]config.EngineURL // per engine: url, key_env, host
	Models    map[string]config.Model
	Log       *bool
}

// FromConfig turns an existing config into answers, so running init again
// starts from what is there and keeps what init doesn't ask about.
func FromConfig(c *config.Config) Answers {
	name, b := c.Only()
	a := Answers{
		Box: name, Workspace: b.Workspace, Memory: b.Memory, CPUs: b.CPUs, PIDs: b.PIDs,
		Network: b.Network, Allow: append([]string(nil), b.Allow...), Harnesses: append([]string(nil), b.Harnesses...),
		Tools:  slices.Clone(b.Tools),
		Engine: c.Engine.Use, Overrides: maps.Clone(c.Engine.Overrides), Models: maps.Clone(c.Models), Log: c.Gateway.Log,
	}
	if a.PIDs == config.DefaultPIDs {
		a.PIDs = 0 // the default, so not written
	}
	return a
}

// MemoryFor is the default memory limit: half of what Docker has, rounded
// down to whole gigabytes, from 2 to 8. On Docker Desktop the box runs in
// Docker's own virtual machine, so its memory is what counts, not the
// computer's. dockerBytes is 0 when it isn't known.
func MemoryFor(dockerBytes int64) string {
	gb := dockerBytes / 2 / (1 << 30)
	if gb < 2 {
		gb = 2
	}
	if gb > 8 {
		gb = 8
	}
	return strconv.FormatInt(gb, 10) + "g"
}

// ConfigText writes the answers as ezbx.toml, with comments that explain
// each setting. Settings left at their defaults are left out.
func ConfigText(a Answers, now time.Time) string {
	var b strings.Builder
	w := func(format string, v ...any) { fmt.Fprintf(&b, format, v...) }
	w("# EZBox config, written by ezbx init on %s.\n", now.Format("2006-01-02"))
	w("# Edit it by hand, or run ezbx init again. Unknown settings are refused.\n")
	w("version = 1\n\n")

	w("[box.%s]\n", a.Box)
	w("workspace = %s   # the ONLY folder the agent can write to\n", quote(a.Workspace))
	w("memory    = %s\n", quote(a.Memory))
	if a.CPUs > 0 {
		w("cpus      = %s\n", strconv.FormatFloat(a.CPUs, 'f', -1, 64))
	}
	if a.PIDs > 0 {
		w("pids      = %d\n", a.PIDs)
	}
	w("network   = %s   # off | allow | on\n", quote(a.Network))
	if len(a.Allow) > 0 {
		w("allow     = %s   # hosts for allow mode, besides the harnesses' own\n", list(a.Allow))
	}
	w("harnesses = %s\n", list(a.Harnesses))
	if a.Tools != nil {
		w("tools     = %s   # tool packs (pack:NAME) and single tools; after a change, ezbx build\n", list(a.Tools))
	}
	w("\n")

	w("[engine]\nuse = %s\n", quote(a.Engine))
	for _, id := range sortedKeys(a.Overrides) {
		o := a.Overrides[id]
		if o == (config.EngineURL{}) {
			continue
		}
		w("\n[engine.%s]\n", id)
		if o.URL != "" {
			w("url     = %s\n", quote(o.URL))
		}
		if o.KeyEnv != "" {
			w("key_env = %s   # the NAME of an environment variable, never the key\n", quote(o.KeyEnv))
		}
		if o.Host != "" {
			w("host    = %s\n", quote(o.Host))
		}
	}
	for _, id := range sortedKeys(a.Models) {
		m := a.Models[id]
		if m == (config.Model{}) {
			continue
		}
		w("\n[models.%s]\n", quote(id))
		if m.Name != "" {
			w("name       = %s\n", quote(m.Name))
		}
		if m.Reasoning != nil {
			w("reasoning  = %t\n", *m.Reasoning)
		}
		if m.MaxTokens > 0 {
			w("max_tokens = %d\n", m.MaxTokens)
		}
		if m.ContextWindow > 0 {
			w("context    = %d\n", m.ContextWindow)
		}
	}
	log := a.Log == nil || *a.Log
	w("\n[gateway]\nlog = %t   # keep a request log in the state folder\n", log)
	return b.String()
}

// quote writes a TOML basic string. Only the escapes TOML knows are used.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func list(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
