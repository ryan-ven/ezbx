// Package paths finds EZBox's per-user folders: one for config and one for
// state (the admin token, logs and, later, snapshots).
//
// State is never inside a workspace and is never mounted into a box.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Dirs are the folders for one user.
type Dirs struct {
	Config string // holds ezbx.toml
	State  string // holds one folder per box
}

// Env reads an environment variable. It is a parameter so tests can supply
// their own.
type Env func(string) string

// Find works out the folders.
//
//   - EZBX_HOME, if set, holds both: config in it, state in its "state" folder.
//   - Windows: %LOCALAPPDATA%\ezbx, with state in its "state" folder. Not the
//     Roaming folder, which a domain profile copies between machines; the
//     token and snapshots must stay on this one.
//   - macOS: ~/Library/Application Support/ezbx, the same way.
//   - Linux and others: $XDG_CONFIG_HOME/ezbx and $XDG_STATE_HOME/ezbx, which
//     default to ~/.config/ezbx and ~/.local/state/ezbx.
func Find(goos string, env Env, home string) (Dirs, error) {
	if h := env("EZBX_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			return Dirs{}, errors.New("EZBX_HOME must be a full path")
		}
		return Dirs{Config: h, State: filepath.Join(h, "state")}, nil
	}
	switch goos {
	case "windows":
		base := env("LOCALAPPDATA")
		if base == "" {
			if home == "" {
				return Dirs{}, errors.New("cannot find the local app data folder; set LOCALAPPDATA or EZBX_HOME")
			}
			base = filepath.Join(home, "AppData", "Local")
		}
		d := filepath.Join(base, "ezbx")
		return Dirs{Config: d, State: filepath.Join(d, "state")}, nil
	case "darwin":
		if home == "" {
			return Dirs{}, errors.New("cannot find the home folder; set EZBX_HOME")
		}
		d := filepath.Join(home, "Library", "Application Support", "ezbx")
		return Dirs{Config: d, State: filepath.Join(d, "state")}, nil
	default:
		cfg, state := env("XDG_CONFIG_HOME"), env("XDG_STATE_HOME")
		// The XDG rules say relative values are invalid and must be ignored.
		if !filepath.IsAbs(cfg) {
			cfg = ""
		}
		if !filepath.IsAbs(state) {
			state = ""
		}
		if (cfg == "" || state == "") && home == "" {
			return Dirs{}, errors.New("cannot find the home folder; set EZBX_HOME")
		}
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		if state == "" {
			state = filepath.Join(home, ".local", "state")
		}
		return Dirs{Config: filepath.Join(cfg, "ezbx"), State: filepath.Join(state, "ezbx")}, nil
	}
}

// Default finds the folders for the current user on this computer.
func Default() (Dirs, error) {
	home, _ := os.UserHomeDir()
	return Find(runtime.GOOS, os.Getenv, home)
}

// Box is one box's state folder.
func (d Dirs) Box(name string) string { return filepath.Join(d.State, name) }

// ConfigFile is where ezbx.toml is looked for: --config first, then
// EZBX_CONFIG, then the config folder.
func (d Dirs) ConfigFile(flag string, env Env) string {
	if flag != "" {
		return flag
	}
	if c := env("EZBX_CONFIG"); c != "" {
		return c
	}
	return filepath.Join(d.Config, "ezbx.toml")
}

// ExpandHome turns a leading ~ into the home folder. Only "~" alone or "~"
// followed by a slash is expanded; "~name" is left as it is.
func ExpandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// Within reports whether path is dir itself or inside it. Both must be clean
// full paths. Windows paths are compared without regard to case.
func Within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(filepath.VolumeName(path), filepath.VolumeName(dir)) {
			return false
		}
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
