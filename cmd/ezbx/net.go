package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/internal/box"
	"github.com/ryan-ven/ezbx/internal/config"
)

const netUsage = `Usage:
  ezbx net                    show the network mode, the allowed hosts and
                              recently blocked hosts
  ezbx net off|allow|on       change the mode now (until the next ezbx up,
                              which uses the mode in ezbx.toml)
  ezbx net allow HOST...      approve hosts for allow mode, now and from
                              then on (saved in the state folder)
  ezbx net remove HOST...     forget approved hosts
Flags: --box NAME, --config FILE`

// runNet shows or changes a box's network rules through its gateway.
func runNet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("net", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file, used to find the box")
	boxFlag := fs.String("box", "", "box name; default the one in the config, else main")
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 0 {
		fmt.Fprintln(stderr, netUsage)
		return 2
	}
	t, err := resolveBox(*cfgFlag, *boxFlag)
	if err != nil {
		return report(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := &box.Runner{Docker: dockerIn, Out: stdout}
	approved := box.ApprovedFileFor(t.dirs.Box(t.name))

	switch {
	case len(words) == 0:
		return report(stderr, showNet(ctx, r, t, stdout))
	case len(words) == 1 && (words[0] == "off" || words[0] == "allow" || words[0] == "on"):
		return report(stderr, setMode(ctx, r, t, words[0], stdout))
	case len(words) >= 2 && words[0] == "allow":
		return report(stderr, allowHosts(ctx, r, t, approved, words[1:], stdout))
	case len(words) >= 2 && words[0] == "remove":
		return report(stderr, removeHosts(ctx, r, t, approved, words[1:], stdout))
	}
	fmt.Fprintln(stderr, netUsage)
	return 2
}

// report prints an error, if any, and gives the exit code.
func report(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, err)
	var u usageError
	if errors.As(err, &u) {
		return 2
	}
	return 1
}

func configMode(t target) string {
	if t.cfg == nil {
		return ""
	}
	_, b := t.cfg.Only()
	return b.Network
}

func showNet(ctx context.Context, r *box.Runner, t target, w io.Writer) error {
	s, err := r.GatewayStatus(ctx, t.name)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Box %q: network %s", t.name, s.Network.Mode)
	if m := configMode(t); m != "" && m != s.Network.Mode {
		fmt.Fprintf(w, " (ezbx.toml says %s, used again at the next ezbx up)", m)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  off:   no internet; allow: only the hosts below; on: the public internet, never local addresses")
	if len(s.Network.Allow) == 0 {
		fmt.Fprintln(w, "Allowed hosts: none")
	} else {
		fmt.Fprintln(w, "Allowed hosts:", strings.Join(s.Network.Allow, ", "))
	}
	if s.Proxy != nil {
		fmt.Fprintf(w, "Since the gateway started: %d allowed, %d blocked, %d failed, %d open now\n", s.Proxy.Allowed, s.Proxy.Blocked, s.Proxy.Failed, s.Proxy.Open)
	}
	data, err := box.TailFile(box.LogFileFor(t.dirs.Box(t.name)), 1<<20)
	if err != nil {
		fmt.Fprintln(w, "Recently blocked: unknown (no gateway log in the state folder)")
		return nil
	}
	blocked := box.RecentBlocked(data)
	allowed := map[string]bool{}
	for _, h := range s.Network.Allow {
		allowed[h] = true
	}
	shown := 0
	for _, b := range blocked {
		if allowed[b.Host] || shown == 10 {
			continue
		}
		if shown == 0 {
			fmt.Fprintln(w, "Recently blocked (approve one with: ezbx net allow HOST):")
		}
		fmt.Fprintf(w, "  %-40s %d time(s), last %s\n", b.Host, b.Count, b.Last)
		shown++
	}
	if shown == 0 {
		fmt.Fprintln(w, "Recently blocked: none")
	}
	return nil
}

func setMode(ctx context.Context, r *box.Runner, t target, mode string, w io.Writer) error {
	s, err := r.GatewayStatus(ctx, t.name)
	if err != nil {
		return err
	}
	got, _, closed, err := r.SetNetwork(ctx, t.name, mode, s.Network.Allow)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Box %q: network %s (was %s).", t.name, got, s.Network.Mode)
	if closed > 0 {
		fmt.Fprintf(w, " Closed %d open connection(s) the new mode doesn't allow.", closed)
	}
	fmt.Fprintln(w)
	if m := configMode(t); m != "" && m != got {
		fmt.Fprintf(w, "This lasts until the next ezbx up, which goes back to %s (from ezbx.toml).\n", m)
	}
	if got == "allow" && len(s.Network.Allow) == 0 {
		fmt.Fprintln(w, "No hosts are allowed yet. Approve one with: ezbx net allow HOST")
	}
	return nil
}

func allowHosts(ctx context.Context, r *box.Runner, t target, file string, hosts []string, w io.Writer) error {
	for _, h := range hosts {
		if !config.HostName(strings.ToLower(h)) {
			return usageError(fmt.Sprintf("%q is not a host name such as registry.npmjs.org or *.example.com", h))
		}
	}
	for _, h := range hosts {
		added, err := box.AddHost(file, h)
		if err != nil {
			return err
		}
		if added {
			fmt.Fprintf(w, "saved %s\n", strings.ToLower(h))
		} else {
			fmt.Fprintf(w, "%s was already saved\n", strings.ToLower(h))
		}
	}
	for _, h := range hosts {
		if _, err := r.Admin(ctx, t.name, "allow", strings.ToLower(h)); err != nil {
			var notUp box.ErrNotUp
			if errors.As(err, &notUp) {
				fmt.Fprintf(w, "Box %q is not up; the saved hosts apply from the next ezbx up.\n", t.name)
				return nil
			}
			return err
		}
	}
	s, err := r.GatewayStatus(ctx, t.name)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Allowed now. The network is %s", s.Network.Mode)
	switch s.Network.Mode {
	case "allow":
		fmt.Fprintln(w, ", so these hosts can be reached.")
	case "off":
		fmt.Fprintln(w, ", so nothing can be reached until: ezbx net allow")
	default:
		fmt.Fprintln(w, "; the list is used in allow mode.")
	}
	return nil
}

func removeHosts(ctx context.Context, r *box.Runner, t target, file string, hosts []string, w io.Writer) error {
	drop := map[string]bool{}
	for _, h := range hosts {
		h = strings.ToLower(h)
		drop[h] = true
		removed, err := box.RemoveHost(file, h)
		if err != nil {
			return err
		}
		if removed {
			fmt.Fprintf(w, "forgot %s\n", h)
		} else {
			fmt.Fprintf(w, "%s was not saved\n", h)
		}
	}
	s, err := r.GatewayStatus(ctx, t.name)
	if err != nil {
		var notUp box.ErrNotUp
		if errors.As(err, &notUp) {
			return nil
		}
		return err
	}
	var keep []string
	for _, h := range s.Network.Allow {
		if !drop[h] {
			keep = append(keep, h)
		}
	}
	if len(keep) == len(s.Network.Allow) {
		return nil
	}
	_, _, closed, err := r.SetNetwork(ctx, t.name, s.Network.Mode, keep)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Removed from the running gateway too")
	if closed > 0 {
		fmt.Fprintf(w, ", and closed %d open connection(s) to it", closed)
	}
	fmt.Fprintln(w, ".")
	if t.cfg != nil {
		_, b := t.cfg.Only()
		for _, h := range b.Allow {
			if drop[strings.ToLower(h)] {
				fmt.Fprintf(w, "Note: %s is also in ezbx.toml (allow), so it comes back at the next ezbx up unless you remove it there.\n", h)
			}
		}
	}
	return nil
}
