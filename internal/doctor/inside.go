package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// InsideConfig says what the inside checks expect. They run in a throwaway
// container attached only to the box's inside network, with the same
// restrictions as the box.
type InsideConfig struct {
	Gateway   string   // host:port of the model route, e.g. gateway:8000
	Proxy     string   // host:port of the egress proxy; empty if none
	Mode      string   // network mode the gateway reports: off, allow or on
	Engine    string   // engine id the gateway should stamp on replies
	OpenPorts []int    // every gateway port the box may reach
	Deny      []string // host:port addresses the box must not reach directly
	Local     []string // addresses the proxy must refuse (host:port)
	Bridges   []string // addresses on which no port at all may be open
	Timeout   time.Duration
}

// Public addresses used to test for a direct route out. Any answer at all
// would mean the box can reach the internet without the gateway.
var publicTargets = []string{"1.1.1.1:443", "8.8.8.8:53", "9.9.9.9:443"}

// Names the box must not be able to resolve: an outside name, and Docker
// Desktop's names for the host.
var outsideNames = []string{"example.com", "host.docker.internal", "gateway.docker.internal"}

// completionWait is how long the tiny completion may take. A model that was
// just loaded, or that shares the GPU with a game, can be slow to answer
// the first time.
var completionWait = 2 * time.Minute

// tinyCompletion asks for one token through the gateway. Taking too long is
// a warning, not a failure: the model list already came back, so the route
// works, and a cold model can simply be slow.
func tinyCompletion(ctx context.Context, base, model string, wait time.Duration) (Status, string, string) {
	client := &http.Client{Timeout: wait, Transport: &http.Transport{Proxy: nil}}
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 1, "messages": []map[string]string{{"role": "user", "content": "Hi"}}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return Warn, fmt.Sprintf("no answer within %s", wait),
				"The engine answered the model list but not a chat in time. A model that was just loaded, or that shares the GPU with another program, can be slow at first. Run ezbx doctor again in a minute."
		}
		return Fail, shortErr(err), "Check the engine with `ezbx engine detect`."
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	switch {
	case resp.StatusCode != http.StatusOK:
		return Fail, fmt.Sprintf("status %d", resp.StatusCode), "The engine refused a chat request. Check its own log."
	case resp.Header.Get("X-Ezbx-Model") != model:
		return Fail, fmt.Sprintf("reply names model %q, expected %q", resp.Header.Get("X-Ezbx-Model"), model), ""
	}
	return Pass, fmt.Sprintf("answered in %d ms", time.Since(start).Milliseconds()), ""
}

// RunInside runs the checks that need to see what the box sees.
func RunInside(ctx context.Context, cfg InsideConfig) Report {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	var r Report
	const g = "From inside the box"
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}

	// 1. The model route works and names the engine.
	model := ""
	resp, err := client.Get("http://" + cfg.Gateway + "/v1/models")
	if err != nil {
		r.add(g, "model list through the gateway", Fail, shortErr(err), "Check that the gateway is running and attached to the inside network as \"gateway\".")
	} else {
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list)
		resp.Body.Close()
		engine := resp.Header.Get("X-Ezbx-Engine")
		switch {
		case resp.StatusCode != http.StatusOK:
			r.add(g, "model list through the gateway", Fail, fmt.Sprintf("status %d", resp.StatusCode), "Run `ezbx gateway admin ready` in the gateway container to see why the engine refused.")
		case len(list.Data) == 0:
			r.add(g, "model list through the gateway", Warn, "the engine lists no models", "Load a model in your engine.")
		case cfg.Engine != "" && engine != cfg.Engine:
			r.add(g, "model list through the gateway", Fail, fmt.Sprintf("answered by engine %q, expected %q", engine, cfg.Engine), "Something other than the gateway answered, or it forwards to the wrong engine.")
		default:
			model = list.Data[0].ID
			r.add(g, "model list through the gateway", Pass, fmt.Sprintf("%d model(s), engine %s", len(list.Data), engine), "")
		}
	}

	// 2. A one-token completion goes through and is stamped with the model.
	if model == "" {
		r.add(g, "a tiny completion", Skip, "no model to ask", "")
	} else {
		s, detail, fix := tinyCompletion(ctx, "http://"+cfg.Gateway, model, completionWait)
		r.add(g, "a tiny completion", s, detail, fix)
	}

	// 3. Only the expected gateway ports are open.
	host, _, _ := net.SplitHostPort(cfg.Gateway)
	open := scanPorts(host, cfg.Timeout)
	want := append([]int(nil), cfg.OpenPorts...)
	sort.Ints(want)
	if equalInts(open, want) {
		r.add(g, "gateway ports", Pass, "only "+joinInts(open)+" open", "")
	} else {
		r.add(g, "gateway ports", Fail, "open: "+joinInts(open)+"; expected: "+joinInts(want),
			"The gateway listens on a port the box shouldn't reach. The admin API must listen on 127.0.0.1 inside the gateway container.")
	}

	// The inside network's bridge address, if it has one, belongs to the
	// machine Docker runs on. Nothing there may answer.
	for _, b := range cfg.Bridges {
		if found := scanPorts(b, cfg.Timeout); len(found) == 0 {
			r.add(g, "nothing answers on "+b, Pass, "every port closed", "")
		} else {
			r.add(g, "nothing answers on "+b, Fail, "open ports on Docker's own machine: "+joinInts(found),
				"Recreate the inside network with -o com.docker.network.bridge.gateway_mode_ipv4=isolated, so its bridge has no address.")
		}
	}

	// 4. No direct way out.
	var reached []string
	for _, t := range append(append([]string{}, publicTargets...), cfg.Deny...) {
		if c, err := net.DialTimeout("tcp", t, cfg.Timeout); err == nil {
			c.Close()
			reached = append(reached, t)
		}
	}
	if len(reached) == 0 {
		r.add(g, "no direct route out", Pass, fmt.Sprintf("%d address(es) tried, none reachable", len(publicTargets)+len(cfg.Deny)), "")
	} else {
		r.add(g, "no direct route out", Fail, "reachable: "+strings.Join(reached, ", "),
			"The inside network must be created with --internal and -o com.docker.network.bridge.gateway_mode_ipv4=isolated, and the box attached to nothing else.")
	}

	var resolved []string
	for _, name := range outsideNames {
		lctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		if addrs, err := net.DefaultResolver.LookupHost(lctx, name); err == nil && len(addrs) > 0 {
			resolved = append(resolved, name)
		}
		cancel()
	}
	if len(resolved) == 0 {
		r.add(g, "no outside name lookups", Pass, "", "")
	} else {
		r.add(g, "no outside name lookups", Fail, "resolved: "+strings.Join(resolved, ", "), "Docker is forwarding DNS to the internal network. Update Docker (Engine 26 or later fixes this).")
	}

	// 5. The proxy applies the network mode and never reaches local addresses.
	if cfg.Proxy == "" {
		r.add(g, "proxy", Skip, "the gateway runs no proxy", "")
		return r
	}
	var opened []string
	for _, t := range cfg.Local {
		if connectVia(cfg.Proxy, t, cfg.Timeout) == "200" {
			opened = append(opened, t)
		}
	}
	if len(opened) == 0 {
		r.add(g, "proxy refuses local addresses", Pass, fmt.Sprintf("%d tried", len(cfg.Local)), "")
	} else {
		r.add(g, "proxy refuses local addresses", Fail, "opened: "+strings.Join(opened, ", "), "The gateway is too old or misconfigured. Run ezbx build, then ezbx down and ezbx up.")
	}
	status := connectVia(cfg.Proxy, "example.com:443", cfg.Timeout*5)
	switch cfg.Mode {
	case "off":
		if status == "403" {
			r.add(g, "network off is enforced", Pass, "example.com refused", "")
		} else {
			r.add(g, "network off is enforced", Fail, "proxy answered "+status+" for example.com", "")
		}
	case "on":
		if status == "200" {
			r.add(g, "network on works", Pass, "example.com reachable through the proxy", "")
		} else {
			r.add(g, "network on works", Warn, "proxy answered "+status+" for example.com", "Check this computer's own internet connection.")
		}
	default:
		r.add(g, "network mode", Pass, cfg.Mode+" (the allowlist is checked by the gateway's own tests)", "")
	}
	return r
}

// connectVia asks the proxy for a CONNECT tunnel and returns the status code
// it answered with, or a short description of what went wrong.
func connectVia(proxy, target string, timeout time.Duration) string {
	c, err := net.DialTimeout("tcp", proxy, timeout)
	if err != nil {
		return "no answer (" + shortErr(err) + ")"
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout * 5))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "no answer (" + shortErr(err) + ")"
	}
	if f := strings.Fields(line); len(f) >= 2 {
		return f[1]
	}
	return "garbage"
}

// scanPorts tries every TCP port on host and returns the open ones.
func scanPorts(host string, timeout time.Duration) []int {
	ports := make(chan int)
	var mu sync.Mutex
	var open []int
	var wg sync.WaitGroup
	for w := 0; w < 256; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range ports {
				if c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(p)), timeout); err == nil {
					c.Close()
					mu.Lock()
					open = append(open, p)
					mu.Unlock()
				}
			}
		}()
	}
	for p := 1; p <= 65535; p++ {
		ports <- p
	}
	close(ports)
	wg.Wait()
	sort.Ints(open)
	return open
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func joinInts(ns []int) string {
	if len(ns) == 0 {
		return "none"
	}
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

// shortErr keeps the end of an error, which says what actually happened.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}
