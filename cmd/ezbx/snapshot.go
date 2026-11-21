package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/paths"
	"github.com/ryan-ven/ezbx/internal/setup"
	"github.com/ryan-ven/ezbx/internal/snapshot"
)

// snapBox is what the snapshot commands need for one box.
type snapBox struct {
	name  string
	root  string // the workspace on this computer
	store *snapshot.Store
	rules snapshot.Rules
	keep  int
}

// openSnapshots finds the box's workspace, its snapshot store (in the box's
// state folder, never mounted into the box) and its rules from ezbx.toml.
func openSnapshots(t target) (snapBox, error) {
	if t.cfg == nil {
		return snapBox{}, errors.New("no ezbx.toml was found, so the workspace isn't known. Run ezbx init, or pass --config FILE")
	}
	name, b := t.cfg.Only()
	if t.name != name {
		return snapBox{}, fmt.Errorf("box %q is not the one in ezbx.toml (%q)", t.name, name)
	}
	root, err := workspaceOf(b)
	if err != nil {
		return snapBox{}, err
	}
	sb := snapBox{name: name, root: root, keep: snapshot.DefaultKeep,
		rules: snapshot.Rules{Skip: snapshot.DefaultSkip, MaxFile: snapshot.DefaultMaxFile}}
	if b.Snapshots.Skip != nil {
		sb.rules.Skip = b.Snapshots.Skip
	}
	if b.Snapshots.MaxFile != "" {
		sb.rules.MaxFile = sizeBytes(b.Snapshots.MaxFile)
	}
	if b.Snapshots.Keep > 0 {
		sb.keep = b.Snapshots.Keep
	}
	if sb.store, err = snapshot.Open(filepath.Join(t.dirs.Box(name), "snapshots")); err != nil {
		return snapBox{}, fmt.Errorf("opening the snapshot store: %w", err)
	}
	return sb, nil
}

// workspaceOf is the box's workspace as a full path on this computer.
func workspaceOf(b *config.Box) (string, error) {
	home, _ := os.UserHomeDir()
	ws := paths.ExpandHome(b.Workspace, home)
	if !filepath.IsAbs(ws) {
		return "", fmt.Errorf("workspace %q must be a full path, or start with ~", b.Workspace)
	}
	return filepath.Clean(ws), nil
}

// sizeBytes turns a checked size such as 100m into bytes.
func sizeBytes(s string) int64 {
	n, _ := strconv.ParseInt(s[:len(s)-1], 10, 64)
	switch s[len(s)-1] {
	case 'k':
		return n << 10
	case 'm':
		return n << 20
	}
	return n << 30
}

// takeAuto takes the snapshot ezbx shell takes before every session, then
// drops automatic snapshots beyond the number kept.
func takeAuto(sb snapBox, stdout io.Writer) error {
	snap, st, err := sb.store.Take(sb.root, snapshot.Auto, "", sb.rules)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "snapshot %s: %d files, %s new%s (undo with ezbx rewind)\n",
		snap.ID, st.Files, snapshot.Bytes(st.NewBytes), skippedText(snap))
	if _, _, err := sb.store.Prune(sb.keep); err != nil {
		fmt.Fprintf(stdout, "note: removing old automatic snapshots failed: %v\n", err)
	}
	return nil
}

func skippedText(s snapshot.Snapshot) string {
	if len(s.Skipped) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d left out", len(s.Skipped))
}

// runSnapshot takes a snapshot by hand, lists them, or removes one.
func runSnapshot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	boxFlag := fs.String("box", "", "box name")
	note := fs.String("m", "", "a note to keep with the snapshot")
	yes := fs.Bool("yes", false, "with remove, don't ask")
	words, _, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	usage := func() int {
		fmt.Fprintln(stderr, "Usage: ezbx snapshot [-m NOTE] | ezbx snapshot list | ezbx snapshot remove ID [--yes]")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	sb, err := openSnapshots(t)
	if err != nil {
		return report(stderr, err)
	}
	switch {
	case len(words) == 0:
		snap, st, err := sb.store.Take(sb.root, snapshot.Manual, *note, sb.rules)
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "snapshot %s: %d files (%s), %s new. Kept until you remove it.\n",
			snap.ID, st.Files, snapshot.Bytes(snap.Size()), snapshot.Bytes(st.NewBytes))
		printSkipped(stdout, snap)
		return 0
	case len(words) == 1 && words[0] == "list":
		return listSnapshots(sb, stdout, stderr)
	case len(words) == 2 && words[0] == "remove":
		snap, err := sb.store.Get(words[1])
		if err != nil {
			return report(stderr, err)
		}
		if !*yes {
			p := &setup.Prompter{In: bufio.NewReader(stdin), Out: stdout}
			ok, err := p.YesNo(fmt.Sprintf("Remove snapshot %s (%s, %d files)? It can't be undone.", snap.ID, snap.Kind, len(snap.Files)), false)
			if err != nil {
				return report(stderr, err)
			}
			if !ok {
				fmt.Fprintln(stdout, "Nothing was removed.")
				return 0
			}
		}
		if err := sb.store.Remove(snap.ID); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "removed snapshot", snap.ID)
		return 0
	}
	return usage()
}

func printSkipped(w io.Writer, s snapshot.Snapshot) {
	const most = 10
	for i, sk := range s.Skipped {
		if i == most {
			fmt.Fprintf(w, "  ... and %d more left out\n", len(s.Skipped)-most)
			break
		}
		fmt.Fprintf(w, "  left out: %s (%s)\n", sk.Path, sk.Reason)
	}
}

func listSnapshots(sb snapBox, stdout, stderr io.Writer) int {
	all, err := sb.store.List()
	if err != nil {
		return report(stderr, err)
	}
	if len(all) == 0 {
		fmt.Fprintln(stdout, "No snapshots yet. ezbx snapshot takes one, and every ezbx shell takes one first.")
		return 0
	}
	fmt.Fprintf(stdout, "Snapshots of %s (newest last)\n", sb.root)
	fmt.Fprintf(stdout, "  %-18s %-7s %-20s %7s %10s  %s\n", "ID", "KIND", "TAKEN", "FILES", "SIZE", "NOTE")
	for _, s := range all {
		taken := s.Created
		if tm, err := time.Parse(time.RFC3339, s.Created); err == nil {
			taken = tm.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(stdout, "  %-18s %-7s %-20s %7d %10s  %s\n", s.ID, s.Kind, taken, len(s.Files), snapshot.Bytes(s.Size()), s.Note)
	}
	fmt.Fprintf(stdout, "Stored content: %s (each file's content is kept once, compressed). Automatic snapshots kept: %d.\n",
		snapshot.Bytes(sb.store.StoreSize()), sb.keep)
	return 0
}

// sessionRunning reports what runs in the box besides its own idle process,
// so rewind can refuse while an agent might be writing.
func sessionRunning(ctx context.Context, name string) ([]string, error) {
	r := &box.Runner{Docker: dockerIn}
	running, err := r.Running(ctx, box.NamesFor(name).Box)
	if err != nil || !running {
		return nil, err
	}
	out, err := dockerIn(ctx, nil, "top", box.NamesFor(name).Box)
	if err != nil {
		return nil, fmt.Errorf("listing what runs in the box: %w", err)
	}
	return busy(string(out)), nil
}

// busy picks, from docker top's output, the processes that aren't the box's
// own: docker's init and the "sleep infinity" that keeps the box up.
func busy(top string) []string {
	var out []string
	for i, line := range strings.Split(strings.TrimSpace(top), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 8 {
			continue // the heading
		}
		cmd := strings.Join(f[7:], " ")
		if cmd == "sleep infinity" || strings.Contains(cmd, "docker-init") && strings.HasSuffix(cmd, "sleep infinity") {
			continue
		}
		out = append(out, cmd)
	}
	return out
}

// runRewind puts the workspace back as it was in a snapshot.
func runRewind(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rewind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	boxFlag := fs.String("box", "", "box name")
	yes := fs.Bool("yes", false, "don't ask before changing the workspace")
	words, _, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(words) > 1 {
		fmt.Fprintln(stderr, "Usage: ezbx rewind [ID] [--yes]")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	sb, err := openSnapshots(t)
	if err != nil {
		return report(stderr, err)
	}
	var id string
	if len(words) == 1 {
		id = words[0]
	}
	target, err := sb.store.Get(id)
	if err != nil {
		return report(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	procs, err := sessionRunning(ctx, sb.name)
	cancel()
	if err != nil {
		return report(stderr, err)
	}
	if len(procs) > 0 {
		return report(stderr, fmt.Errorf("a session is running in the box (%s), and it could be writing to the workspace. Exit it (or ezbx down), then rewind", strings.Join(procs, "; ")))
	}

	cur, err := snapshot.Current(sb.root, sb.rules)
	if err != nil {
		return report(stderr, err)
	}
	ch := snapshot.Compare(target, cur)
	if ch.Empty() {
		fmt.Fprintf(stdout, "The workspace already matches snapshot %s. Nothing to do.\n", target.ID)
		return 0
	}
	fmt.Fprintf(stdout, "Rewind %s to snapshot %s (%s, %s):\n", sb.root, target.ID, target.Kind, target.Created)
	printChanges(stdout, ch, "put back as it was", "put back", "removed")
	fmt.Fprintln(stdout, "A snapshot of the workspace as it is now is taken first, so this can be undone.")
	if !*yes {
		p := &setup.Prompter{In: bufio.NewReader(stdin), Out: stdout}
		ok, err := p.YesNo("Rewind?", false)
		if err != nil {
			return report(stderr, err)
		}
		if !ok {
			fmt.Fprintln(stdout, "Nothing was changed.")
			return 0
		}
	}
	res, err := sb.store.Rewind(sb.root, target, sb.rules)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "Rewound to %s: %d put back, %d removed. To undo this rewind: ezbx rewind %s\n",
		target.ID, len(res.Changes.Changed)+len(res.Changes.Deleted), len(res.Changes.Added), res.Safety.ID)
	if len(res.Problems) > 0 {
		for _, p := range res.Problems {
			fmt.Fprintln(stderr, "  not done:", p)
		}
		return 1
	}
	if _, _, err := sb.store.Prune(sb.keep); err != nil {
		fmt.Fprintf(stdout, "note: removing old automatic snapshots failed: %v\n", err)
	}
	return 0
}

// gitDir returns the .git folder a path lies in ("app/.git"), or "".
func gitDir(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts[:len(parts)-1] {
		if part == ".git" {
			return strings.Join(parts[:i+1], "/")
		}
	}
	return ""
}

// printChanges lists changes, at most 30 lines per kind. The words say what
// each kind means for the command (empty for review). Files inside a .git
// folder are git's own bookkeeping, so they are summed up in one line per
// project instead of listed.
func printChanges(w io.Writer, c snapshot.Changes, changed, deleted, added string) {
	type counts struct{ changed, deleted, added int }
	repos := map[string]*counts{}
	var order []string
	split := func(list []string, bump func(*counts)) []string {
		var plain []string
		for _, p := range list {
			g := gitDir(p)
			if g == "" {
				plain = append(plain, p)
				continue
			}
			if repos[g] == nil {
				repos[g] = &counts{}
				order = append(order, g)
			}
			bump(repos[g])
		}
		return plain
	}
	ch := split(c.Changed, func(n *counts) { n.changed++ })
	del := split(c.Deleted, func(n *counts) { n.deleted++ })
	add := split(c.Added, func(n *counts) { n.added++ })
	show := func(mark, what, how string, list []string) {
		if len(list) == 0 {
			return
		}
		if how != "" {
			what += ", " + how
		}
		fmt.Fprintf(w, "  %d %s", len(list), what)
		fmt.Fprintln(w, ":")
		for i, p := range list {
			if i == 30 {
				fmt.Fprintf(w, "    ... and %d more\n", len(list)-30)
				break
			}
			fmt.Fprintf(w, "    %s %s\n", mark, p)
		}
	}
	show("~", "changed", changed, ch)
	show("-", "deleted", deleted, del)
	show("+", "new", added, add)
	sort.Strings(order)
	for _, g := range order {
		n := repos[g]
		var parts []string
		for _, x := range []struct {
			n    int
			what string
		}{{n.changed, "changed"}, {n.deleted, "deleted"}, {n.added, "new"}} {
			if x.n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", x.n, x.what))
			}
		}
		fmt.Fprintf(w, "  git history: %s (git's own files: %s)\n", g, strings.Join(parts, ", "))
	}
}

// runReview shows what changed in the workspace since a snapshot.
func runReview(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	boxFlag := fs.String("box", "", "box name")
	diff := fs.Bool("diff", false, "also show the changed lines of text files")
	words, _, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(words) > 1 {
		fmt.Fprintln(stderr, "Usage: ezbx review [ID] [--diff]")
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	sb, err := openSnapshots(t)
	if err != nil {
		return report(stderr, err)
	}
	var id string
	if len(words) == 1 {
		id = words[0]
	}
	target, err := sb.store.Get(id)
	if err != nil {
		return report(stderr, err)
	}
	cur, err := snapshot.Current(sb.root, sb.rules)
	if err != nil {
		return report(stderr, err)
	}
	ch := snapshot.Compare(target, cur)
	fmt.Fprintf(stdout, "Changes in %s since snapshot %s (%s, %s)\n", sb.root, target.ID, target.Kind, target.Created)
	if ch.Empty() {
		fmt.Fprintln(stdout, "  none")
		return 0
	}
	printChanges(stdout, ch, "", "", "")
	if !*diff {
		fmt.Fprintln(stdout, "Line by line: ezbx review --diff. To undo: ezbx rewind", target.ID)
		return 0
	}
	before := map[string]snapshot.Entry{}
	for _, e := range target.Files {
		before[e.Path] = e
	}
	now := map[string]snapshot.Entry{}
	for _, e := range cur.Files {
		now[e.Path] = e
	}
	for _, p := range append(append(append([]string(nil), ch.Changed...), ch.Deleted...), ch.Added...) {
		if gitDir(p) != "" {
			continue // git's own files; summed up above
		}
		fmt.Fprintln(stdout)
		var a, b []byte
		var aLink, bLink string
		if e, ok := before[p]; ok {
			if aLink = e.Link; aLink == "" {
				if a, err = sb.store.Content(e.Hash); err != nil {
					fmt.Fprintf(stdout, "%s: %v\n", p, err)
					continue
				}
			}
		}
		if e, ok := now[p]; ok {
			if bLink = e.Link; bLink == "" {
				if b, err = os.ReadFile(filepath.Join(sb.root, filepath.FromSlash(p))); err != nil {
					fmt.Fprintf(stdout, "%s: %v\n", p, err)
					continue
				}
			}
		}
		switch {
		case aLink != "" || bLink != "":
			fmt.Fprintf(stdout, "%s: link %q -> %q\n", p, aLink, bLink)
		case snapshot.Binary(a) || snapshot.Binary(b):
			fmt.Fprintf(stdout, "%s: binary file, %s -> %s\n", p, snapshot.Bytes(int64(len(a))), snapshot.Bytes(int64(len(b))))
		case len(a) > 1<<20 || len(b) > 1<<20:
			fmt.Fprintf(stdout, "%s: too large to show line by line (%s -> %s)\n", p, snapshot.Bytes(int64(len(a))), snapshot.Bytes(int64(len(b))))
		default:
			fmt.Fprint(stdout, snapshot.Unified(p, a, b))
		}
	}
	return 0
}
