package main

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/paths"
)

// fakeGateway answers like a running box's gateway through docker exec.
type fakeGateway struct {
	up    bool
	mode  string
	allow []string
	execs []string
}

func (g *fakeGateway) docker(_ context.Context, _ []byte, args ...string) ([]byte, error) {
	switch {
	case args[0] == "container" && args[1] == "ls":
		if !g.up {
			return nil, nil
		}
		return []byte("ezbx-t-gateway|true\n"), nil
	case args[0] == "container" && args[1] == "inspect":
		return []byte("true"), nil
	case args[0] == "exec":
		call := args[7:] // after: exec NAME /ezbx gateway admin --addr ADDR
		g.execs = append(g.execs, strings.Join(call, " "))
		switch call[0] {
		case "network":
			g.mode, g.allow = call[1], append([]string{}, call[2:]...)
		case "allow":
			g.allow = append(g.allow, call[1])
		}
		out, _ := json.Marshal(map[string]any{
			"engine": "strata", "upstream": "http://host.docker.internal:53000",
			"network": map[string]any{"mode": g.mode, "allow": g.allow},
			"proxy":   map[string]any{"allowed": 1, "blocked": 2},
			"mode":    g.mode, "allow": g.allow, "closed": 1,
		})
		return out, nil
	}
	return nil, nil
}

func netTarget(t *testing.T) (target, string) {
	dir := t.TempDir()
	tg := target{name: "t", cfg: cfgWith(t, `["pi"]`), dirs: paths.Dirs{Config: dir, State: filepath.Join(dir, "state")}}
	return tg, box.ApprovedFileFor(tg.dirs.Box("t"))
}

func TestNetModeKeepsTheList(t *testing.T) {
	g := &fakeGateway{up: true, mode: "allow", allow: []string{"github.com", "registry.npmjs.org"}}
	tg, _ := netTarget(t)
	var out strings.Builder
	if err := setMode(context.Background(), &box.Runner{Docker: g.docker}, tg, "on", &out); err != nil {
		t.Fatal(err)
	}
	if g.mode != "on" || strings.Join(g.allow, ",") != "github.com,registry.npmjs.org" {
		t.Errorf("mode %s allow %v", g.mode, g.allow)
	}
	if !strings.Contains(out.String(), "until the next ezbx up, which goes back to off") {
		t.Error(out.String())
	}
}

func TestNetAllowSavesAndApplies(t *testing.T) {
	g := &fakeGateway{up: true, mode: "allow"}
	tg, file := netTarget(t)
	if err := allowHosts(context.Background(), &box.Runner{Docker: g.docker}, tg, file, []string{"GitHub.com"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	saved, _ := box.ReadHosts(file)
	if strings.Join(saved, ",") != "github.com" || strings.Join(g.allow, ",") != "github.com" {
		t.Errorf("saved %v, live %v", saved, g.allow)
	}

	// A bad host changes nothing, saved or live.
	err := allowHosts(context.Background(), &box.Runner{Docker: g.docker}, tg, file, []string{"pypi.org", "10.0.0.1"}, io.Discard)
	if err == nil {
		t.Fatal("an address must be refused")
	}
	saved, _ = box.ReadHosts(file)
	if len(saved) != 1 || len(g.allow) != 1 {
		t.Errorf("a refused list was partly applied: saved %v, live %v", saved, g.allow)
	}

	// With the box down, the host is still saved.
	down := &fakeGateway{}
	var out strings.Builder
	if err := allowHosts(context.Background(), &box.Runner{Docker: down.docker}, tg, file, []string{"pypi.org"}, &out); err != nil {
		t.Fatal(err)
	}
	if saved, _ = box.ReadHosts(file); len(saved) != 2 || !strings.Contains(out.String(), "not up") {
		t.Errorf("saved %v: %s", saved, out.String())
	}
}

func TestNetRemove(t *testing.T) {
	g := &fakeGateway{up: true, mode: "allow", allow: []string{"github.com", "pypi.org"}}
	tg, file := netTarget(t)
	box.AddHost(file, "github.com")
	box.AddHost(file, "pypi.org")
	if err := removeHosts(context.Background(), &box.Runner{Docker: g.docker}, tg, file, []string{"GITHUB.COM"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	saved, _ := box.ReadHosts(file)
	if strings.Join(saved, ",") != "pypi.org" || strings.Join(g.allow, ",") != "pypi.org" || g.mode != "allow" {
		t.Errorf("saved %v, live %s %v", saved, g.mode, g.allow)
	}
}

func TestNetShowNeedsTheBox(t *testing.T) {
	tg, _ := netTarget(t)
	err := showNet(context.Background(), &box.Runner{Docker: (&fakeGateway{}).docker}, tg, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not up") {
		t.Fatal(err)
	}
	g := &fakeGateway{up: true, mode: "on", allow: []string{"github.com"}}
	var out strings.Builder
	if err := showNet(context.Background(), &box.Runner{Docker: g.docker}, tg, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"network on", "ezbx.toml says off", "github.com", "2 blocked"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}
