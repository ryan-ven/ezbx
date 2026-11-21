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
	"strings"
	"time"

	"github.com/ryan-ven/ezbx"
	"github.com/ryan-ven/ezbx/catalog"
	"github.com/ryan-ven/ezbx/harnesses"
	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/internal/images"
	"github.com/ryan-ven/ezbx/internal/paths"
	"github.com/ryan-ven/ezbx/internal/setup"
	"github.com/ryan-ven/ezbx/internal/tools"
)

// imageSpecs works out the two images a box needs, from the files carried
// in this program and the box's tools setting (nil means none chosen).
func imageSpecs(chosen []string) (gw, bx images.Spec, err error) {
	hs, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		return gw, bx, fmt.Errorf("internal error loading harnesses: %w", err)
	}
	cat, err := tools.Load(catalog.FS)
	if err != nil {
		return gw, bx, fmt.Errorf("internal error loading the tool catalogue: %w", err)
	}
	if gw, err = images.Gateway(ezbx.Source); err != nil {
		return gw, bx, err
	}
	if bx, err = images.Box(ezbx.Source, hs, cat, chosen); err != nil {
		return gw, bx, fmt.Errorf("tools: %w", err)
	}
	return gw, bx, nil
}

func planImage(s images.Spec) box.Image { return box.Image{Tag: s.Tag(), Inputs: s.Inputs} }

func imageRunner(stdout io.Writer) *images.Runner {
	return &images.Runner{Docker: dockerIn, Stream: dockerStream(stdout), Out: stdout}
}

// runBuild builds the images the config needs, if they aren't built yet,
// and records what it built in images.json next to the config.
func runBuild(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file; its tools setting decides what goes into the box image")
	prune := fs.Bool("prune", false, "then remove EZBox's older images, keeping the current ones and the ones before them")
	yes := fs.Bool("yes", false, "with --prune, remove without asking")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage: ezbx build [--config FILE] [--prune [--yes]]")
		return 2
	}
	dirs, err := paths.Default()
	if err != nil {
		return report(stderr, err)
	}
	cfgFile := dirs.ConfigFile(*cfgFlag, os.Getenv)
	var chosen []string
	switch cfg, err := config.Load(cfgFile); {
	case err == nil:
		_, b := cfg.Only()
		chosen = b.Tools
		fmt.Fprintln(stdout, "config:", cfgFile)
	case *cfgFlag == "" && os.Getenv("EZBX_CONFIG") == "" && errors.Is(statErr(cfgFile), os.ErrNotExist):
		fmt.Fprintln(stdout, "No config yet, so the box image gets only the harnesses' own tools. Run ezbx init to choose more.")
	default:
		return report(stderr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	r := &box.Runner{Docker: dockerIn, Out: stdout}
	if err := r.CheckDocker(ctx); err != nil {
		return report(stderr, err)
	}
	gw, bx, err := imageSpecs(chosen)
	if err != nil {
		return report(stderr, err)
	}
	if err := buildImages(ctx, dirs, stdout, gw, bx); err != nil {
		return report(stderr, err)
	}
	if *prune {
		return pruneImages(ctx, dirs, stdout, stderr, *yes, gw, bx)
	}
	return 0
}

// buildImages builds each image that isn't built yet and records it.
func buildImages(ctx context.Context, dirs paths.Dirs, stdout io.Writer, specs ...images.Spec) error {
	return buildImagesWith(ctx, dirs, stdout, false, specs...)
}

// rebuildImages builds every layer again, even of images that are built,
// for ezbx reset --rebuild.
func rebuildImages(ctx context.Context, dirs paths.Dirs, stdout io.Writer, specs ...images.Spec) error {
	return buildImagesWith(ctx, dirs, stdout, true, specs...)
}

func buildImagesWith(ctx context.Context, dirs paths.Dirs, stdout io.Writer, again bool, specs ...images.Spec) error {
	ir := imageRunner(stdout)
	ir.NoCache = again
	file := images.RecordFile(dirs.Config)
	rec, err := images.ReadRecord(file)
	if err != nil {
		return err
	}
	for _, s := range specs {
		err := ir.Check(ctx, s)
		var missing images.ErrNotBuilt
		switch {
		case err == nil && again:
		case err == nil:
			fmt.Fprintf(stdout, "%s is built already (%s image)\n", s.Tag(), s.Kind)
			if b, ok := rec.Images[s.Kind]; !ok || b.Tag != s.Tag() {
				// Built by an earlier run that didn't get to record it.
				if b, err := ir.Observe(ctx, s); err == nil {
					rec.Put(s.Kind, b)
				}
			}
			continue
		case !errors.As(err, &missing):
			return err
		}
		b, err := ir.Build(ctx, s)
		if err != nil {
			return err
		}
		rec.Put(s.Kind, b)
		if err := rec.Write(file); err != nil {
			return fmt.Errorf("the image is built, but recording it in %s failed: %w", file, err)
		}
		fmt.Fprintf(stdout, "built %s\n", s.Tag())
		if s.Kind == "box" {
			fmt.Fprintf(stdout, "  tools: %s\n", toolsLine(s.Tools.Chosen))
		}
	}
	if err := rec.Write(file); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "recorded in", file)
	return nil
}

func toolsLine(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// pruneImages removes EZBox's older gateway and box images after asking.
// The current images and the ones they replaced are kept, and so is any
// image a container uses, running or stopped.
func pruneImages(ctx context.Context, dirs paths.Dirs, stdout, stderr io.Writer, yes bool, current ...images.Spec) int {
	rec, err := images.ReadRecord(images.RecordFile(dirs.Config))
	if err != nil {
		return report(stderr, err)
	}
	ir := imageRunner(stdout)
	old, inUse, err := ir.Old(ctx, rec.Keep(current...))
	if err != nil {
		return report(stderr, err)
	}
	for _, t := range inUse {
		fmt.Fprintf(stdout, "kept %s: a container uses it (ezbx down first to remove it)\n", t)
	}
	if len(old) == 0 {
		fmt.Fprintln(stdout, "No older EZBox images to remove.")
		return 0
	}
	fmt.Fprintln(stdout, "\nOlder EZBox images (the current ones and the ones before them are kept):")
	for _, t := range old {
		fmt.Fprintln(stdout, "  "+t)
	}
	if !yes {
		p := &setup.Prompter{In: bufio.NewReader(stdin), Out: stdout}
		ok, err := p.YesNo(fmt.Sprintf("Remove these %d image(s)?", len(old)), false)
		if err != nil {
			return report(stderr, err)
		}
		if !ok {
			fmt.Fprintln(stdout, "Nothing was removed.")
			return 0
		}
	}
	failed := 0
	for _, t := range old {
		if err := ir.Remove(ctx, t); err != nil {
			fmt.Fprintf(stderr, "kept %s: %v\n", t, firstLine(err))
			failed++
			continue
		}
		fmt.Fprintln(stdout, "removed", t)
	}
	if failed > 0 {
		fmt.Fprintln(stderr, "Some images were kept (see above).")
		return 1
	}
	return 0
}

// firstLine keeps the first line of an error, which for docker is the
// message without its usage text.
func firstLine(err error) string {
	s, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	return s
}

func statErr(file string) error {
	_, err := os.Stat(file)
	return err
}

// dockerStream runs a long docker command with its output going straight to
// the person's terminal, and the build context on standard input.
func dockerStream(out io.Writer) func(ctx context.Context, stdin []byte, args ...string) error {
	return func(ctx context.Context, stdin []byte, args ...string) error {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdin = strings.NewReader(string(stdin))
		cmd.Stdout, cmd.Stderr = out, out
		return cmd.Run()
	}
}
