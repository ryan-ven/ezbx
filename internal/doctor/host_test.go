package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ryan-ven/ezbx/internal/detect"
)

// fakeDocker answers docker commands from canned JSON shaped like the real
// output of docker inspect. Tests change one setting at a time.
type fakeDocker struct {
	version  string
	networks map[string]map[string]any
	ctrs     map[string]map[string]any
	exec     map[string]string // admin call -> reply
	inside   Report
	ran      [][]string
}

func goodDocker() *fakeDocker {
	return &fakeDocker{
		version: "29.9.3",
		networks: map[string]map[string]any{
			"ezbx-t-inside": {"Name": "ezbx-t-inside", "Internal": true, "EnableIPv6": false,
				"Options": map[string]string{isolatedOption: "isolated", "com.docker.network.enable_ipv6": "false"},
				"IPAM":    map[string]any{"Config": []map[string]string{{"Subnet": "172.18.0.0/16"}}}},
			"ezbx-t-outside": {"Name": "ezbx-t-outside", "Internal": false,
				"IPAM": map[string]any{"Config": []map[string]string{{"Subnet": "172.19.0.0/16", "Gateway": "172.19.0.1"}}}},
		},
		ctrs: map[string]map[string]any{
			"ezbx-t-gateway": {
				"Name":  "/ezbx-t-gateway",
				"State": map[string]any{"Running": true, "Health": map[string]string{"Status": "healthy"}},
				"Config": map[string]any{"Image": "ezbx-gateway:dev", "User": "65532:65532",
					"Cmd": []string{"gateway", "serve", "--listen", "0.0.0.0:8000", "--upstream", "http://host.docker.internal:53000", "--proxy", "0.0.0.0:3128", "--network", "off", "--admin", "127.0.0.1:8099"}},
				"HostConfig": map[string]any{"ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges"}, "PortBindings": map[string]any{}},
				"NetworkSettings": map[string]any{"Networks": map[string]any{
					"ezbx-t-inside":  map[string]any{"Aliases": []string{"gateway"}, "IPAddress": "172.18.0.2"},
					"ezbx-t-outside": map[string]any{"IPAddress": "172.19.0.2"},
				}},
			},
			"ezbx-t-box": {
				"Name":       "/ezbx-t-box",
				"State":      map[string]any{"Running": true},
				"Config":     map[string]any{"Image": "ezbx-box:dev", "User": "agent"},
				"HostConfig": map[string]any{"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges"}},
				"Mounts":     []map[string]any{{"Source": "/host/work", "Destination": "/workspace", "RW": true}},
				"NetworkSettings": map[string]any{"Networks": map[string]any{
					"ezbx-t-inside": map[string]any{"IPAddress": "172.18.0.3"},
				}},
			},
		},
		exec: map[string]string{
			"status": `{"engine":"strata","upstream":"http://host.docker.internal:53000","network":{"mode":"off","allow":[]}}`,
			"ready":  `{"ready":true,"engine":"strata","models":1}`,
		},
		inside: Report{Checks: []Check{{Group: "From inside the box", Name: "model list through the gateway", Status: Pass}}},
	}
}

func (f *fakeDocker) run(_ context.Context, args ...string) ([]byte, error) {
	f.ran = append(f.ran, args)
	switch {
	case args[0] == "version":
		if f.version == "" {
			return nil, errors.New("Cannot connect to the Docker daemon")
		}
		return []byte(f.version + "\n"), nil
	case len(args) == 3 && args[1] == "inspect":
		var v map[string]any
		if args[0] == "network" {
			v = f.networks[args[2]]
		} else {
			v = f.ctrs[args[2]]
		}
		if v == nil {
			return nil, errors.New("No such object: " + args[2])
		}
		b, _ := json.Marshal([]any{v})
		return b, nil
	case args[0] == "exec":
		if out, ok := f.exec[args[len(args)-1]]; ok {
			return []byte(out), nil
		}
		return nil, errors.New("exec failed")
	case args[0] == "run":
		b, _ := json.Marshal(f.inside)
		return b, nil
	}
	return nil, errors.New("unexpected docker call: " + strings.Join(args, " "))
}

func runWith(f *fakeDocker) Report {
	return runEngines(f, []detect.Engine{{ID: "strata", Name: "Strata", URL: "http://127.0.0.1:53000", Models: []detect.Model{{ID: "m"}}}}, nil, nil)
}

// runEngines runs the doctor with engines found on the usual ports, plus
// engines found only when their address is given as an extra address. The
// extra addresses the doctor asked about are written to asked.
func runEngines(f *fakeDocker, usual []detect.Engine, onlyAt map[string]detect.Engine, asked *[]string) Report {
	return Run(context.Background(), HostConfig{
		Box:    "t",
		Docker: f.run,
		Detect: func(_ context.Context, extra []string) detect.Result {
			if asked != nil {
				*asked = append(*asked, extra...)
			}
			res := detect.Result{Engines: append([]detect.Engine(nil), usual...)}
			for _, a := range extra {
				if e, ok := onlyAt[a]; ok {
					res.Engines = append(res.Engines, e)
				}
			}
			return res
		},
		LocalAddrs: func() []string { return []string{"192.0.2.10"} },
	})
}

// An engine on an unusual port is found through the gateway's upstream.
func TestEngineFoundThroughGatewayUpstream(t *testing.T) {
	f := goodDocker()
	gw := f.ctrs["ezbx-t-gateway"]["Config"].(map[string]any)
	gw["Cmd"] = []string{"gateway", "serve", "--listen", "0.0.0.0:8000", "--upstream", "http://host.docker.internal:18080", "--proxy", "0.0.0.0:3128", "--network", "off", "--admin", "127.0.0.1:8099"}
	f.exec["status"] = `{"engine":"openai","upstream":"http://host.docker.internal:18080","network":{"mode":"off","allow":[]}}`
	fake := detect.Engine{ID: "openai", Name: "OpenAI-compatible server", URL: "http://127.0.0.1:18080", Models: []detect.Model{{ID: "m"}}}
	var asked []string
	r := runEngines(f, nil, map[string]detect.Engine{"http://127.0.0.1:18080": fake}, &asked)
	if strings.Join(asked, ",") != "http://127.0.0.1:18080" {
		t.Errorf("asked about %v", asked)
	}
	for _, name := range []string{"an engine is running", "forwards to the running engine"} {
		if c := find(t, r, name); c.Status != Pass {
			t.Errorf("%s: %s %s", name, c.Status, c.Detail)
		}
	}

	// Without a gateway there is nothing extra to ask about.
	f = goodDocker()
	delete(f.ctrs, "ezbx-t-gateway")
	asked = nil
	runEngines(f, nil, nil, &asked)
	if len(asked) != 0 {
		t.Errorf("asked about %v with no gateway", asked)
	}
}

func TestHostSide(t *testing.T) {
	cases := map[string]string{
		"http://host.docker.internal:18080":        "http://127.0.0.1:18080",
		"http://HOST.docker.internal:18080/v1?x=1": "http://127.0.0.1:18080",
		"http://host.docker.internal":              "http://127.0.0.1:80",
		"http://192.0.2.10:18080":                  "http://192.0.2.10:18080",
		"":                                         "",
		"host.docker.internal:18080":               "",
		"ftp://host.docker.internal:21":            "",
	}
	for in, want := range cases {
		if got := hostSide(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func find(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, r.Checks)
	return Check{}
}

func TestAGoodSetupPasses(t *testing.T) {
	f := goodDocker()
	r := runWith(f)
	for _, c := range r.Checks {
		if c.Status != Pass {
			t.Errorf("%s / %s: %s %s", c.Group, c.Name, c.Status, c.Detail)
		}
	}
	if r.Failed() {
		t.Error("report says failed")
	}

	// The inside checks run locked down, on the inside network only, from
	// the gateway's own image.
	var run []string
	for _, a := range f.ran {
		if a[0] == "run" {
			run = a
		}
	}
	joined := strings.Join(run, " ")
	for _, want := range []string{"--network ezbx-t-inside", "--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--user 65532:65532", "--entrypoint /ezbx ezbx-gateway:dev doctor inside"} {
		if !strings.Contains(joined, want) {
			t.Errorf("check container is missing %q: %s", want, joined)
		}
	}
	var ic InsideConfig
	if err := json.Unmarshal([]byte(run[len(run)-1]), &ic); err != nil {
		t.Fatal(err)
	}
	if ic.Gateway != "gateway:8000" || ic.Proxy != "gateway:3128" || ic.Mode != "off" || ic.Engine != "strata" || joinInts(ic.OpenPorts) != "8000,3128" {
		t.Errorf("inside config: %+v", ic)
	}
	for _, want := range []string{"172.19.0.2:8000", "172.19.0.1:80", "192.0.2.10:445"} {
		if !contains(ic.Deny, want) {
			t.Errorf("deny list is missing %s: %v", want, ic.Deny)
		}
	}
	for _, want := range []string{"127.0.0.1:8099", "host.docker.internal:443", "169.254.169.254:80", "192.0.2.10:443"} {
		if !contains(ic.Local, want) {
			t.Errorf("proxy refusal list is missing %s: %v", want, ic.Local)
		}
	}
	if len(ic.Bridges) != 0 {
		t.Errorf("an isolated network has no bridge to scan: %v", ic.Bridges)
	}
}

// Each change below makes the setup less safe or broken. The doctor must
// fail the named check.
func TestEachProblemIsCaught(t *testing.T) {
	gwHost := func(f *fakeDocker) map[string]any { return f.ctrs["ezbx-t-gateway"]["HostConfig"].(map[string]any) }
	boxHost := func(f *fakeDocker) map[string]any { return f.ctrs["ezbx-t-box"]["HostConfig"].(map[string]any) }
	cases := map[string]struct {
		change func(f *fakeDocker)
		check  string
	}{
		"old Docker":                {func(f *fakeDocker) { f.version = "27.5.1" }, "Docker engine version"},
		"inside not internal":       {func(f *fakeDocker) { f.networks["ezbx-t-inside"]["Internal"] = false }, "ezbx-t-inside"},
		"inside bridge has address": {func(f *fakeDocker) { f.networks["ezbx-t-inside"]["Options"] = map[string]string{} }, "ezbx-t-inside"},
		"inside IPv6 on":            {func(f *fakeDocker) { f.networks["ezbx-t-inside"]["EnableIPv6"] = true }, "ezbx-t-inside"},
		"outside internal":          {func(f *fakeDocker) { f.networks["ezbx-t-outside"]["Internal"] = true }, "ezbx-t-outside"},
		"gateway stopped":           {func(f *fakeDocker) { f.ctrs["ezbx-t-gateway"]["State"] = map[string]any{"Running": false} }, "running"},
		"gateway writable":          {func(f *fakeDocker) { gwHost(f)["ReadonlyRootfs"] = false }, "locked down"},
		"gateway keeps caps":        {func(f *fakeDocker) { gwHost(f)["CapDrop"] = []string{} }, "locked down"},
		"gateway adds a cap":        {func(f *fakeDocker) { gwHost(f)["CapAdd"] = []string{"NET_ADMIN"} }, "locked down"},
		"gateway privileged":        {func(f *fakeDocker) { gwHost(f)["Privileged"] = true }, "locked down"},
		"gateway new privileges":    {func(f *fakeDocker) { gwHost(f)["SecurityOpt"] = []string{} }, "locked down"},
		"gateway as root":           {func(f *fakeDocker) { f.ctrs["ezbx-t-gateway"]["Config"].(map[string]any)["User"] = "" }, "locked down"},
		"gateway as uid 0":          {func(f *fakeDocker) { f.ctrs["ezbx-t-gateway"]["Config"].(map[string]any)["User"] = "0:0" }, "locked down"},
		"port on every address": {func(f *fakeDocker) {
			gwHost(f)["PortBindings"] = map[string]any{"8000/tcp": []map[string]string{{"HostIp": "", "HostPort": "8000"}}}
		}, "published ports"},
		"port on 0.0.0.0": {func(f *fakeDocker) {
			gwHost(f)["PortBindings"] = map[string]any{"8000/tcp": []map[string]string{{"HostIp": "0.0.0.0", "HostPort": "8000"}}}
		}, "published ports"},
		"admin port published": {func(f *fakeDocker) {
			gwHost(f)["PortBindings"] = map[string]any{"8099/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "8099"}}}
		}, "published ports"},
		"gateway without alias": {func(f *fakeDocker) {
			f.ctrs["ezbx-t-gateway"]["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)["ezbx-t-inside"] = map[string]any{"IPAddress": "172.18.0.2"}
		}, "networks"},
		"gateway on an extra network": {func(f *fakeDocker) {
			f.ctrs["ezbx-t-gateway"]["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)["bridge"] = map[string]any{}
		}, "networks"},
		"admin API down":   {func(f *fakeDocker) { delete(f.exec, "status") }, "admin API"},
		"engine not ready": {func(f *fakeDocker) { f.exec["ready"] = `{"ready":false,"reason":"engine answered with status 403"}` }, "engine ready"},
		"box as root":      {func(f *fakeDocker) { f.ctrs["ezbx-t-box"]["Config"].(map[string]any)["User"] = "root" }, "locked down"},
		"box keeps caps":   {func(f *fakeDocker) { boxHost(f)["CapDrop"] = nil }, "locked down"},
		"box on the bridge": {func(f *fakeDocker) {
			f.ctrs["ezbx-t-box"]["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)["bridge"] = map[string]any{}
		}, "networks"},
		"docker socket in the box": {func(f *fakeDocker) {
			f.ctrs["ezbx-t-box"]["Mounts"] = []map[string]any{{"Source": "/var/run/docker.sock", "Destination": "/var/run/docker.sock", "RW": true}}
		}, "no Docker socket"},
		"inside check fails": {func(f *fakeDocker) {
			f.inside = Report{Checks: []Check{{Group: "From inside the box", Name: "gateway ports", Status: Fail}}}
		}, "gateway ports"},
		"inside checks give no report": {func(f *fakeDocker) { f.inside = Report{} }, "inside checks"},
	}
	for name, tc := range cases {
		f := goodDocker()
		tc.change(f)
		r := runWith(f)
		if !r.Failed() {
			t.Errorf("%s: the report passed", name)
			continue
		}
		failed := false
		for _, c := range r.Checks {
			failed = failed || (c.Name == tc.check && c.Status == Fail)
		}
		if !failed {
			t.Errorf("%s: check %q did not fail", name, tc.check)
		}
	}
}

func TestAPlainInternalNetworkGetsItsBridgeScanned(t *testing.T) {
	f := goodDocker()
	f.networks["ezbx-t-inside"]["Options"] = map[string]string{}
	f.networks["ezbx-t-inside"]["IPAM"] = map[string]any{"Config": []map[string]string{{"Subnet": "172.18.0.0/16", "Gateway": "172.18.0.1"}}}
	runWith(f)
	run := f.ran[len(f.ran)-1]
	var ic InsideConfig
	_ = json.Unmarshal([]byte(run[len(run)-1]), &ic)
	if len(ic.Bridges) != 1 || ic.Bridges[0] != "172.18.0.1" {
		t.Errorf("bridge address should be scanned from inside: %v", ic.Bridges)
	}
}

func TestDockerNotRunningStopsEarly(t *testing.T) {
	f := goodDocker()
	f.version = ""
	r := runWith(f)
	if len(r.Checks) != 1 || r.Checks[0].Status != Fail || !strings.Contains(r.Checks[0].Fix, "Start Docker") {
		t.Errorf("report: %+v", r.Checks)
	}
}

func TestMissingPiecesAreReported(t *testing.T) {
	f := goodDocker()
	delete(f.ctrs, "ezbx-t-box")
	if c := find(t, runWith(f), "ezbx-t-box"); c.Status != Skip {
		t.Errorf("no box yet should be a skip: %+v", c)
	}
	f = goodDocker()
	delete(f.ctrs, "ezbx-t-gateway")
	r := runWith(f)
	if c := find(t, r, "ezbx-t-gateway"); c.Status != Fail {
		t.Errorf("missing gateway: %+v", c)
	}
	if c := find(t, r, "inside checks"); c.Status != Skip {
		t.Errorf("inside checks without a gateway: %+v", c)
	}
}

func TestUpstreamOnAnotherPortWarns(t *testing.T) {
	f := goodDocker()
	f.exec["status"] = strings.Replace(f.exec["status"], "53000", "11434", 1)
	if c := find(t, runWith(f), "forwards to the running engine"); c.Status != Warn {
		t.Errorf("gateway pointed at no running engine: %+v", c)
	}
}

func TestCmdFlags(t *testing.T) {
	got := cmdFlags([]string{"gateway", "serve", "--listen", "0.0.0.0:8000", "--proxy=0.0.0.0:3128", "--verbose", "--admin", "127.0.0.1:8099"})
	if got["--listen"] != "0.0.0.0:8000" || got["--proxy"] != "0.0.0.0:3128" || got["--verbose"] != "true" || got["--admin"] != "127.0.0.1:8099" {
		t.Errorf("flags: %v", got)
	}
}
