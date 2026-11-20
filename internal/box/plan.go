// Package box starts and stops one EZBox box: two Docker networks, the
// gateway container and the box container, set up exactly as proven in
// docs/isolation-test.md.
//
// A Plan holds every decision, worked out before anything is changed, so the
// exact Docker flags can be tested without Docker.
package box

import (
	"archive/tar"
	"bytes"
	"fmt"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryan-ven/ezbx/internal/harness"
	"github.com/ryan-ven/ezbx/internal/paths"
)

// Images used until the image builder exists.
const (
	GatewayImage = "ezbx-gateway:dev"
	BoxImage     = "ezbx-box:dev"
)

// Fixed values inside the containers.
const (
	ManagedLabel   = "ezbx.managed=true"
	agentHome      = "/home/agent"
	agentUID       = 1000
	gatewayUser    = "65532:65532"
	modelAddr      = "0.0.0.0:8000"
	proxyAddr      = "0.0.0.0:3128"
	adminAddr      = "127.0.0.1:8099"
	tokenInside    = "/run/ezbx/admin-token"
	logDirInside   = "/var/log/ezbx"
	gatewayAlias   = "gateway"
	proxyURL       = "http://gateway:3128"
	GatewayBaseURL = "http://gateway:8000/v1"
	// The isolated option leaves the internal network's bridge without an
	// address; without it the box could reach services in Docker's VM.
	isolatedOption = "com.docker.network.bridge.gateway_mode_ipv4"
	ipv6Option     = "com.docker.network.enable_ipv6"
)

// Names are a box's Docker resource names.
type Names struct {
	Inside, Outside, Gateway, Box, Home string
}

// NamesFor gives the resource names for a box.
func NamesFor(box string) Names {
	p := "ezbx-" + box + "-"
	return Names{Inside: p + "inside", Outside: p + "outside", Gateway: p + "gateway", Box: p + "box", Home: p + "home"}
}

// Plan is everything ezbx up will do for one box.
type Plan struct {
	Box       string
	Names     Names
	Workspace string // full host path
	StateDir  string // this box's state folder on the host
	LogOn     bool

	Upstream     string // engine address as the gateway sees it
	EngineID     string
	EngineName   string
	UpstreamHost string
	KeyEnv       string

	Network string
	Allow   []string

	Memory string
	CPUs   string // "" means no limit
	PIDs   int

	Harnesses []string
	HomeFiles []harness.File // rendered harness configs, relative to the agent's home
}

// TokenFile is where the admin token is kept on the host.
func (p Plan) TokenFile() string { return filepath.Join(p.StateDir, "admin-token") }

// LogDir is where the gateway log is kept on the host.
func (p Plan) LogDir() string { return filepath.Join(p.StateDir, "logs") }

func labels(box string) []string {
	return []string{"--label", ManagedLabel, "--label", "ezbx.box=" + box}
}

// InsideNetworkArgs creates the network the box lives on: internal, with an
// isolated bridge and no IPv6.
func (p Plan) InsideNetworkArgs() []string {
	return append(append([]string{"network", "create"}, labels(p.Box)...),
		"--internal", "-o", isolatedOption+"=isolated", "-o", ipv6Option+"=false", p.Names.Inside)
}

// OutsideNetworkArgs creates the gateway's way out.
func (p Plan) OutsideNetworkArgs() []string {
	return append(append([]string{"network", "create"}, labels(p.Box)...), p.Names.Outside)
}

// HomeVolumeArgs creates the agent's home volume.
func (p Plan) HomeVolumeArgs() []string {
	return append(append([]string{"volume", "create"}, labels(p.Box)...), p.Names.Home)
}

// GatewayArgs creates the gateway container. No port is published. The admin
// API listens only on the container's own loopback.
func (p Plan) GatewayArgs() []string {
	a := append([]string{"create"}, labels(p.Box)...)
	a = append(a, "--name", p.Names.Gateway, "--network", p.Names.Outside,
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", gatewayUser,
		"--restart", "unless-stopped",
		// Check health every second while starting, so up doesn't wait 30s.
		"--health-start-period", "15s", "--health-start-interval", "1s",
		"-v", p.TokenFile()+":"+tokenInside+":ro")
	if p.LogOn {
		a = append(a, "-v", p.LogDir()+":"+logDirInside)
	}
	if p.KeyEnv != "" {
		// "-e NAME" without a value copies it from ezbx's own environment, so
		// the key never appears in a command line.
		a = append(a, "-e", p.KeyEnv)
	}
	a = append(a, GatewayImage, "gateway", "serve", "--listen", modelAddr,
		"--upstream", p.Upstream, "--engine", p.EngineID)
	if p.UpstreamHost != "" {
		a = append(a, "--upstream-host", p.UpstreamHost)
	}
	if p.KeyEnv != "" {
		a = append(a, "--key-env", p.KeyEnv)
	}
	a = append(a, "--proxy", proxyAddr, "--network", p.Network)
	for _, h := range p.Allow {
		a = append(a, "--allow", h)
	}
	a = append(a, "--admin", adminAddr, "--admin-token-file", tokenInside)
	if p.LogOn {
		a = append(a, "--log", logDirInside+"/gateway.log")
	}
	return a
}

// ConnectInsideArgs puts the gateway on the inside network as "gateway".
func (p Plan) ConnectInsideArgs() []string {
	return []string{"network", "connect", "--alias", gatewayAlias, p.Names.Inside, p.Names.Gateway}
}

// BoxArgs creates the box container: only on the inside network, no
// capabilities, the workspace and its own home, and nothing else.
func (p Plan) BoxArgs() []string {
	a := append([]string{"create"}, labels(p.Box)...)
	a = append(a, "--name", p.Names.Box, "--network", p.Names.Inside,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(p.PIDs), "--memory", p.Memory)
	if p.CPUs != "" {
		a = append(a, "--cpus", p.CPUs)
	}
	a = append(a, "--restart", "unless-stopped", "--init")
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
		a = append(a, "-e", k+"="+proxyURL)
	}
	a = append(a, "-e", "NO_PROXY="+gatewayAlias, "-e", "no_proxy="+gatewayAlias,
		"-v", p.Workspace+":/workspace",
		"-v", p.Names.Home+":"+agentHome,
		BoxImage, "sleep", "infinity")
	return a
}

// HomeCopyArgs copies the harness configs, as a tar stream on standard input,
// into the agent's home. Archive mode keeps the owner from the tar headers.
func (p Plan) HomeCopyArgs() []string {
	return []string{"cp", "-a", "-", p.Names.Box + ":" + agentHome}
}

// HomeTar packs the harness configs, owned by the agent, with the folders
// they need. Folders are private to the agent.
func (p Plan) HomeTar() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	dirs := map[string]bool{}
	files := append([]harness.File(nil), p.HomeFiles...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		clean := path.Clean(f.Path)
		if clean != f.Path || path.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("harness file %q is not a plain path inside the home", f.Path)
		}
		for d := path.Dir(clean); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var dirList []string
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.Strings(dirList) // parents sort before their children
	for _, d := range dirList {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o700, Uid: agentUID, Gid: agentUID, ModTime: now}); err != nil {
			return nil, err
		}
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.Path, Mode: int64(f.Mode & 0o777), Size: int64(len(f.Data)), Uid: agentUID, Gid: agentUID, ModTime: now}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// GatewayUpstream turns an engine address on this computer into the address
// the gateway uses. Loopback becomes Docker Desktop's host alias; any other
// address is kept.
func GatewayUpstream(engineURL string) (string, error) {
	u, err := url.Parse(engineURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("engine address %q is not an http address", engineURL)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
		}
		u.Host = net.JoinHostPort("host.docker.internal", port)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// CheckWorkspace refuses folders that must never be the agent's writable
// folder. ws must be a clean full path.
func CheckWorkspace(ws, home string, own []string, system []string) error {
	if !filepath.IsAbs(ws) {
		return fmt.Errorf("workspace %q must be a full path", ws)
	}
	if filepath.Dir(ws) == ws {
		return fmt.Errorf("workspace %s is the root of a drive; choose a folder inside it", ws)
	}
	if home != "" && paths.Within(home, ws) {
		return fmt.Errorf("workspace %s is your home folder or holds it; choose a folder inside it, such as %s", ws, filepath.Join(home, "ezbx", "workspace"))
	}
	for _, d := range own {
		if d != "" && (paths.Within(ws, d) || paths.Within(d, ws)) {
			return fmt.Errorf("workspace %s overlaps EZBox's own folder %s, which holds the admin token; choose another folder", ws, d)
		}
	}
	for _, d := range system {
		if d != "" && (paths.Within(ws, d) || paths.Within(d, ws)) {
			return fmt.Errorf("workspace %s is a system folder or holds one (%s); choose a folder in your home", ws, d)
		}
	}
	return nil
}

// AllowList merges the configured hosts with each harness's own, without
// repeats, in a stable order.
func AllowList(configured []string, manifests []harness.Manifest) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, m := range manifests {
		for _, d := range m.Network.Domains {
			add(d)
		}
	}
	for _, h := range configured {
		add(h)
	}
	sort.Strings(out)
	return out
}
