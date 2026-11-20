package gateway

// Tests for the egress proxy: network modes, the allowlist, local-address
// blocking (including names that resolve to local addresses), ports, plain
// HTTP, closing tunnels when the mode tightens, and the connection limit.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// proxyEnv is a proxy in front of one local test site, reachable under the
// made-up public name "public.test". Tests need a local site, so the address
// check lets 127.0.0.1 through, and only 127.0.0.1. Everything else uses the
// real check.
type proxyEnv struct {
	p       *Proxy
	srv     *httptest.Server
	site    *httptest.Server
	port    string
	logs    *syncBuffer
	lookups atomic.Int64
}

func newProxyEnv(t *testing.T, cfg ProxyConfig) *proxyEnv {
	t.Helper()
	env := &proxyEnv{logs: &syncBuffer{}}
	env.site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from "+r.Host+r.URL.Path)
	}))
	t.Cleanup(env.site.Close)
	_, env.port, _ = net.SplitHostPort(strings.TrimPrefix(env.site.URL, "http://"))
	n, _ := strconv.Atoi(env.port)

	g, err := New(Config{Upstream: "http://127.0.0.1:1", Log: env.logs})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ports = append(cfg.Ports, n, 80, 443)
	env.p, err = g.NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loop := netip.MustParseAddr("127.0.0.1")
	env.p.isBlocked = func(a netip.Addr) bool { return a != loop && blockedAddr(a) }
	env.p.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		env.lookups.Add(1)
		switch host {
		case "public.test", "other.test":
			return []netip.Addr{loop}, nil
		case "private.test":
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		case "metadata.test":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		case "mixed.test": // one public answer and one private: refused
			return []netip.Addr{loop, netip.MustParseAddr("192.168.1.1")}, nil
		case "mapped.test":
			return []netip.Addr{netip.MustParseAddr("::ffff:10.0.0.1")}, nil
		}
		return nil, errors.New("no such host")
	}
	env.srv = httptest.NewServer(env.p)
	t.Cleanup(env.srv.Close)
	return env
}

// connect sends CONNECT target through the proxy. It returns the status code
// and, when the tunnel opened, the connection to use.
func (env *proxyEnv) connect(t *testing.T, target string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(env.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		c.Close()
		return resp.StatusCode, nil, nil
	}
	t.Cleanup(func() { c.Close() })
	return resp.StatusCode, c, br
}

// get sends one HTTP request over an open tunnel and returns the body.
func get(t *testing.T, c net.Conn, br *bufio.Reader) string {
	t.Helper()
	io.WriteString(c, "GET /through HTTP/1.1\r\nHost: public.test\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("request through the tunnel: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// lastLog returns the newest proxy log line.
func (env *proxyEnv) lastLog(t *testing.T) entry {
	t.Helper()
	var e entry
	for i := 0; i < 100; i++ {
		lines := strings.Split(strings.TrimSpace(env.logs.String()), "\n")
		if last := lines[len(lines)-1]; last != "" {
			if err := json.Unmarshal([]byte(last), &e); err != nil {
				t.Fatalf("log line is not JSON: %q", last)
			}
			return e
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no log line")
	return e
}

func TestProxyModes(t *testing.T) {
	cases := []struct {
		mode   string
		allow  []string
		want   int
		reason string
	}{
		{ModeOff, nil, http.StatusForbidden, reasonNetworkOff},
		{ModeOff, []string{"public.test"}, http.StatusForbidden, reasonNetworkOff},
		{ModeAllow, nil, http.StatusForbidden, reasonNotAllowed},
		{ModeAllow, []string{"other.test"}, http.StatusForbidden, reasonNotAllowed},
		{ModeAllow, []string{"public.test"}, http.StatusOK, ""},
		{ModeAllow, []string{"*.public.test"}, http.StatusForbidden, reasonNotAllowed}, // a wildcard doesn't cover the name itself
		{ModeOn, nil, http.StatusOK, ""},
	}
	for _, tc := range cases {
		env := newProxyEnv(t, ProxyConfig{Mode: tc.mode, Allow: tc.allow})
		code, c, br := env.connect(t, "public.test:"+env.port)
		if code != tc.want {
			t.Errorf("mode %s, allow %v: status %d, want %d", tc.mode, tc.allow, code, tc.want)
			continue
		}
		if c != nil {
			if body := get(t, c, br); !strings.HasPrefix(body, "hello from public.test/through") {
				t.Errorf("mode %s: body through tunnel = %q", tc.mode, body)
			}
			c.Close()
		}
		e := env.lastLog(t)
		if e.Route != "proxy" || e.Method != "CONNECT" || e.Host != "public.test:"+env.port || e.Reason != tc.reason || e.Blocked != (tc.reason != "") {
			t.Errorf("mode %s: log entry %+v", tc.mode, e)
		}
	}
}

func TestProxyRefusesLocalAddressesInEveryMode(t *testing.T) {
	env := newProxyEnv(t, ProxyConfig{Mode: ModeOn})
	targets := []string{
		"private.test", "metadata.test", "mixed.test", "mapped.test",
		"10.0.0.5", "192.168.65.254", "169.254.169.254", "[::1]", "[::ffff:10.0.0.1]",
		"localhost", "host.docker.internal", "gateway.docker.internal", "HOST.DOCKER.INTERNAL.",
	}
	for _, h := range targets {
		code, c, _ := env.connect(t, h+":"+env.port)
		if c != nil {
			c.Close()
		}
		if code != http.StatusForbidden {
			t.Errorf("CONNECT %s: status %d, want 403", h, code)
			continue
		}
		if e := env.lastLog(t); e.Reason != reasonLocalAddress || !e.Blocked {
			t.Errorf("CONNECT %s: log entry %+v", h, e)
		}
	}

	// A local address is reported as local in every mode, not as "not on the
	// allowlist" or "network off", so the log never suggests approving it.
	for _, mode := range []string{ModeOff, ModeAllow} {
		if _, err := env.p.SetNetwork(mode, nil); err != nil {
			t.Fatal(err)
		}
		env.connect(t, "10.0.0.5:"+env.port)
		if e := env.lastLog(t); e.Reason != reasonLocalAddress {
			t.Errorf("mode %s, CONNECT 10.0.0.5: reason %q, want %q", mode, e.Reason, reasonLocalAddress)
		}
	}

	// The allowlist can't open a local address either.
	if _, err := env.p.SetNetwork(ModeAllow, []string{"private.test"}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := env.connect(t, "private.test:"+env.port); code != http.StatusForbidden {
		t.Errorf("allowlisted private name: status %d, want 403", code)
	}
}

func TestProxyResolvesOnceAndConnectsToTheCheckedAddress(t *testing.T) {
	env := newProxyEnv(t, ProxyConfig{Mode: ModeOn})
	var dialed atomic.Value
	env.p.dialer = &net.Dialer{Timeout: time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		dialed.Store(address)
		return nil
	}}
	code, c, br := env.connect(t, "public.test:"+env.port)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	get(t, c, br)
	if n := env.lookups.Load(); n != 1 {
		t.Errorf("looked up the name %d times, want 1", n)
	}
	if got := dialed.Load(); got != "127.0.0.1:"+env.port {
		t.Errorf("connected to %v, want the checked address 127.0.0.1:%s", got, env.port)
	}
}

func TestProxyPortsAndBadRequests(t *testing.T) {
	env := newProxyEnv(t, ProxyConfig{Mode: ModeOn})
	if code, _, _ := env.connect(t, "public.test:22"); code != http.StatusForbidden {
		t.Errorf("port 22: status %d, want 403", code)
	}
	if e := env.lastLog(t); e.Reason != reasonPort {
		t.Errorf("port 22: log %+v", e)
	}
	if code, _, _ := env.connect(t, "unknown.test:"+env.port); code != http.StatusBadGateway {
		t.Errorf("unknown name: status %d, want 502", code)
	}
	if e := env.lastLog(t); e.Reason != reasonDNS || e.Blocked {
		t.Errorf("unknown name: log %+v", e)
	}
	for _, h := range []string{"bad_host.test:443", "public.test:0", "public.test:99999", "public.test"} {
		if code, _, _ := env.connect(t, h); code != http.StatusBadRequest {
			t.Errorf("CONNECT %s: status %d, want 400", h, code)
		}
	}

	// An ordinary request to the proxy port is not a proxy request.
	resp, err := quick.Get(env.srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-proxy request: status %d, want 400", resp.StatusCode)
	}
}

func TestProxyPlainHTTP(t *testing.T) {
	env := newProxyEnv(t, ProxyConfig{Mode: ModeAllow, Allow: []string{"public.test"}})
	pu, _ := url.Parse(env.srv.URL)
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	resp, err := client.Get("http://public.test:" + env.port + "/page")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "hello from public.test") {
		t.Errorf("allowed plain HTTP: %d %q", resp.StatusCode, body)
	}
	if e := env.lastLog(t); e.Method != "GET" || e.Status != 200 || e.Blocked {
		t.Errorf("allowed plain HTTP: log %+v", e)
	}

	for _, target := range []string{"http://other.test:" + env.port + "/", "http://private.test:" + env.port + "/", "http://10.0.0.1/"} {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", target, resp.StatusCode)
		}
	}
}

func TestProxyTighteningClosesOpenTunnels(t *testing.T) {
	env := newProxyEnv(t, ProxyConfig{Mode: ModeOn})
	_, keep, keepBR := env.connect(t, "public.test:"+env.port)
	_, drop, _ := env.connect(t, "other.test:"+env.port)
	if keep == nil || drop == nil {
		t.Fatal("tunnels did not open")
	}
	closed, err := env.p.SetNetwork(ModeAllow, []string{"public.test"})
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Errorf("closed %d tunnels, want 1", closed)
	}

	// The dropped tunnel ends; the allowed one keeps working.
	_ = drop.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := drop.Read(make([]byte, 1)); err == nil {
		t.Error("tunnel to other.test is still open")
	}
	if body := get(t, keep, keepBR); !strings.HasPrefix(body, "hello from public.test") {
		t.Errorf("tunnel to public.test: %q", body)
	}

	found := false
	for i := 0; i < 100 && !found; i++ {
		found = strings.Contains(env.logs.String(), `"closed":"network_tightened"`)
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Errorf("no log line says the tunnel was closed by the mode change:\n%s", env.logs.String())
	}

	if _, err := env.p.SetNetwork(ModeOff, nil); err != nil {
		t.Fatal(err)
	}
	if st := env.p.Stats(); st.Open != 0 {
		t.Errorf("open tunnels after switching off: %d", st.Open)
	}
}

func TestProxyConnectionLimit(t *testing.T) {
	env := newProxyEnv(t, ProxyConfig{Mode: ModeOn, MaxTunnels: 1})
	if code, c, _ := env.connect(t, "public.test:"+env.port); code != http.StatusOK || c == nil {
		t.Fatalf("first tunnel: %d", code)
	}
	if code, _, _ := env.connect(t, "public.test:"+env.port); code != http.StatusServiceUnavailable {
		t.Errorf("second tunnel: status %d, want 503", code)
	}
	if e := env.lastLog(t); e.Reason != reasonBusy || e.Blocked {
		t.Errorf("second tunnel: log %+v", e)
	}
}

func TestSetNetworkValidates(t *testing.T) {
	env := newProxyEnv(t, ProxyConfig{})
	if mode, _ := env.p.Network(); mode != ModeOff {
		t.Errorf("default mode %q, want off", mode)
	}
	if _, err := env.p.SetNetwork("sometimes", nil); err == nil {
		t.Error("unknown mode accepted")
	}
	if _, err := env.p.SetNetwork(ModeAllow, []string{"10.0.0.1"}); err == nil {
		t.Error("address on the allowlist accepted")
	}
	if mode, _ := env.p.Network(); mode != ModeOff {
		t.Errorf("a refused change still changed the mode to %q", mode)
	}
}
