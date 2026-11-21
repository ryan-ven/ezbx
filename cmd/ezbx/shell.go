package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"time"

	"github.com/ryan-ven/ezbx/harnesses"
	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/harness"
)

// runShell opens a terminal in the box, or starts a harness there, as the
// unprivileged agent user. Words after "--" go to the harness.
func runShell(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file, used to find the box")
	boxFlag := fs.String("box", "", "box name; default the one in the config, else main")
	noSnap := fs.Bool("no-snapshot", false, "don't snapshot the workspace first")
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(words) > 1 {
		fmt.Fprintln(stderr, "Usage: ezbx shell [HARNESS] [--box NAME] [--no-snapshot] [-- HARNESS ARGS...]")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	var id string
	if len(words) == 1 {
		id = words[0]
	}
	argv, err := shellArgs(t, id, rest, isTerminal(os.Stdin))
	if err != nil {
		return report(stderr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	r := &box.Runner{Docker: dockerIn}
	running, err := r.Running(ctx, box.NamesFor(t.name).Box)
	cancel()
	if err != nil {
		return report(stderr, err)
	}
	if !running {
		return report(stderr, box.ErrNotUp{Box: t.name, What: "the box container isn't running"})
	}

	// An undo point before every session. If it fails, the session doesn't
	// start, because the agent's changes couldn't be undone.
	if !*noSnap {
		sb, err := openSnapshots(t)
		if err == nil {
			err = takeAuto(sb, stderr)
		}
		if err != nil {
			return report(stderr, fmt.Errorf("the snapshot before the session failed, so the session wasn't started: %w\nTo start without one: ezbx shell --no-snapshot", err))
		}
	}

	// The session is interactive: docker takes over this terminal.
	cmd := exec.Command("docker", argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return report(stderr, err)
	}
	return 0
}

// shellArgs builds the docker exec command. Without a harness it opens bash
// in the workspace; with one, it runs the harness's launch command with its
// environment. Nothing here adds privileges: docker exec runs as the
// container's own user, the agent.
func shellArgs(t target, id string, extra []string, tty bool) ([]string, error) {
	a := []string{"exec", "-i"}
	if tty {
		a = append(a, "-t")
	}
	name := box.NamesFor(t.name).Box
	if id == "" {
		if len(extra) > 0 {
			return nil, usageError("Words after -- are for a harness: ezbx shell HARNESS -- ARGS")
		}
		return append(a, "-w", "/workspace", name, "bash"), nil
	}
	all, err := harness.LoadFS(harnesses.FS)
	if err != nil {
		return nil, fmt.Errorf("internal error loading harnesses: %w", err)
	}
	m, ok := harness.Find(all, id)
	if !ok {
		return nil, usageError(fmt.Sprintf("Unknown harness %q.", id))
	}
	if t.cfg != nil {
		_, b := t.cfg.Only()
		in := false
		for _, h := range b.Harnesses {
			in = in || h == id
		}
		if !in {
			return nil, fmt.Errorf("box %q doesn't have %s; add %q to harnesses in ezbx.toml, then ezbx down and ezbx up", t.name, m.Name, id)
		}
	}
	if len(m.Launch.Command) == 0 {
		return nil, fmt.Errorf("%s has no launch command in its manifest", m.Name)
	}
	if m.Launch.Cwd != "" {
		a = append(a, "-w", m.Launch.Cwd)
	}
	keys := make([]string, 0, len(m.Env))
	for k := range m.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "-e", k+"="+m.Env[k])
	}
	a = append(a, name)
	a = append(a, m.Launch.Command...)
	return append(a, extra...), nil
}
