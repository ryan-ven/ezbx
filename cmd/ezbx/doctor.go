package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/internal/adapter"
	"github.com/ryan-ven/ezbx/internal/config"
	"github.com/ryan-ven/ezbx/internal/detect"
	"github.com/ryan-ven/ezbx/internal/doctor"
	"github.com/ryan-ven/ezbx/providers"
)

// runDoctor checks a box from the host and from inside its network.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "inside" {
		return runDoctorInside(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	box := fs.String("box", "main", "box name; its containers are ezbx-<box>-gateway and so on")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !config.BoxName(*box) {
		fmt.Fprintln(stderr, "A box name is short, lowercase letters, digits and dashes.")
		return 2
	}

	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		fmt.Fprintln(stderr, "internal error loading adapters:", err)
		return 1
	}
	cfg := doctor.HostConfig{
		Box:    *box,
		Docker: dockerCLI,
		Detect: func(ctx context.Context) detect.Result {
			return (&detect.Detector{Adapters: adapters, Run: runCommand}).Detect(ctx, nil)
		},
		LocalAddrs: localAddrs,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rep := doctor.Run(ctx, cfg)
	if *asJSON {
		rep.PrintJSON(stdout)
	} else {
		fmt.Fprintf(stdout, "EZBox doctor, box %q\n", *box)
		rep.Print(stdout)
	}
	if rep.Failed() {
		return 1
	}
	return 0
}

// runDoctorInside runs in the check container on the inside network and
// prints its report as JSON for the host side to read.
func runDoctorInside(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor inside", flag.ContinueOnError)
	fs.SetOutput(stderr)
	spec := fs.String("config", "", "what to expect, as JSON (written by ezbx doctor)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var cfg doctor.InsideConfig
	dec := json.NewDecoder(strings.NewReader(*spec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		fmt.Fprintln(stderr, "doctor inside: bad --config:", err)
		return 2
	}
	doctor.RunInside(context.Background(), cfg).PrintJSON(stdout)
	return 0
}

// dockerCLI runs the docker command. What it prints on standard error comes
// back in the error, so failures can be explained.
func dockerCLI(ctx context.Context, args ...string) ([]byte, error) {
	return dockerIn(ctx, nil, args...)
}

// localAddrs lists this computer's own addresses (not loopback or
// link-local), so the doctor can check the box can't reach any of them.
// They are only passed to the check container, never written anywhere.
func localAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.To4() == nil {
				continue
			}
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
