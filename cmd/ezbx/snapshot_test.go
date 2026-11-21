package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/paths"
	"github.com/ryan-ven/ezbx/internal/snapshot"
)

// docker top's output, with the box idle and with a session.
func TestBusy(t *testing.T) {
	idle := `UID     PID     PPID    C    STIME   TTY   TIME       CMD
1000    4101    4080    0    07:01   ?     00:00:00   /sbin/docker-init -- sleep infinity
1000    4130    4101    0    07:01   ?     00:00:00   sleep infinity
`
	if got := busy(idle); len(got) != 0 {
		t.Errorf("idle box: %v", got)
	}
	session := idle + "1000    5000    4080    1    07:05   pts/0 00:00:02   node /usr/local/bin/pi\n1000    5010    5000    0    07:05   ?     00:00:00   sleep 30\n"
	if got := strings.Join(busy(session), "; "); got != "node /usr/local/bin/pi; sleep 30" {
		t.Errorf("session: %q", got)
	}
}

func TestSizeBytes(t *testing.T) {
	for in, want := range map[string]int64{"512k": 512 << 10, "100m": 100 << 20, "2g": 2 << 30} {
		if got := sizeBytes(in); got != want {
			t.Errorf("%s: %d", in, got)
		}
	}
}

func TestOpenSnapshots(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	dirs := paths.Dirs{Config: filepath.Join(dir, "cfg"), State: filepath.Join(dir, "state")}
	load := func(extra string) target {
		cfg, err := config.Parse("version = 1\n[box.main]\nworkspace = '" + ws + "'\nharnesses = ['pi']\n" + extra)
		if err != nil {
			t.Fatal(err)
		}
		return target{name: "main", cfg: cfg, dirs: dirs}
	}
	sb, err := openSnapshots(load(""))
	if err != nil {
		t.Fatal(err)
	}
	if sb.root != ws || sb.keep != snapshot.DefaultKeep || sb.rules.MaxFile != snapshot.DefaultMaxFile ||
		strings.Join(sb.rules.Skip, ",") != strings.Join(snapshot.DefaultSkip, ",") || sb.store.Dir != filepath.Join(dirs.State, "main", "snapshots") {
		t.Errorf("defaults: %+v", sb)
	}
	sb, err = openSnapshots(load("[box.main.snapshots]\nskip = ['*.log']\nmax_file = '5m'\nkeep = 3\n"))
	if err != nil || strings.Join(sb.rules.Skip, ",") != "*.log" || sb.rules.MaxFile != 5<<20 || sb.keep != 3 {
		t.Errorf("settings: %+v %v", sb, err)
	}
	// An empty skip list means skip nothing, not the defaults.
	if sb, _ = openSnapshots(load("[box.main.snapshots]\nskip = []\n")); len(sb.rules.Skip) != 0 {
		t.Errorf("skip = []: %v", sb.rules.Skip)
	}
	if _, err := openSnapshots(target{name: "main", dirs: dirs}); err == nil || !strings.Contains(err.Error(), "no ezbx.toml") {
		t.Error(err)
	}
	if _, err := openSnapshots(target{name: "other", cfg: load("").cfg, dirs: dirs}); err == nil {
		t.Error("a box not in the config must be refused")
	}
}
