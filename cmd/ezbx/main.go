// Command ezbx is the EZBox command line tool.
//
// So far it can do one thing: find inference engines (ezbx engine detect).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/internal/adapter"
	"github.com/ryan-ven/ezbx/internal/detect"
	"github.com/ryan-ven/ezbx/providers"
)

const usage = `ezbx - EZBox command line tool (early development)

Usage:
  ezbx engine detect [--json] [--timeout 500ms] [--url ADDRESS]...

Commands:
  engine detect   Find inference engines running on this computer.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is separate from main so it can be tested. It returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 2 && args[0] == "engine" && args[1] == "detect" {
		return runDetect(args[2:], stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return 0
	}
	return 2
}

// addresses lets --url be given more than once.
type addresses []string

func (a *addresses) String() string     { return strings.Join(*a, ",") }
func (a *addresses) Set(v string) error { *a = append(*a, v); return nil }

func runDetect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("engine detect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	timeout := fs.Duration("timeout", 500*time.Millisecond, "time to wait for each request")
	var urls addresses
	fs.Var(&urls, "url", "an address to check as well (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		fmt.Fprintln(stderr, "internal error loading adapters:", err)
		return 1
	}

	d := &detect.Detector{Adapters: adapters, Run: runCommand, Timeout: *timeout}
	res := d.Detect(context.Background(), urls)

	if *asJSON {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(stdout, string(out))
	} else {
		printTable(stdout, res)
	}
	if len(res.Engines) == 0 {
		return 1
	}
	return 0
}

func printTable(w io.Writer, res detect.Result) {
	if len(res.Engines) == 0 {
		fmt.Fprintln(w, "No inference engine found.")
		fmt.Fprintln(w, "Start one, or point at it with --url http://127.0.0.1:PORT")
	} else {
		fmt.Fprintln(w, "Inference engines found")
		for _, e := range res.Engines {
			fmt.Fprintf(w, "  %-10s %s   %d model(s)   [%s]\n", e.ID, e.URL, len(e.Models), e.Verified)
			for _, m := range e.Models {
				fmt.Fprintf(w, "      %s%s\n", m.ID, describe(m))
			}
			if len(e.AlsoMatched) > 0 {
				fmt.Fprintf(w, "      (also looked like: %s; the most specific match was chosen)\n", strings.Join(e.AlsoMatched, ", "))
			}
		}
	}
	for _, n := range res.Notes {
		fmt.Fprintln(w, "note:", n)
	}
}

func describe(m detect.Model) string {
	var parts []string
	if m.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("%d ctx", m.ContextWindow))
	}
	if len(m.Input) > 0 {
		parts = append(parts, strings.Join(m.Input, "+"))
	}
	if m.Loaded != nil && *m.Loaded {
		parts = append(parts, "loaded")
	}
	if len(parts) == 0 {
		return ""
	}
	return "   (" + strings.Join(parts, ", ") + ")"
}

// runCommand runs an engine's own status command. Only commands that come
// from the adapters built into ezbx are ever run.
func runCommand(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
