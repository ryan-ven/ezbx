package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/catalog"
	"github.com/ryan-ven/ezbx/harnesses"
	"github.com/ryan-ven/ezbx/internal/adapter"
	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/detect"
	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/internal/images"
	"github.com/ryan-ven/ezbx/internal/paths"
	"github.com/ryan-ven/ezbx/internal/setup"
	"github.com/ryan-ven/ezbx/internal/tools"
	"github.com/ryan-ven/ezbx/providers"
)

// stdin is where init reads answers. Tests replace it.
var stdin io.Reader = os.Stdin

// runInit is the guided setup: it asks the questions, shows the plan, and
// only then writes ezbx.toml and starts the box. See docs/init-flow.md.
func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file to write (and to start from, if it exists)")
	plan := fs.Bool("plan", false, "show what would be done, without asking or changing anything")
	yes := fs.Bool("yes", false, "take every default without asking")
	noStart := fs.Bool("no-start", false, "write the config, but don't start the box")
	var o setup.Options
	fs.StringVar(&o.Engine, "engine", "", "engine id, auto, or later")
	fs.StringVar(&o.URL, "url", "", "the engine's address, if detection doesn't find it")
	fs.StringVar(&o.KeyEnv, "key-env", "", "NAME of an environment variable holding the engine's key")
	fs.BoolVar(&o.Reasoning, "reasoning", false, "mark the engine's models as reasoning models")
	harnessList := fs.String("harness", "", "harnesses, separated by commas, such as pi")
	toolList := fs.String("tools", "", "tool packs and tools, separated by commas, such as pack:coding,ffmpeg, or none")
	fs.StringVar(&o.Workspace, "workspace", "", "the workspace folder")
	fs.StringVar(&o.Network, "network", "", "off, allow or on")
	fs.StringVar(&o.Memory, "memory", "", "memory limit, such as 8g")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx init [--plan] [--yes] [--no-start] [--config FILE] [--engine ID] [--harness pi] [--tools pack:coding] [--workspace PATH] [--network off|allow|on] [--memory 8g]")
		return 2
	}
	if *harnessList != "" {
		for _, h := range strings.Split(*harnessList, ",") {
			o.Harnesses = append(o.Harnesses, strings.TrimSpace(h))
		}
	}
	if *toolList != "" {
		o.Tools = []string{}
		if !strings.EqualFold(*toolList, "none") {
			for _, t := range strings.Split(*toolList, ",") {
				if t = strings.TrimSpace(t); t != "" {
					o.Tools = append(o.Tools, t)
				}
			}
		}
	}
	if o.URL != "" && o.Engine == "" {
		return report(stderr, usageError("--url needs --engine ID too, such as: --engine strata --url http://127.0.0.1:PORT"))
	}
	if o.KeyEnv != "" && !envName(o.KeyEnv) {
		return report(stderr, usageError("--key-env is the NAME of an environment variable, such as STRATA_API_KEY, never the key itself"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	p := &setup.Prompter{In: bufio.NewReader(stdin), Out: stdout, Auto: *yes || *plan}

	// 0. Preflight.
	fmt.Fprintln(stdout, "EZBox setup")
	if err := preflight(ctx, stdout); err != nil {
		return report(stderr, err)
	}
	dirs, err := paths.Default()
	if err != nil {
		return report(stderr, err)
	}
	cfgFile := dirs.ConfigFile(*cfgFlag, os.Getenv)
	var existing *config.Config
	if _, err := os.Stat(cfgFile); err == nil {
		if existing, err = config.Load(cfgFile); err != nil {
			return report(stderr, fmt.Errorf("%w\nThe existing config can't be read, so init won't replace it. Fix it, or move it away, then run ezbx init again", err))
		}
		fmt.Fprintln(stdout, "Starting from the existing config:", cfgFile)
	}

	facts, err := gatherFacts(ctx, existing, o, dirs)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintln(stdout)
	res, err := setup.Gather(facts, o, p)
	if err != nil {
		return report(stderr, err)
	}

	text := setup.ConfigText(res.Answers, time.Now())
	if _, err := config.Parse(text); err != nil {
		return report(stderr, fmt.Errorf("internal error: the config init would write doesn't load: %w", err))
	}
	gw, bx, err := imageSpecs(res.Answers.Tools)
	if err != nil {
		return report(stderr, err)
	}
	pl := setup.Plan{
		Result: res, ConfigFile: cfgFile, StateDir: dirs.Box(res.Answers.Box),
		Images: imagesPresent(ctx, gw, bx), Tools: bx.Tools.IDs(), Harnesses: facts.Harnesses, NoStart: *noStart,
	}
	if existing != nil {
		old := setup.FromConfig(existing)
		pl.Old = &old
	}
	pl.BoxUp = boxExists(ctx, res.Answers.Box)
	if res.Engine != nil {
		if k := res.Answers.Overrides[res.Engine.ID].KeyEnv; k != "" {
			pl.KeyEnvSet = os.Getenv(k) != ""
		}
	}
	if st, err := os.Stat(res.Answers.Workspace); err == nil && st.IsDir() {
		pl.Workspace = true
	}
	fmt.Fprint(stdout, pl.Review())

	if *plan {
		fmt.Fprintln(stdout, "\nThis was a plan only (--plan): nothing was changed.")
		return 0
	}
	start, why := pl.Start()
	if !pl.Changed() && !start {
		fmt.Fprintf(stdout, "\nNothing to change (%s).\n", why)
		return 0
	}
	if !*yes {
		fmt.Fprintln(stdout)
		ok, err := p.YesNo("Continue?", false)
		if err != nil {
			return report(stderr, err)
		}
		if !ok {
			fmt.Fprintln(stdout, "Nothing was changed.")
			return 0
		}
	}

	// 7. Apply.
	if pl.Changed() {
		if err := writeConfig(cfgFile, text); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "wrote", cfgFile)
	}
	if err := os.MkdirAll(res.Answers.Workspace, 0o700); err != nil {
		return report(stderr, fmt.Errorf("creating the workspace: %w", err))
	}
	if !start {
		fmt.Fprintf(stdout, "\nThe box was not started: %s.\n", why)
		return 0
	}
	if len(pl.MissingImages()) > 0 {
		fmt.Fprintln(stdout)
		if err := buildImages(ctx, dirs, stdout, gw, bx); err != nil {
			return report(stderr, fmt.Errorf("%w\nThe config is written. Fix what is shown above, then run ezbx build and ezbx up (or ezbx init again)", err))
		}
	}
	fmt.Fprintln(stdout)
	if code := runUp([]string{"--config", cfgFile}, stdout, stderr); code != 0 {
		fmt.Fprintln(stderr, "The config is written. Fix what is shown above, then run ezbx up (or ezbx init again).")
		return code
	}
	if len(res.Answers.Harnesses) > 0 {
		fmt.Fprintf(stdout, "\nNext: ezbx shell %s   (or ezbx shell for a terminal in the box)\n", res.Answers.Harnesses[0])
	}
	return 0
}

// preflight checks the system and waits for Docker if it isn't running.
func preflight(ctx context.Context, w io.Writer) error {
	ok := map[string]bool{"windows": true, "linux": true, "darwin": true}
	if !ok[runtime.GOOS] || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return fmt.Errorf("EZBox doesn't support %s/%s yet", runtime.GOOS, runtime.GOARCH)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("the docker command isn't installed. Install Docker Desktop (https://docs.docker.com/desktop/), then run ezbx init again. EZBox doesn't install it for you")
	}
	r := &box.Runner{Docker: dockerIn}
	err := r.CheckDocker(ctx)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "too old") {
		return err
	}
	// Never start Docker: say so and wait (decided in docs/init-flow.md).
	fmt.Fprintln(w, "Docker isn't answering. Start Docker Desktop; this waits up to 5 minutes (Ctrl+C to stop).")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
		if err = r.CheckDocker(ctx); err == nil {
			fmt.Fprintln(w, "Docker is answering.")
			return nil
		}
		if strings.Contains(err.Error(), "too old") {
			return err
		}
	}
	return err
}

// gatherFacts finds everything the questions need.
func gatherFacts(ctx context.Context, existing *config.Config, o setup.Options, dirs paths.Dirs) (setup.Facts, error) {
	home, _ := os.UserHomeDir()
	f := setup.Facts{Home: home, Own: []string{dirs.Config, dirs.State}, System: systemDirs(), Existing: existing, DockerMemory: dockerMemory(ctx)}

	all, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		return f, fmt.Errorf("internal error loading harnesses: %w", err)
	}
	for _, m := range all {
		if m.Verified.Status == "verified" {
			f.Harnesses = append(f.Harnesses, m)
		}
	}
	if f.Catalog, err = tools.Load(catalog.FS); err != nil {
		return f, fmt.Errorf("internal error loading the tool catalogue: %w", err)
	}

	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		return f, fmt.Errorf("internal error loading adapters: %w", err)
	}
	known := map[string]bool{"auto": true, "later": true}
	for _, a := range adapters {
		known[a.ID] = true
	}
	if o.Engine != "" && !known[o.Engine] {
		var ids []string
		for id := range known {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return f, usageError(fmt.Sprintf("--engine %q: use one of %s", o.Engine, strings.Join(ids, ", ")))
	}
	var urls []string
	if o.URL != "" {
		urls = append(urls, o.URL)
	}
	if existing != nil {
		for _, ov := range existing.Engine.Overrides {
			if ov.URL != "" {
				urls = append(urls, ov.URL)
			}
		}
	}
	sort.Strings(urls)
	res := (&detect.Detector{Adapters: adapters, Run: runCommand}).Detect(ctx, urls)
	f.Engines = res.Engines
	return f, nil
}

// imagesPresent reports whether each image is built from these inputs.
// This only asks the local Docker engine, so --plan stays off the network.
func imagesPresent(ctx context.Context, specs ...images.Spec) []setup.Image {
	ir := imageRunner(io.Discard)
	var out []setup.Image
	for _, s := range specs {
		out = append(out, setup.Image{Tag: s.Tag(), Present: ir.Check(ctx, s) == nil})
	}
	return out
}

// boxExists reports whether either of the box's containers exists.
func boxExists(ctx context.Context, name string) bool {
	n := box.NamesFor(name)
	for _, c := range []string{n.Gateway, n.Box} {
		if out, err := dockerIn(ctx, nil, "container", "ls", "-a", "--filter", "name="+c, "--format", "{{.Names}}"); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.TrimSpace(line) == c {
					return true
				}
			}
		}
	}
	return false
}

// writeConfig writes the config file, readable only by this user. An
// existing file is kept as FILE.previous first, so nothing is lost.
func writeConfig(file, text string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	if old, err := os.ReadFile(file); err == nil {
		if err := os.WriteFile(file+".previous", old, 0o600); err != nil {
			return fmt.Errorf("keeping the current config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := file + ".new"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func envName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
