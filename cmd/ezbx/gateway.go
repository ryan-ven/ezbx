package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ryan-ven/ezbx/internal/adapter"
	"github.com/ryan-ven/ezbx/internal/detect"
	"github.com/ryan-ven/ezbx/internal/gateway"
	"github.com/ryan-ven/ezbx/providers"
)

const (
	defaultAdminAddr      = "127.0.0.1:8099"
	defaultAdminTokenFile = "/run/ezbx/admin-token"
	defaultModelAddr      = "127.0.0.1:8000"
)

func runGateway(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gateway serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", defaultModelAddr, "address for the model route")
	upstream := fs.String("upstream", "", "engine address; empty means use the one engine that is detected")
	engine := fs.String("engine", "", "engine id shown in replies (set automatically when detected)")
	keyEnv := fs.String("key-env", "", "NAME of an environment variable holding the engine's key")
	logFile := fs.String("log", "-", "where to write the JSON log; - means standard output")
	proxyAddr := fs.String("proxy", "", "address for the egress proxy; empty means no proxy")
	network := fs.String("network", gateway.ModeOff, "network mode for the proxy: off, allow or on")
	var allow addresses
	fs.Var(&allow, "allow", "host the proxy allows in allow mode, such as registry.npmjs.org or *.github.com (repeatable)")
	adminAddr := fs.String("admin", "", "loopback address for the admin API, such as "+defaultAdminAddr+"; empty means no admin API")
	tokenFile := fs.String("admin-token-file", defaultAdminTokenFile, "file holding the admin token")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// The key comes from the environment, so it never appears in a command line.
	key := ""
	if *keyEnv != "" {
		key = os.Getenv(*keyEnv)
		if key == "" {
			fmt.Fprintf(stderr, "The environment variable %s is empty or not set.\n", *keyEnv)
			return 2
		}
	}

	if *upstream == "" {
		eng, err := onlyEngine()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		*upstream = eng.URL
		if *engine == "" {
			*engine = eng.ID
		}
	}

	logOut := stdout
	if *logFile != "-" {
		f, err := os.OpenFile(*logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, "cannot open the log file:", err)
			return 1
		}
		defer f.Close()
		logOut = f
	}

	gw, err := gateway.New(gateway.Config{Upstream: *upstream, EngineID: *engine, Key: key, Log: logOut})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	var proxy *gateway.Proxy
	if *proxyAddr != "" {
		proxy, err = gw.NewProxy(gateway.ProxyConfig{Mode: *network, Allow: allow})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}

	var admin *gateway.Admin
	if *adminAddr != "" {
		if err := loopbackOnly(*adminAddr); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		token, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(stderr, "cannot read the admin token file:", err)
			return 2
		}
		admin, err = gateway.NewAdmin(gw, proxy, string(token))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}

	// Listen on every address first, so a port that is taken fails at once.
	type server struct {
		name string
		srv  *http.Server
		l    net.Listener
	}
	var servers []server
	add := func(name, addr string, h http.Handler, srv *http.Server) error {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		srv.Handler = h
		servers = append(servers, server{name, srv, l})
		return nil
	}
	closeAll := func() {
		for _, s := range servers {
			_ = s.l.Close()
		}
	}

	// No write timeout on the model route or the proxy: a streamed answer or
	// a download can legitimately take many minutes.
	if err := add("model route", *listen, gw, &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if proxy != nil {
		if err := add("proxy", *proxyAddr, proxy, &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}); err != nil {
			closeAll()
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if admin != nil {
		if err := add("admin API", *adminAddr, admin, &http.Server{ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}); err != nil {
			closeAll()
			fmt.Fprintln(stderr, err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func(s server) {
			if err := s.srv.Serve(s.l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("%s: %w", s.name, err)
			}
		}(s)
	}

	fmt.Fprintf(stderr, "gateway listening on %s, forwarding to %s (engine %q)\n", *listen, *upstream, *engine)
	if proxy != nil {
		mode, list := proxy.Network()
		fmt.Fprintf(stderr, "proxy listening on %s, network %s, allowlist %d entries\n", *proxyAddr, mode, len(list))
	}
	if admin != nil {
		fmt.Fprintf(stderr, "admin API listening on %s\n", *adminAddr)
	}

	code := 0
	select {
	case <-ctx.Done():
	case err := <-errs:
		fmt.Fprintln(stderr, err)
		code = 1
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.srv.Shutdown(shutdown)
	}
	// Shutdown doesn't end proxy tunnels, which are no longer HTTP. Switching
	// the network off closes them.
	if proxy != nil {
		_, _ = proxy.SetNetwork(gateway.ModeOff, nil)
	}
	return code
}

// loopbackOnly refuses an admin address that isn't a loopback IP address.
// Inside the gateway container, anything else would be reachable from the box.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("admin address %q: %w", addr, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("the admin API may only listen on a loopback address such as %s, not %q", defaultAdminAddr, addr)
	}
	return nil
}

// runAdmin calls the admin API and prints the reply. It is meant to run in
// the gateway container: docker exec <gateway> ezbx gateway admin status
func runAdmin(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gateway admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultAdminAddr, "admin API address")
	tokenFile := fs.String("token-file", defaultAdminTokenFile, "file holding the admin token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "Say which call: status, engine, network or allow.")
		return 2
	}

	var method, path string
	var body any
	switch rest[0] {
	case "status":
		method, path = http.MethodGet, "/admin/status"
	case "engine":
		if len(rest) < 2 || len(rest) > 3 {
			fmt.Fprintln(stderr, "Usage: engine UPSTREAM [ID]")
			return 2
		}
		id := ""
		if len(rest) == 3 {
			id = rest[2]
		}
		method, path, body = http.MethodPut, "/admin/engine", map[string]string{"upstream": rest[1], "engine": id}
	case "network":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "Usage: network off|allow|on [HOST]...")
			return 2
		}
		method, path, body = http.MethodPut, "/admin/network", gateway.NetworkState{Mode: rest[1], Allow: append([]string{}, rest[2:]...)}
	case "allow":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "Usage: allow HOST")
			return 2
		}
		method, path, body = http.MethodPost, "/admin/network/allow", map[string]string{"domain": rest[1]}
	default:
		fmt.Fprintf(stderr, "Unknown call %q. Use status, engine, network or allow.\n", rest[0])
		return 2
	}

	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "cannot read the admin token file:", err)
		return 1
	}
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+*addr+path, reader)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "the admin API did not answer:", err)
		return 1
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		out = pretty.Bytes()
	}
	fmt.Fprintln(stdout, string(out))
	if resp.StatusCode/100 != 2 {
		return 1
	}
	return 0
}

// runHealth exits 0 when the gateway answers /healthz. The gateway image has
// no shell or curl, so Docker's healthcheck runs this instead.
func runHealth(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("gateway health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultModelAddr, "model route address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get("http://" + *addr + "/healthz")
	if err != nil {
		fmt.Fprintln(stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(stderr, "unhealthy: status", resp.StatusCode)
		return 1
	}
	return 0
}

// onlyEngine detects engines and returns the single one found. If there are
// none or several, it says so and asks the user to choose with --upstream.
func onlyEngine() (detect.Engine, error) {
	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		return detect.Engine{}, fmt.Errorf("internal error loading adapters: %w", err)
	}
	d := &detect.Detector{Adapters: adapters, Run: runCommand}
	res := d.Detect(context.Background(), nil)

	switch len(res.Engines) {
	case 1:
		return res.Engines[0], nil
	case 0:
		return detect.Engine{}, errors.New("no inference engine found. Start one, or pass --upstream http://127.0.0.1:PORT")
	default:
		return detect.Engine{}, errors.New("more than one engine found. Choose one with --upstream (see: ezbx engine detect)")
	}
}
