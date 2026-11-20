package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/internal/detect"
)

// Docker runs one docker command and returns what it printed. It is a field
// so tests can replace it.
type Docker func(ctx context.Context, args ...string) ([]byte, error)

// HostConfig holds what the host checks need.
type HostConfig struct {
	Box        string // box name; containers are ezbx-<box>-gateway and so on
	Docker     Docker
	Detect     func(ctx context.Context) detect.Result // finds engines on this computer
	LocalAddrs func() []string                         // this computer's own non-loopback addresses
}

// Docker resource names for a box.
func names(box string) (inside, outside, gateway, boxName string) {
	p := "ezbx-" + box + "-"
	return p + "inside", p + "outside", p + "gateway", p + "box"
}

// The option that leaves an internal network's bridge without an address.
// Without it, the box can reach services in Docker Desktop's VM.
const isolatedOption = "com.docker.network.bridge.gateway_mode_ipv4"

type networkInfo struct {
	Name       string            `json:"Name"`
	Internal   bool              `json:"Internal"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Options    map[string]string `json:"Options"`
	Labels     map[string]string `json:"Labels"`
	IPAM       struct {
		Config []struct {
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

type containerInfo struct {
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
		Health  *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		User   string            `json:"User"`
		Cmd    []string          `json:"Cmd"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
		Privileged     bool     `json:"Privileged"`
		CapDrop        []string `json:"CapDrop"`
		CapAdd         []string `json:"CapAdd"`
		SecurityOpt    []string `json:"SecurityOpt"`
		PortBindings   map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			Aliases   []string `json:"Aliases"`
			DNSNames  []string `json:"DNSNames"`
			IPAddress string   `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (cfg HostConfig) inspect(ctx context.Context, kind, name string, v any) error {
	out, err := cfg.Docker(ctx, kind, "inspect", name)
	if err != nil {
		return err
	}
	var list []json.RawMessage
	if err := json.Unmarshal(out, &list); err != nil || len(list) != 1 {
		return fmt.Errorf("unexpected answer from docker %s inspect", kind)
	}
	return json.Unmarshal(list[0], v)
}

// Run does every check: on the host, then from inside the box.
func Run(ctx context.Context, cfg HostConfig) Report {
	var r Report
	insideNet, outsideNet, gwName, boxName := names(cfg.Box)

	// Docker itself.
	const gd = "Docker"
	out, err := cfg.Docker(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		r.add(gd, "Docker engine answers", Fail, shortErr(err), "Start Docker Desktop, then run ezbx doctor again.")
		return r
	}
	version := strings.TrimSpace(string(out))
	major, _ := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	if major >= 28 {
		r.add(gd, "Docker engine version", Pass, version, "")
	} else {
		r.add(gd, "Docker engine version", Fail, version, "EZBox needs Docker Engine 28 or later, for the isolated network option. Update Docker Desktop.")
	}

	// The engine on this computer.
	const ge = "Engine"
	var engines []detect.Engine
	if cfg.Detect != nil {
		res := cfg.Detect(ctx)
		engines = res.Engines
		if len(engines) == 0 {
			r.add(ge, "an engine is running", Warn, "none found", "Start your inference engine. The box works without one, but has no model.")
		} else {
			var found []string
			for _, e := range engines {
				found = append(found, fmt.Sprintf("%s at %s (%d model(s))", e.Name, e.URL, len(e.Models)))
			}
			r.add(ge, "an engine is running", Pass, strings.Join(found, "; "), "")
		}
	}

	// Networks.
	const gn = "Networks"
	var inside, outside networkInfo
	insideOK := cfg.inspect(ctx, "network", insideNet, &inside) == nil
	if !insideOK {
		r.add(gn, insideNet, Fail, "missing", "Create it: docker network create --internal -o "+isolatedOption+"=isolated -o com.docker.network.enable_ipv6=false --label ezbx.managed=true "+insideNet)
	} else {
		var problems []string
		if !inside.Internal {
			problems = append(problems, "not internal, so it has a route out")
		}
		if inside.Options[isolatedOption] != "isolated" {
			problems = append(problems, "its bridge has an address, so the box can reach services on Docker's own machine")
		}
		if inside.EnableIPv6 || inside.Options["com.docker.network.enable_ipv6"] == "true" {
			problems = append(problems, "IPv6 is on")
		}
		if len(problems) == 0 {
			r.add(gn, insideNet, Pass, "internal, isolated bridge, IPv6 off", "")
		} else {
			r.add(gn, insideNet, Fail, strings.Join(problems, "; "),
				"Recreate it: docker network create --internal -o "+isolatedOption+"=isolated -o com.docker.network.enable_ipv6=false --label ezbx.managed=true "+insideNet)
		}
	}
	if err := cfg.inspect(ctx, "network", outsideNet, &outside); err != nil {
		r.add(gn, outsideNet, Fail, "missing", "Create it: docker network create --label ezbx.managed=true "+outsideNet)
	} else if outside.Internal {
		r.add(gn, outsideNet, Fail, "internal, so the gateway can't reach the engine or the internet", "Recreate it without --internal.")
	} else {
		r.add(gn, outsideNet, Pass, "", "")
	}

	// The gateway container.
	const gg = "Gateway"
	var gw containerInfo
	if err := cfg.inspect(ctx, "container", gwName, &gw); err != nil {
		r.add(gg, gwName, Fail, "missing", "Start the gateway container as shown in docs/isolation-test.md.")
		r.add("From inside the box", "inside checks", Skip, "the gateway is missing", "")
		return r
	}
	args := cmdFlags(gw.Config.Cmd)
	switch {
	case !gw.State.Running:
		r.add(gg, "running", Fail, "stopped", "Start it: docker start "+gwName)
	case gw.State.Health == nil:
		r.add(gg, "running", Warn, "running, with no healthcheck", "Rebuild the gateway image; it has a healthcheck built in.")
	case gw.State.Health.Status != "healthy":
		r.add(gg, "running", Warn, "health is "+gw.State.Health.Status, "Wait a few seconds; if it stays unhealthy, see docker logs "+gwName+".")
	default:
		r.add(gg, "running", Pass, "healthy", "")
	}
	r.add(gg, "locked down", lockStatus(gw, true), lockDetail(gw, true), "Recreate it with --read-only --cap-drop ALL --security-opt no-new-privileges --user 65532:65532.")

	nets := sortedKeys(gw.NetworkSettings.Networks)
	if len(nets) == 2 && nets[0] == insideNet && nets[1] == outsideNet {
		in := gw.NetworkSettings.Networks[insideNet]
		if contains(in.Aliases, "gateway") || contains(in.DNSNames, "gateway") {
			r.add(gg, "networks", Pass, "inside (as \"gateway\") and outside", "")
		} else {
			r.add(gg, "networks", Fail, "not reachable as \"gateway\" on the inside network", "docker network disconnect "+insideNet+" "+gwName+", then docker network connect --alias gateway "+insideNet+" "+gwName+".")
		}
	} else {
		r.add(gg, "networks", Fail, "attached to: "+joinOr(nets, "none"), "The gateway must be on exactly "+insideNet+" and "+outsideNet+".")
	}

	_, adminPort, _ := net.SplitHostPort(args["--admin"])
	var exposed []string
	for port, binds := range gw.HostConfig.PortBindings {
		for _, b := range binds {
			ip := net.ParseIP(b.HostIP)
			if adminPort != "" && strings.HasPrefix(port, adminPort+"/") {
				exposed = append(exposed, port+" (the admin API, which is never published)")
			} else if ip == nil || !ip.IsLoopback() {
				where := b.HostIP
				if ip == nil || ip.IsUnspecified() {
					where = "every address, so other computers can reach it"
				}
				exposed = append(exposed, port+" on "+where)
			}
		}
	}
	if len(exposed) == 0 {
		r.add(gg, "published ports", Pass, "none beyond this computer's loopback", "")
	} else {
		sort.Strings(exposed)
		r.add(gg, "published ports", Fail, strings.Join(exposed, "; "), "Publish only as 127.0.0.1:PORT:PORT, and never the admin port.")
	}

	// Through the admin API, the same way the CLI controls the gateway.
	status := struct {
		Engine   string `json:"engine"`
		Upstream string `json:"upstream"`
		Network  *struct {
			Mode string `json:"mode"`
		} `json:"network"`
	}{}
	mode := ""
	if adminPort == "" {
		r.add(gg, "admin API", Warn, "not running", "Start the gateway with --admin 127.0.0.1:8099 and a token file.")
	} else if out, err := cfg.Docker(ctx, "exec", gwName, "/ezbx", "gateway", "admin", "--addr", args["--admin"], "status"); err != nil || json.Unmarshal(out, &status) != nil {
		r.add(gg, "admin API", Fail, "no answer through docker exec", "See docker logs "+gwName+". The token file must be mounted at /run/ezbx/admin-token.")
	} else {
		if status.Network != nil {
			mode = status.Network.Mode
		}
		r.add(gg, "admin API", Pass, fmt.Sprintf("engine %s, network %s", status.Engine, joinOr([]string{mode}, "(no proxy)")), "")

		ready := struct {
			Ready  bool   `json:"ready"`
			Reason string `json:"reason"`
		}{}
		out, _ := cfg.Docker(ctx, "exec", gwName, "/ezbx", "gateway", "admin", "--addr", args["--admin"], "ready")
		if json.Unmarshal(out, &ready) == nil && ready.Ready {
			r.add(gg, "engine ready", Pass, "", "")
		} else {
			r.add(gg, "engine ready", Fail, joinOr([]string{ready.Reason}, "no answer"), "Check that the engine is running and that the gateway's --upstream points at it.")
		}
		s, detail, fix := upstreamMatches(status.Upstream, engines)
		r.add(gg, "forwards to the running engine", s, detail, fix)
	}

	// The box container, if there is one yet.
	const gb = "Box"
	var box containerInfo
	if err := cfg.inspect(ctx, "container", boxName, &box); err != nil {
		r.add(gb, boxName, Skip, "no box container yet", "")
	} else {
		r.add(gb, "locked down", lockStatus(box, false), lockDetail(box, false), "Recreate it with --cap-drop ALL --security-opt no-new-privileges and a non-root user.")
		bnets := sortedKeys(box.NetworkSettings.Networks)
		if len(bnets) == 1 && bnets[0] == insideNet {
			r.add(gb, "networks", Pass, "only the inside network", "")
		} else {
			r.add(gb, "networks", Fail, "attached to: "+joinOr(bnets, "none"), "The box must be attached to "+insideNet+" and nothing else.")
		}
		var sock []string
		for _, m := range box.Mounts {
			if strings.Contains(m.Source, "docker.sock") || strings.Contains(m.Destination, "docker.sock") {
				sock = append(sock, m.Destination)
			}
		}
		if len(sock) == 0 {
			r.add(gb, "no Docker socket", Pass, "", "")
		} else {
			r.add(gb, "no Docker socket", Fail, "mounted at "+strings.Join(sock, ", "), "Never mount the Docker socket into the box: it gives full control of this computer.")
		}
	}

	// From inside, seeing what the box sees.
	if !insideOK {
		r.add("From inside the box", "inside checks", Skip, "the inside network is missing", "")
		return r
	}
	ic := InsideConfig{Gateway: "gateway:" + portOf(args["--listen"], "8000"), Mode: mode, Engine: status.Engine}
	ic.OpenPorts = []int{atoi(portOf(args["--listen"], "8000"))}
	if p := args["--proxy"]; p != "" {
		ic.Proxy = "gateway:" + portOf(p, "3128")
		ic.OpenPorts = append(ic.OpenPorts, atoi(portOf(p, "3128")))
	}
	upPort := "80"
	if u, err := url.Parse(status.Upstream); err == nil && u.Port() != "" {
		upPort = u.Port()
	}
	// Addresses the box must not reach: the gateway's own outside address,
	// the outside network's bridge, Docker Desktop's host address and this
	// computer's own addresses.
	if o, ok := gw.NetworkSettings.Networks[outsideNet]; ok && o.IPAddress != "" {
		ic.Deny = append(ic.Deny, net.JoinHostPort(o.IPAddress, strconv.Itoa(ic.OpenPorts[0])))
		ic.Local = append(ic.Local, net.JoinHostPort(o.IPAddress, "443"))
	}
	for _, c := range outside.IPAM.Config {
		if c.Gateway != "" {
			ic.Deny = append(ic.Deny, net.JoinHostPort(c.Gateway, "80"))
		}
	}
	for _, c := range inside.IPAM.Config {
		if c.Gateway != "" {
			ic.Bridges = append(ic.Bridges, c.Gateway)
		}
	}
	ic.Deny = append(ic.Deny, net.JoinHostPort("192.168.65.254", upPort))
	ic.Local = append(ic.Local, "host.docker.internal:443", "localhost:443", "169.254.169.254:80", net.JoinHostPort("192.168.65.254", "443"))
	if adminPort != "" {
		ic.Local = append(ic.Local, net.JoinHostPort("127.0.0.1", adminPort))
	}
	if cfg.LocalAddrs != nil {
		for _, a := range cfg.LocalAddrs() {
			ic.Deny = append(ic.Deny, net.JoinHostPort(a, "445"))
			ic.Local = append(ic.Local, net.JoinHostPort(a, "443"))
		}
	}
	ic.Timeout = time.Second
	spec, _ := json.Marshal(ic)
	out, err = cfg.Docker(ctx, "run", "--rm", "--label", "ezbx.managed=true", "--network", insideNet,
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532",
		"--entrypoint", "/ezbx", gw.Config.Image, "doctor", "inside", "--config", string(spec))
	var inner Report
	if jerr := json.Unmarshal(out, &inner); jerr != nil || len(inner.Checks) == 0 {
		detail := "the check container gave no report"
		if err != nil {
			detail = shortErr(err)
		}
		r.add("From inside the box", "inside checks", Fail, detail, "Rebuild the gateway image so it has the current ezbx.")
		return r
	}
	r.Checks = append(r.Checks, inner.Checks...)
	return r
}

// lockStatus checks a container's privileges. The gateway also needs a
// read-only root filesystem.
func lockStatus(c containerInfo, readOnly bool) Status {
	if len(lockProblems(c, readOnly)) == 0 {
		return Pass
	}
	return Fail
}

func lockDetail(c containerInfo, readOnly bool) string {
	if bad := lockProblems(c, readOnly); len(bad) > 0 {
		return strings.Join(bad, "; ")
	}
	if readOnly {
		return "no capabilities, no-new-privileges, not root, read-only"
	}
	return "no capabilities, no-new-privileges, not root"
}

func lockProblems(c containerInfo, readOnly bool) []string {
	var bad []string
	if c.HostConfig.Privileged {
		bad = append(bad, "privileged")
	}
	if !containsFold(c.HostConfig.CapDrop, "ALL") {
		bad = append(bad, "capabilities not dropped")
	}
	if len(c.HostConfig.CapAdd) > 0 {
		bad = append(bad, "capabilities added: "+strings.Join(c.HostConfig.CapAdd, ","))
	}
	nnp := false
	for _, o := range c.HostConfig.SecurityOpt {
		nnp = nnp || o == "no-new-privileges" || o == "no-new-privileges:true" || o == "no-new-privileges=true"
	}
	if !nnp {
		bad = append(bad, "no-new-privileges is off")
	}
	if u := strings.SplitN(c.Config.User, ":", 2)[0]; u == "" || u == "0" || u == "root" {
		bad = append(bad, "runs as root")
	}
	if readOnly && !c.HostConfig.ReadonlyRootfs {
		bad = append(bad, "root filesystem is writable")
	}
	return bad
}

// upstreamMatches checks that the gateway forwards to an engine that is
// running, by port, since the gateway reaches it through the host alias.
func upstreamMatches(upstream string, engines []detect.Engine) (Status, string, string) {
	u, err := url.Parse(upstream)
	if err != nil || u.Port() == "" {
		return Warn, "can't tell from " + upstream, ""
	}
	for _, e := range engines {
		if eu, err := url.Parse(e.URL); err == nil && eu.Port() == u.Port() {
			return Pass, e.Name + " on port " + u.Port(), ""
		}
	}
	if len(engines) == 0 {
		return Skip, "no engine found to compare with", ""
	}
	return Warn, "the gateway forwards to port " + u.Port() + ", but no engine was found there",
		"Point the gateway at the running engine: docker exec ezbx-<box>-gateway /ezbx gateway admin engine http://host.docker.internal:PORT ID"
}

// cmdFlags reads --name value pairs from a container's command.
func cmdFlags(cmd []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(cmd); i++ {
		if !strings.HasPrefix(cmd[i], "--") {
			continue
		}
		if k, v, ok := strings.Cut(cmd[i], "="); ok {
			out[k] = v
		} else if i+1 < len(cmd) && !strings.HasPrefix(cmd[i+1], "--") {
			out[cmd[i]] = cmd[i+1]
			i++
		} else {
			out[cmd[i]] = "true"
		}
	}
	return out
}

func portOf(addr, def string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		return p
	}
	return def
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
