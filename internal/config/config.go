// Package config reads and checks ezbx.toml.
//
// Unknown keys are refused, so a typo such as "netwrok" is an error rather
// than a setting that is quietly ignored.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the whole file.
type Config struct {
	Version int              `toml:"version"`
	Box     map[string]*Box  `toml:"box"`
	Engine  Engine           `toml:"engine"`
	Models  map[string]Model `toml:"models"`
	Gateway Gateway          `toml:"gateway"`
	names   []string         // box names, sorted
}

// Box is one sandbox: a gateway and a box container.
type Box struct {
	Workspace string     `toml:"workspace"` // the only folder the agent can write to
	Memory    string     `toml:"memory"`    // Docker's form, such as 4g or 512m
	CPUs      float64    `toml:"cpus"`      // 0 means no limit
	PIDs      int        `toml:"pids"`      // process limit
	Network   string     `toml:"network"`   // off | allow | on
	Allow     []string   `toml:"allow"`     // extra hosts for allow mode
	Harnesses []string   `toml:"harnesses"`
	ReadOnly  []ReadOnly `toml:"readonly"`
}

// ReadOnly is a folder the agent may read but not change. Not built yet.
type ReadOnly struct {
	Path string   `toml:"path"`
	Mask []string `toml:"mask"`
}

// Engine chooses the inference engine.
type Engine struct {
	Use       string               `toml:"use"` // auto, or an adapter id
	Overrides map[string]EngineURL `toml:"-"`   // filled from the other keys
}

// EngineURL overrides what detection finds for one engine.
type EngineURL struct {
	URL    string `toml:"url"`
	KeyEnv string `toml:"key_env"` // NAME of an environment variable, never the key
	Host   string `toml:"host"`    // Host header to send, like --upstream-host
}

// Model holds facts about one model that engines don't report.
type Model struct {
	Name          string `toml:"name"`
	ContextWindow int    `toml:"context"`
	MaxTokens     int    `toml:"max_tokens"`
	Reasoning     *bool  `toml:"reasoning"`
}

// Gateway settings.
type Gateway struct {
	Log *bool `toml:"log"` // keep a request log; on by default
}

// Defaults used when the file leaves a value out.
const (
	DefaultMemory  = "4g"
	DefaultPIDs    = 512
	DefaultNetwork = "off"
)

// The engine section is read in two passes: "use" into Engine, and every
// table under it into Overrides.
type rawEngine map[string]toml.Primitive

type rawConfig struct {
	Version int              `toml:"version"`
	Box     map[string]*Box  `toml:"box"`
	Engine  rawEngine        `toml:"engine"`
	Models  map[string]Model `toml:"models"`
	Gateway Gateway          `toml:"gateway"`
}

// Load reads and checks a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config file at %s. Copy ezbx.toml.example there and edit it", path)
		}
		return nil, err
	}
	c, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse reads and checks a config from text.
func Parse(text string) (*Config, error) {
	var raw rawConfig
	md, err := toml.Decode(text, &raw)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("line %d: %s", pe.Position.Line, pe.Message)
		}
		return nil, err
	}
	c := &Config{Version: raw.Version, Box: raw.Box, Models: raw.Models, Gateway: raw.Gateway}
	c.Engine.Overrides = map[string]EngineURL{}
	for k, prim := range raw.Engine {
		if k == "use" {
			if err := md.PrimitiveDecode(prim, &c.Engine.Use); err != nil {
				return nil, fmt.Errorf("engine.use: %w", err)
			}
			continue
		}
		var o EngineURL
		if err := md.PrimitiveDecode(prim, &o); err != nil {
			return nil, fmt.Errorf("engine.%s must be a table with url, key_env or host: %w", k, err)
		}
		c.Engine.Overrides[k] = o
	}
	if bad := md.Undecoded(); len(bad) > 0 {
		var keys []string
		for _, k := range bad {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("unknown setting: %s", strings.Join(keys, ", "))
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	return c, nil
}

var (
	boxNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	memoryRE  = regexp.MustCompile(`^[1-9][0-9]*[kmg]$`)
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hostRE    = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// BoxName reports whether s can be used in Docker resource names.
func BoxName(s string) bool { return boxNameRE.MatchString(s) }

func (c *Config) check() error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1 (found %d)", c.Version)
	}
	if len(c.Box) == 0 {
		return errors.New("no box: add a [box.main] section")
	}
	for name, b := range c.Box {
		if !BoxName(name) {
			return fmt.Errorf("box name %q: use up to 32 lowercase letters, digits and dashes", name)
		}
		if b == nil {
			return fmt.Errorf("box.%s is empty", name)
		}
		if err := b.check(name); err != nil {
			return err
		}
		c.names = append(c.names, name)
	}
	sort.Strings(c.names)
	if len(c.names) > 1 {
		return fmt.Errorf("found %d boxes (%s); this version runs one box", len(c.names), strings.Join(c.names, ", "))
	}
	if c.Engine.Use == "" {
		c.Engine.Use = "auto"
	}
	for id, o := range c.Engine.Overrides {
		if o.URL != "" && !strings.HasPrefix(o.URL, "http://") && !strings.HasPrefix(o.URL, "https://") {
			return fmt.Errorf("engine.%s.url must start with http:// or https://", id)
		}
		if o.KeyEnv != "" && !envNameRE.MatchString(o.KeyEnv) {
			return fmt.Errorf("engine.%s.key_env is the NAME of an environment variable, such as STRATA_API_KEY, never the key itself", id)
		}
	}
	for id, m := range c.Models {
		if m.ContextWindow < 0 || m.MaxTokens < 0 {
			return fmt.Errorf("models.%q: sizes can't be negative", id)
		}
	}
	return nil
}

func (b *Box) check(name string) error {
	p := "box." + name + "."
	if strings.TrimSpace(b.Workspace) == "" {
		return errors.New(p + "workspace is required: the one folder the agent can write to")
	}
	if b.Memory == "" {
		b.Memory = DefaultMemory
	}
	b.Memory = strings.ToLower(b.Memory)
	if !memoryRE.MatchString(b.Memory) {
		return fmt.Errorf("%smemory %q: use a whole number and k, m or g, such as 4g", p, b.Memory)
	}
	if b.CPUs < 0 {
		return errors.New(p + "cpus can't be negative")
	}
	if b.PIDs == 0 {
		b.PIDs = DefaultPIDs
	}
	if b.PIDs < 64 {
		return errors.New(p + "pids must be at least 64")
	}
	if b.Network == "" {
		b.Network = DefaultNetwork
	}
	switch b.Network {
	case "off", "allow", "on":
	default:
		return fmt.Errorf("%snetwork %q: use off, allow or on", p, b.Network)
	}
	for _, h := range b.Allow {
		if !hostRE.MatchString(h) {
			return fmt.Errorf("%sallow %q: use a host name such as registry.npmjs.org or *.example.com", p, h)
		}
	}
	if len(b.Harnesses) == 0 {
		return errors.New(p + `harnesses is empty: add at least one, such as ["pi"]`)
	}
	if len(b.ReadOnly) > 0 {
		return errors.New(p + "readonly folders are not supported yet, because hiding secret files in them isn't built. Remove the readonly entries for now")
	}
	return nil
}

// Only returns the one box, with its name.
func (c *Config) Only() (string, *Box) { return c.names[0], c.Box[c.names[0]] }

// LogOn reports whether the gateway keeps a request log.
func (c *Config) LogOn() bool { return c.Gateway.Log == nil || *c.Gateway.Log }

// CPUsFlag formats the CPU limit for docker, or "" for none.
func (b *Box) CPUsFlag() string {
	if b.CPUs == 0 {
		return ""
	}
	return strconv.FormatFloat(b.CPUs, 'f', -1, 64)
}
