package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/config"
)

// runEngineUse points a running box's gateway at an engine, found again
// now, and refreshes the harness configs in the box from its model list.
// The change lasts until the next ezbx up, which uses ezbx.toml.
func runEngineUse(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("engine use", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file, used to find the box")
	boxFlag := fs.String("box", "", "box name; default the one in the config, else main")
	url := fs.String("url", "", "the engine's address on this computer, if detection doesn't find it")
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(words) > 1 || len(rest) > 0 {
		fmt.Fprintln(stderr, "Usage: ezbx engine use [ID] [--url http://127.0.0.1:PORT] [--box NAME]")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	if t.cfg == nil {
		return report(stderr, fmt.Errorf("engine use needs the box's ezbx.toml, for its harnesses and model settings. See: ezbx up"))
	}
	cfg, err := engineChoice(t.cfg, words, *url)
	if err != nil {
		return report(stderr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	eng, override, err := chooseEngine(ctx, cfg)
	if err != nil {
		return report(stderr, err)
	}
	if override.KeyEnv != "" {
		return report(stderr, fmt.Errorf("%s needs a key (key_env %s), which the running gateway can't take on. Set engine.use in ezbx.toml, then ezbx down and ezbx up", eng.Name, override.KeyEnv))
	}
	upstream, err := box.GatewayUpstream(eng.URL)
	if err != nil {
		return report(stderr, err)
	}
	_, b := cfg.Only()
	chosen, err := boxHarnesses(b)
	if err != nil {
		return report(stderr, err)
	}
	files, err := renderHome(cfg, chosen, eng)
	if err != nil {
		return report(stderr, err)
	}

	r := &box.Runner{Docker: dockerIn, Out: stdout}
	if _, err := r.Admin(ctx, t.name, "engine", upstream, eng.ID); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "The gateway now forwards to %s at %s (%d model(s)).\n", eng.Name, eng.URL, len(eng.Models))
	p := box.Plan{Box: t.name, Names: box.NamesFor(t.name), HomeFiles: files}
	if err := r.CopyHome(ctx, p); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "Updated the config of %s in the box. A harness that is already running keeps its old model list until you start it again.\n", strings.Join(b.Harnesses, ", "))
	if cfg.Engine.Use != t.cfg.Engine.Use || *url != "" {
		fmt.Fprintln(stdout, "This lasts until the next ezbx up, which uses the engine in ezbx.toml.")
	}
	return 0
}

// engineChoice applies the command's ID and --url to a copy of the config,
// leaving the loaded config unchanged.
func engineChoice(c *config.Config, words []string, url string) (*config.Config, error) {
	cp := *c
	cp.Engine.Overrides = maps.Clone(c.Engine.Overrides)
	if cp.Engine.Overrides == nil {
		cp.Engine.Overrides = map[string]config.EngineURL{}
	}
	if len(words) == 1 {
		cp.Engine.Use = words[0]
	}
	if url != "" {
		if cp.Engine.Use == "auto" {
			return nil, usageError("--url needs the engine's id too, such as: ezbx engine use strata --url http://127.0.0.1:PORT")
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, usageError("--url must start with http:// or https://")
		}
		o := cp.Engine.Overrides[cp.Engine.Use]
		o.URL = url
		cp.Engine.Overrides[cp.Engine.Use] = o
	}
	return &cp, nil
}
