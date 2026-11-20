package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ryan-ven/ezbx/harnesses"
	"github.com/ryan-ven/ezbx/internal/adapter"
	"github.com/ryan-ven/ezbx/internal/detect"
	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/providers"
)

// gatewayKey is the placeholder key harnesses send to the gateway. It is not
// a credential: the gateway drops it and adds the engine's real key, if any.
const gatewayKey = "ezbx-placeholder"

// runHarnessRender writes a harness's config files, filled in from the
// engine's model list. The files go under --out, laid out as they will be in
// the agent's home folder.
func runHarnessRender(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("harness render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "folder to write into, laid out like the agent's home (required)")
	engineURL := fs.String("url", "", "engine address; empty means use the one engine that is detected")
	gatewayURL := fs.String("gateway-url", "http://gateway:8000/v1", "the gateway's address as seen from inside the box")
	def := fs.String("default", "", "default model id; empty means the first one the engine lists")
	ctxWin := fs.Int("context", 0, "context size for models whose engine doesn't report one (default 32768)")
	maxTok := fs.Int("max-tokens", 0, "longest reply to allow (default 8192)")
	reasoning := fs.Bool("reasoning", false, "mark the models as reasoning models")
	// Go's flag parsing stops at the first word that isn't a flag, so take
	// the harness id off the front when it comes first.
	id := ""
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	} else if fs.NArg() != 0 {
		id = ""
	}
	if id == "" || *out == "" {
		fmt.Fprintln(stderr, "Usage: ezbx harness render ID --out FOLDER [--url ADDRESS] [--default MODEL]")
		return 2
	}

	list, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		fmt.Fprintln(stderr, "internal error loading harnesses:", err)
		return 1
	}
	m, ok := harness.Find(list, id)
	if !ok {
		fmt.Fprintf(stderr, "Unknown harness %q.\n", id)
		return 2
	}

	eng, err := findEngine(*engineURL)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	models := harness.Models(eng.Models, harness.Defaults{ContextWindow: *ctxWin, MaxTokens: *maxTok, Reasoning: *reasoning}, nil)
	files, err := harness.Render(m, harness.Values{GatewayBaseURL: *gatewayURL, GatewayKey: gatewayKey, Models: models, Default: *def})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	for _, f := range files {
		// Render only returns paths inside the home, so this stays under out.
		dst := filepath.Join(*out, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := os.WriteFile(dst, f.Data, os.FileMode(f.Mode)); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "wrote ~/%s\n", f.Path)
	}
	fmt.Fprintf(stdout, "%s is set up for %d model(s) from %s (%s).\n", m.Name, len(models), eng.Name, eng.URL)
	return 0
}

// findEngine detects the engine at url, or the one engine running if url is
// empty.
func findEngine(url string) (detect.Engine, error) {
	if url == "" {
		return onlyEngine()
	}
	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		return detect.Engine{}, fmt.Errorf("internal error loading adapters: %w", err)
	}
	d := &detect.Detector{Adapters: adapters, SkipHintPorts: true}
	res := d.Detect(context.Background(), []string{url})
	if len(res.Engines) == 0 {
		return detect.Engine{}, errors.New("no engine answered at " + url)
	}
	return res.Engines[0], nil
}
