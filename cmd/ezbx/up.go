package main

import (
	"bytes"
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
	"strconv"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/harnesses"
	"github.com/ryan-ven/ezbx/internal/adapter"
	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/detect"
	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/internal/paths"
	"github.com/ryan-ven/ezbx/internal/setup"
	"github.com/ryan-ven/ezbx/providers"
)

// runUp starts the box described in ezbx.toml, then runs the doctor.
func runUp(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file; default EZBX_CONFIG, then ezbx.toml in the config folder")
	noDoctor := fs.Bool("no-doctor", false, "skip the doctor's checks at the end")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx up [--config FILE] [--no-doctor]")
		return 2
	}
	dirs, err := paths.Default()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfgFile := dirs.ConfigFile(*cfgFlag, os.Getenv)
	cfg, err := config.Load(cfgFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "config:", cfgFile)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	plan, err := makePlan(ctx, cfg, dirs, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	r := &box.Runner{Docker: dockerIn, Out: stdout}
	if err := r.Up(ctx, plan); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *noDoctor {
		fmt.Fprintf(stdout, "Box %q is up.\n", plan.Box)
		return 0
	}
	fmt.Fprintln(stdout)
	if code := runDoctor([]string{"--box", plan.Box}, stdout, stderr); code != 0 {
		fmt.Fprintf(stderr, "Box %q is up, but the doctor found problems (above).\n", plan.Box)
		return code
	}
	fmt.Fprintf(stdout, "\nBox %q is up. Open it with: docker exec -it %s bash\n", plan.Box, plan.Names.Box)
	return 0
}

// runDown removes a box's containers and networks. It never removes the
// workspace, the state folder or the agent's home volume.
func runDown(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file, used to find the box name")
	boxFlag := fs.String("box", "", "box name; default the one in the config, else main")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx down [--box NAME] [--config FILE]")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := &box.Runner{Docker: dockerIn, Out: stdout}
	if err := r.Down(ctx, t.name); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "Kept: the workspace, the state folder (token, log, approved hosts) and the volume %s.\n", box.NamesFor(t.name).Home)
	return 0
}

// makePlan turns the config into a plan: it checks the workspace, finds the
// engine and renders each harness's config. Nothing is changed yet.
func makePlan(ctx context.Context, cfg *config.Config, dirs paths.Dirs, stdout io.Writer) (box.Plan, error) {
	name, b := cfg.Only()
	home, _ := os.UserHomeDir()

	ws := paths.ExpandHome(b.Workspace, home)
	if !filepath.IsAbs(ws) {
		return box.Plan{}, fmt.Errorf("workspace %q must be a full path, or start with ~", b.Workspace)
	}
	ws = filepath.Clean(ws)
	if err := box.CheckWorkspace(ws, home, []string{dirs.Config, dirs.State}, systemDirs()); err != nil {
		return box.Plan{}, err
	}

	chosen, err := boxHarnesses(b)
	if err != nil {
		return box.Plan{}, err
	}
	eng, override, err := chooseEngine(ctx, cfg)
	if err != nil {
		return box.Plan{}, err
	}
	upstream, err := box.GatewayUpstream(eng.URL)
	if err != nil {
		return box.Plan{}, err
	}
	if override.KeyEnv != "" && os.Getenv(override.KeyEnv) == "" {
		return box.Plan{}, fmt.Errorf("engine.%s.key_env names %s, but that environment variable is empty or not set", eng.ID, override.KeyEnv)
	}
	fmt.Fprintf(stdout, "engine: %s at %s, %d model(s)\n", eng.Name, eng.URL, len(eng.Models))
	files, err := renderHome(cfg, chosen, eng)
	if err != nil {
		return box.Plan{}, err
	}

	return box.Plan{
		Box: name, Names: box.NamesFor(name), Workspace: ws, StateDir: dirs.Box(name), LogOn: cfg.LogOn(),
		Upstream: upstream, EngineID: eng.ID, EngineName: eng.Name, UpstreamHost: override.Host, KeyEnv: override.KeyEnv,
		Network: b.Network, Allow: box.AllowList(b.Allow, chosen),
		Memory: memoryLimit(ctx, b.Memory), CPUs: b.CPUsFlag(), PIDs: b.PIDs,
		Harnesses: b.Harnesses, HomeFiles: files,
	}, nil
}

// memoryLimit is the configured limit, or, when there is none, half of
// Docker's memory from 2g to 8g (the same rule as ezbx init).
func memoryLimit(ctx context.Context, configured string) string {
	if configured != "" {
		return configured
	}
	return setup.MemoryFor(dockerMemory(ctx))
}

// dockerMemory is the memory Docker has, in bytes, or 0 if Docker doesn't
// say. On Docker Desktop this is its virtual machine's memory.
func dockerMemory(ctx context.Context) int64 {
	out, err := dockerIn(ctx, nil, "info", "--format", "{{.MemTotal}}")
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

// boxHarnesses loads the manifests of a box's harnesses. Only verified ones
// are in the box image.
func boxHarnesses(b *config.Box) ([]harness.Manifest, error) {
	all, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		return nil, fmt.Errorf("internal error loading harnesses: %w", err)
	}
	var chosen []harness.Manifest
	for _, id := range b.Harnesses {
		m, ok := harness.Find(all, id)
		if !ok {
			return nil, fmt.Errorf("unknown harness %q", id)
		}
		if m.Verified.Status != "verified" {
			return nil, fmt.Errorf("harness %s is not verified yet, so it isn't in the box image", m.Name)
		}
		chosen = append(chosen, m)
	}
	return chosen, nil
}

// renderHome fills in each harness's config from the engine's models and
// the config's model overrides.
func renderHome(cfg *config.Config, chosen []harness.Manifest, eng detect.Engine) ([]harness.File, error) {
	overrides := map[string]harness.Override{}
	for id, m := range cfg.Models {
		overrides[id] = harness.Override{Name: m.Name, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Reasoning: m.Reasoning}
	}
	models := harness.Models(eng.Models, harness.Defaults{}, overrides)
	if len(models) == 0 {
		return nil, fmt.Errorf("%s lists no models. Load a model, then try again", eng.Name)
	}
	var files []harness.File
	seen := map[string]string{}
	for _, m := range chosen {
		out, err := harness.Render(m, harness.Values{GatewayBaseURL: box.GatewayBaseURL, GatewayKey: gatewayKey, Models: models})
		if err != nil {
			return nil, err
		}
		for _, f := range out {
			if other, dup := seen[f.Path]; dup {
				return nil, fmt.Errorf("harnesses %s and %s both write ~/%s", other, m.ID, f.Path)
			}
			seen[f.Path] = m.ID
		}
		files = append(files, out...)
	}
	return files, nil
}

// chooseEngine finds the engine the config asks for, with its overrides.
func chooseEngine(ctx context.Context, cfg *config.Config) (detect.Engine, config.EngineURL, error) {
	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		return detect.Engine{}, config.EngineURL{}, fmt.Errorf("internal error loading adapters: %w", err)
	}
	ids := map[string]bool{}
	var idList []string
	for _, a := range adapters {
		ids[a.ID] = true
		idList = append(idList, a.ID)
	}
	sort.Strings(idList)
	use := cfg.Engine.Use
	if use != "auto" && !ids[use] {
		return detect.Engine{}, config.EngineURL{}, fmt.Errorf("engine.use %q: use auto or one of %s", use, strings.Join(idList, ", "))
	}
	var urls []string
	for id, o := range cfg.Engine.Overrides {
		if !ids[id] {
			return detect.Engine{}, config.EngineURL{}, fmt.Errorf("[engine.%s]: no such engine; use one of %s", id, strings.Join(idList, ", "))
		}
		if o.URL != "" && (use == "auto" || use == id) {
			urls = append(urls, o.URL)
		}
	}
	sort.Strings(urls)
	d := &detect.Detector{Adapters: adapters, Run: runCommand}
	if use != "auto" && cfg.Engine.Overrides[use].URL != "" {
		// The config says exactly where the engine is: check only there, and
		// leave other engines on this computer alone.
		d.Run, d.SkipHintPorts = nil, true
	}
	res := d.Detect(ctx, urls)
	var matches []detect.Engine
	for _, e := range res.Engines {
		if use == "auto" || e.ID == use {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], cfg.Engine.Overrides[matches[0].ID], nil
	case 0:
		what := "No inference engine found"
		if use != "auto" {
			what = "Engine " + use + " not found"
		}
		return detect.Engine{}, config.EngineURL{}, errors.New(what + ". Start it (and load a model), then try again. If it runs on an unusual address, set url under [engine.ID] in ezbx.toml, or use ezbx engine use ID --url ADDRESS on a running box")
	default:
		var found []string
		for _, e := range matches {
			found = append(found, e.ID+" at "+e.URL)
		}
		return detect.Engine{}, config.EngineURL{}, fmt.Errorf("more than one engine found (%s). Choose one with engine.use in the config", strings.Join(found, "; "))
	}
}

// systemDirs are folders that must never be a workspace.
func systemDirs() []string {
	if runtime.GOOS == "windows" {
		var out []string
		for _, k := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
			if v := os.Getenv(k); v != "" {
				out = append(out, filepath.Clean(v))
			}
		}
		return out
	}
	return []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/sbin", "/sys", "/usr",
		"/System", "/Library", "/Applications", "/private/etc"}
}

// dockerIn runs the docker command, with stdin when given.
func dockerIn(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), errors.New(msg)
	}
	return out.Bytes(), nil
}
