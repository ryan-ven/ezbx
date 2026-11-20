package box

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Docker runs one docker command, with stdin when it isn't nil, and returns
// what it printed. What docker prints on standard error comes back in the
// error. It is a field so tests can replace it.
type Docker func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)

// Runner carries out a plan.
type Runner struct {
	Docker Docker
	Out    io.Writer     // progress, one line per step
	Wait   time.Duration // how long to wait for the gateway; 0 means 45s
	Sleep  func(time.Duration)
}

func (r *Runner) say(format string, a ...any) {
	if r.Out != nil {
		fmt.Fprintf(r.Out, format+"\n", a...)
	}
}

func (r *Runner) run(ctx context.Context, args ...string) ([]byte, error) {
	return r.Docker(ctx, nil, args...)
}

// found describes a Docker resource looked up by its exact name.
type found struct {
	exists, managed bool
}

// lookup finds a container, network or volume by exact name and reports
// whether EZBox made it. Docker's name filter matches parts of names, so the
// exact name is checked here.
func (r *Runner) lookup(ctx context.Context, kind, name string) (found, error) {
	var args []string
	switch kind {
	case "container":
		args = []string{"container", "ls", "-a", "--filter", "name=" + name, "--format", `{{.Names}}|{{.Label "ezbx.managed"}}`}
	case "network", "volume":
		args = []string{kind, "ls", "--filter", "name=" + name, "--format", `{{.Name}}|{{.Label "ezbx.managed"}}`}
	default:
		return found{}, errors.New("unknown kind " + kind)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return found{}, fmt.Errorf("docker %s ls: %w", kind, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		n, label, _ := strings.Cut(strings.TrimSpace(line), "|")
		if n == name {
			return found{exists: true, managed: label == "true"}, nil
		}
	}
	return found{}, nil
}

// CheckDocker makes sure the Docker engine answers and is new enough for the
// isolated network option.
func (r *Runner) CheckDocker(ctx context.Context) error {
	out, err := r.run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return fmt.Errorf("the Docker engine doesn't answer (%v). Start Docker Desktop, then try again", firstLine(err))
	}
	v := strings.TrimSpace(string(out))
	major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if major < 28 {
		return fmt.Errorf("Docker Engine %s is too old: EZBox needs 28 or later for the isolated network option. Update Docker Desktop", v)
	}
	return nil
}

// Up creates and starts everything in the plan. It refuses to touch any
// resource with one of the box's names that EZBox didn't create. If the box
// is already up, it says so and changes nothing.
func (r *Runner) Up(ctx context.Context, p Plan) error {
	if err := r.CheckDocker(ctx); err != nil {
		return err
	}
	for _, img := range []string{GatewayImage, BoxImage} {
		if _, err := r.run(ctx, "image", "inspect", "--format", "{{.Id}}", img); err != nil {
			return fmt.Errorf("the image %s is missing. Build it from the repository: docker build -f docker/%s.Dockerfile -t %s .", img, strings.TrimSuffix(strings.TrimPrefix(img, "ezbx-"), ":dev"), img)
		}
	}

	// Containers: none may exist yet.
	for _, name := range []string{p.Names.Gateway, p.Names.Box} {
		f, err := r.lookup(ctx, "container", name)
		if err != nil {
			return err
		}
		if f.exists && !f.managed {
			return fmt.Errorf("a container named %s exists, but EZBox didn't create it, so EZBox won't touch it. Rename or remove it yourself", name)
		}
		if f.exists {
			return fmt.Errorf("box %q is already up (or partly up). To apply changes, run ezbx down, then ezbx up", p.Box)
		}
	}

	// Networks and the home volume may be left from an earlier run. They are
	// reused only if EZBox made them, and the inside network only if it is
	// still isolated.
	if err := r.ensureNetwork(ctx, p.Names.Inside, p.InsideNetworkArgs(), true); err != nil {
		return err
	}
	if err := r.ensureNetwork(ctx, p.Names.Outside, p.OutsideNetworkArgs(), false); err != nil {
		return err
	}
	if f, err := r.lookup(ctx, "volume", p.Names.Home); err != nil {
		return err
	} else if f.exists && !f.managed {
		return fmt.Errorf("a volume named %s exists, but EZBox didn't create it, so EZBox won't use it", p.Names.Home)
	} else if !f.exists {
		if _, err := r.run(ctx, p.HomeVolumeArgs()...); err != nil {
			return fmt.Errorf("creating the home volume: %w", err)
		}
		r.say("created volume %s (the agent's home)", p.Names.Home)
	} else {
		r.say("kept volume %s (the agent's home)", p.Names.Home)
	}

	// Host folders.
	if err := os.MkdirAll(p.Workspace, 0o700); err != nil {
		return fmt.Errorf("creating the workspace: %w", err)
	}
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return fmt.Errorf("creating the state folder: %w", err)
	}
	if p.LogOn {
		if err := os.MkdirAll(p.LogDir(), 0o700); err != nil {
			return fmt.Errorf("creating the log folder: %w", err)
		}
	}
	made, err := EnsureToken(p.TokenFile())
	if err != nil {
		return err
	}
	if made {
		r.say("created the admin token in %s", p.StateDir)
	}

	// The gateway, then the box.
	if _, err := r.run(ctx, p.GatewayArgs()...); err != nil {
		return fmt.Errorf("creating the gateway: %w%s", err, cleanupHint)
	}
	if _, err := r.run(ctx, p.ConnectInsideArgs()...); err != nil {
		return fmt.Errorf("connecting the gateway to the inside network: %w%s", err, cleanupHint)
	}
	if _, err := r.run(ctx, "start", p.Names.Gateway); err != nil {
		return fmt.Errorf("starting the gateway: %w%s", err, cleanupHint)
	}
	r.say("started %s (engine %s at %s, network %s)", p.Names.Gateway, p.EngineName, p.Upstream, p.Network)
	if err := r.waitHealthy(ctx, p.Names.Gateway); err != nil {
		return err
	}

	if _, err := r.run(ctx, p.BoxArgs()...); err != nil {
		return fmt.Errorf("creating the box: %w%s", err, cleanupHint)
	}
	if len(p.HomeFiles) > 0 {
		data, err := p.HomeTar()
		if err != nil {
			return err
		}
		if _, err := r.Docker(ctx, data, p.HomeCopyArgs()...); err != nil {
			return fmt.Errorf("copying the harness config into the box: %w%s", err, cleanupHint)
		}
	}
	if _, err := r.run(ctx, "start", p.Names.Box); err != nil {
		return fmt.Errorf("starting the box: %w%s", err, cleanupHint)
	}
	r.say("started %s (workspace %s, harnesses %s)", p.Names.Box, p.Workspace, strings.Join(p.Harnesses, ", "))
	return nil
}

const cleanupHint = ". Run ezbx down to remove what was made, then try again"

func (r *Runner) ensureNetwork(ctx context.Context, name string, create []string, inside bool) error {
	f, err := r.lookup(ctx, "network", name)
	if err != nil {
		return err
	}
	if f.exists && !f.managed {
		return fmt.Errorf("a network named %s exists, but EZBox didn't create it, so EZBox won't use it", name)
	}
	if !f.exists {
		if _, err := r.run(ctx, create...); err != nil {
			return fmt.Errorf("creating network %s: %w", name, err)
		}
		r.say("created network %s", name)
		return nil
	}
	out, err := r.run(ctx, "network", "inspect", name)
	if err != nil {
		return fmt.Errorf("inspecting network %s: %w", name, err)
	}
	var list []struct {
		Internal   bool              `json:"Internal"`
		EnableIPv6 bool              `json:"EnableIPv6"`
		Options    map[string]string `json:"Options"`
	}
	if json.Unmarshal(out, &list) != nil || len(list) != 1 {
		return fmt.Errorf("unexpected answer from docker network inspect %s", name)
	}
	n := list[0]
	if inside && (!n.Internal || n.Options[isolatedOption] != "isolated" || n.EnableIPv6 || n.Options[ipv6Option] == "true") {
		return fmt.Errorf("network %s exists but isn't isolated. Run ezbx down, then ezbx up, to recreate it", name)
	}
	if !inside && n.Internal {
		return fmt.Errorf("network %s exists but is internal, so the gateway can't reach the engine. Run ezbx down, then ezbx up", name)
	}
	r.say("kept network %s", name)
	return nil
}

// waitHealthy waits for the gateway's healthcheck to pass, and shows its
// last log lines if it stops or fails.
func (r *Runner) waitHealthy(ctx context.Context, name string) error {
	wait := r.Wait
	if wait == 0 {
		wait = 45 * time.Second
	}
	sleep := r.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := time.Now().Add(wait)
	last := ""
	for {
		out, err := r.run(ctx, "container", "inspect", "--format", `{{.State.Running}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}`, name)
		if err != nil {
			return fmt.Errorf("checking the gateway: %w", err)
		}
		running, health, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
		last = health
		if running != "true" {
			return fmt.Errorf("the gateway stopped right after starting.%s%s", r.logTail(ctx, name), cleanupHint)
		}
		if health == "healthy" {
			r.say("gateway is healthy")
			return nil
		}
		if health == "unhealthy" || time.Now().After(deadline) {
			break
		}
		sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("the gateway isn't healthy (status %q).%s%s", last, r.logTail(ctx, name), cleanupHint)
}

func (r *Runner) logTail(ctx context.Context, name string) string {
	out, err := r.run(ctx, "logs", "--tail", "5", name)
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return " See docker logs " + name + "."
	}
	return " Its last lines:\n" + strings.TrimRight(string(out), "\n") + "\n"
}

// Down removes the box's containers and networks, only those EZBox made.
// The home volume, the state folder and the workspace are kept.
func (r *Runner) Down(ctx context.Context, box string) error {
	if err := r.CheckDocker(ctx); err != nil {
		return err
	}
	n := NamesFor(box)
	var refused []string
	removed := 0
	for _, item := range []struct{ kind, name string }{
		{"container", n.Box}, {"container", n.Gateway}, {"network", n.Inside}, {"network", n.Outside},
	} {
		f, err := r.lookup(ctx, item.kind, item.name)
		if err != nil {
			return err
		}
		switch {
		case !f.exists:
			continue
		case !f.managed:
			refused = append(refused, item.kind+" "+item.name)
			continue
		}
		args := []string{"network", "rm", item.name}
		if item.kind == "container" {
			args = []string{"container", "rm", "--force", item.name}
		}
		if _, err := r.run(ctx, args...); err != nil {
			return fmt.Errorf("removing %s %s: %w", item.kind, item.name, err)
		}
		removed++
		r.say("removed %s %s", item.kind, item.name)
	}
	if removed == 0 && len(refused) == 0 {
		r.say("box %q was not up; nothing to remove", box)
	}
	if len(refused) > 0 {
		return fmt.Errorf("left alone because EZBox didn't create them: %s", strings.Join(refused, ", "))
	}
	return nil
}

// EnsureToken creates the admin token file if it is missing: 32 random bytes
// as 64 hex characters, readable only by this user. An existing token is kept
// if it is long enough. It reports whether it made a new one.
func EnsureToken(file string) (bool, error) {
	if data, err := os.ReadFile(file); err == nil {
		if len(strings.TrimSpace(string(data))) < 32 {
			return false, fmt.Errorf("the admin token in %s is shorter than 32 characters. Delete the file and run ezbx up again to make a new one", file)
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return false, err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("creating the admin token: %w", err)
	}
	if _, err := f.WriteString(hex.EncodeToString(b)); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

func firstLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}
