package main

import (
	"flag"
	"os"

	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/paths"
)

// target is the box a command acts on.
type target struct {
	name string
	cfg  *config.Config // nil when there is no config file
	dirs paths.Dirs
}

// resolveBox finds the box: --box if given, else the one box in the config,
// else "main". A config named with --config must load.
func resolveBox(cfgFlag, boxFlag string) (target, error) {
	dirs, err := paths.Default()
	if err != nil {
		return target{}, err
	}
	t := target{dirs: dirs, name: boxFlag}
	cfg, err := config.Load(dirs.ConfigFile(cfgFlag, os.Getenv))
	if err != nil && cfgFlag != "" {
		return target{}, err
	}
	if err == nil {
		t.cfg = cfg
		if t.name == "" {
			t.name, _ = cfg.Only()
		}
	}
	if t.name == "" {
		t.name = "main"
	}
	if !config.BoxName(t.name) {
		return target{}, usageError("A box name is short, lowercase letters, digits and dashes.")
	}
	return t, nil
}

// usageError is a mistake in how a command was typed (exit code 2).
type usageError string

func (e usageError) Error() string { return string(e) }

// parseInterleaved parses flags that may come before, between or after the
// other words, which Go's flag package alone doesn't allow. Everything after
// the first "--" is not parsed and comes back as rest, for passing on to a
// harness.
func parseInterleaved(fs *flag.FlagSet, args []string) (words, rest []string, err error) {
	for i, a := range args {
		if a == "--" {
			args, rest = args[:i], append([]string{}, args[i+1:]...)
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			return nil, nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return words, rest, nil
		}
		words = append(words, args[0])
		args = args[1:]
	}
}
