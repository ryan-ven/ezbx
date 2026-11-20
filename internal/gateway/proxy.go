package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultMaxTunnels = 256
	lookupTimeout     = 5 * time.Second
	dialTimeout       = 10 * time.Second
)

// Reasons a proxy request didn't go through, as written to the log.
const (
	reasonNetworkOff   = "network_off"
	reasonNotAllowed   = "not_on_allowlist"
	reasonLocalAddress = "local_address"
	reasonPort         = "port_not_allowed"
	reasonBadHost      = "bad_host"
	reasonDNS          = "lookup_failed"
	reasonConnect      = "connect_failed"
	reasonBusy         = "proxy_busy"
)

// ProxyConfig holds the egress proxy's settings.
type ProxyConfig struct {
	Mode       string   // off, allow or on; empty means off
	Allow      []string // allowlist used in allow mode
	MaxTunnels int      // open connections at once; 0 means 256
	Ports      []int    // destination ports allowed; empty means 80 and 443
}

// Proxy is the egress proxy: an HTTP forward proxy for the box that applies
// the network mode. HTTPS goes through as a CONNECT tunnel and is never
// decrypted, so rules are by host name. Local addresses are refused in every
// mode. See docs/gateway.md, section 5.
type Proxy struct {
	log        *logger
	ports      map[int]bool
	maxTunnels int

	// mu guards mode, allow and tunnels together, so a mode change and a new
	// tunnel can't pass each other unchecked.
	mu      sync.Mutex
	mode    string
	allow   *allowlist
	tunnels map[*tunnel]struct{}

	allowed, blocked, failed atomic.Int64

	// Swappable in tests, which need local test servers.
	lookup    func(ctx context.Context, host string) ([]netip.Addr, error)
	isBlocked func(netip.Addr) bool
	dialer    *net.Dialer
}

// tunnel is an open connection through the proxy.
type tunnel struct {
	host   string
	conns  []net.Conn
	killed atomic.Bool // closed because the network rules were tightened
}

func (t *tunnel) close() {
	for _, c := range t.conns {
		_ = c.Close()
	}
}

// NewProxy creates the egress proxy. It writes to the gateway's log.
func (g *Gateway) NewProxy(cfg ProxyConfig) (*Proxy, error) {
	p := &Proxy{
		log:        g.log,
		ports:      map[int]bool{},
		maxTunnels: cfg.MaxTunnels,
		tunnels:    map[*tunnel]struct{}{},
		isBlocked:  blockedAddr,
		dialer:     &net.Dialer{Timeout: dialTimeout},
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
	}
	if p.maxTunnels <= 0 {
		p.maxTunnels = defaultMaxTunnels
	}
	ports := cfg.Ports
	if len(ports) == 0 {
		ports = []int{80, 443}
	}
	for _, port := range ports {
		p.ports[port] = true
	}
	mode := cfg.Mode
	if mode == "" {
		mode = ModeOff
	}
	if _, err := p.SetNetwork(mode, cfg.Allow); err != nil {
		return nil, err
	}
	return p, nil
}

// SetNetwork changes the mode and allowlist. It takes effect for new
// connections at once, and open tunnels the new rules don't allow are closed.
// It returns how many were closed.
func (p *Proxy) SetNetwork(mode string, allow []string) (int, error) {
	if !ValidMode(mode) {
		return 0, errors.New("network mode must be off, allow or on")
	}
	list, err := newAllowlist(allow)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode, p.allow = mode, list
	closed := 0
	for t := range p.tunnels {
		if p.ruleLocked(t.host) != "" {
			t.killed.Store(true)
			t.close()
			delete(p.tunnels, t)
			closed++
		}
	}
	return closed, nil
}

// Allow adds one entry to the allowlist and keeps the mode. It reads and
// writes the list under one lock, so two approvals at once can't lose one.
// Adding only ever allows more, so no open tunnel needs closing.
func (p *Proxy) Allow(entry string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	list, err := newAllowlist(append(p.allow.entries(), entry))
	if err != nil {
		return err
	}
	p.allow = list
	return nil
}

// Network returns the current mode and allowlist.
func (p *Proxy) Network() (string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode, p.allow.entries()
}

// ProxyStats is a snapshot of the proxy's counters.
type ProxyStats struct {
	Allowed int64 `json:"allowed"`
	Blocked int64 `json:"blocked"`
	Failed  int64 `json:"failed"`
	Open    int   `json:"open"`
}

// Stats returns the proxy's counters.
func (p *Proxy) Stats() ProxyStats {
	p.mu.Lock()
	open := len(p.tunnels)
	p.mu.Unlock()
	return ProxyStats{Allowed: p.allowed.Load(), Blocked: p.blocked.Load(), Failed: p.failed.Load(), Open: open}
}

// ruleLocked applies the mode and allowlist to a normalized host name. It
// returns a reason when they refuse it, or "". The caller holds p.mu.
func (p *Proxy) ruleLocked(host string) string {
	switch {
	case p.mode == ModeOff:
		return reasonNetworkOff
	case p.mode == ModeAllow && !p.allow.allows(host):
		return reasonNotAllowed
	}
	return ""
}

// check decides whether host:port may be asked for at all, before any lookup.
func (p *Proxy) check(hostport, defaultPort string) (host string, port uint16, reason string) {
	h, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		h, ps = hostport, defaultPort
	}
	host, err = normalizeHost(h)
	if err != nil {
		return "", 0, reasonBadHost
	}
	n, err := strconv.Atoi(ps)
	if err != nil || n < 1 || n > 65535 {
		return host, 0, reasonBadHost
	}
	port = uint16(n)
	if blockedNames[host] {
		return host, port, reasonLocalAddress
	}
	if a, err := netip.ParseAddr(host); err == nil && p.isBlocked(a) {
		return host, port, reasonLocalAddress
	}
	p.mu.Lock()
	reason = p.ruleLocked(host)
	p.mu.Unlock()
	if reason != "" {
		return host, port, reason
	}
	if !p.ports[n] {
		return host, port, reasonPort
	}
	return host, port, ""
}

// dial resolves the host once, refuses it if any address it resolves to is
// local, and connects to the checked address itself. Connecting by address
// rather than by name means a second lookup can't return something else.
func (p *Proxy) dial(ctx context.Context, host string, port uint16) (net.Conn, string) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		addrs, err = p.lookup(lctx, host)
		cancel()
		if err != nil || len(addrs) == 0 {
			return nil, reasonDNS
		}
	}
	for _, a := range addrs {
		if p.isBlocked(a) {
			return nil, reasonLocalAddress
		}
	}
	for _, a := range addrs {
		c, err := p.dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), port).String())
		if err == nil {
			return c, ""
		}
	}
	return nil, reasonConnect
}

// open registers a connection, checking the rules once more under the lock
// in case the mode changed while it was being set up.
func (p *Proxy) open(host string, conns ...net.Conn) (*tunnel, string) {
	t := &tunnel{host: host, conns: conns}
	p.mu.Lock()
	defer p.mu.Unlock()
	if reason := p.ruleLocked(host); reason != "" {
		return nil, reason
	}
	if len(p.tunnels) >= p.maxTunnels {
		return nil, reasonBusy
	}
	p.tunnels[t] = struct{}{}
	return t, ""
}

func (p *Proxy) done(t *tunnel) {
	p.mu.Lock()
	delete(p.tunnels, t)
	p.mu.Unlock()
}

// ServeHTTP handles one proxy request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	e := entry{Time: start.UTC().Format(time.RFC3339Nano), ID: newID(), Route: "proxy", Method: r.Method}
	defer func() {
		e.Ms = time.Since(start).Milliseconds()
		switch {
		case e.Blocked:
			p.blocked.Add(1)
		case e.Reason != "":
			p.failed.Add(1)
		default:
			p.allowed.Add(1)
		}
		p.log.write(e)
	}()

	switch {
	case r.Method == http.MethodConnect:
		p.connect(w, r, &e)
	case r.URL.IsAbs() && r.URL.Scheme == "http":
		p.forwardHTTP(w, r, &e)
	default:
		e.Status, e.Reason = http.StatusBadRequest, reasonBadHost
		writeError(w, http.StatusBadRequest, "not_a_proxy_request",
			"This is the gateway's proxy port. Send CONNECT for HTTPS, or a full http:// address.")
	}
}

// refuse writes the reply for a request that didn't go through.
func (p *Proxy) refuse(w http.ResponseWriter, e *entry, reason string) {
	e.Reason = reason
	switch reason {
	case reasonDNS, reasonConnect:
		e.Status = http.StatusBadGateway
		writeError(w, e.Status, reason, "The proxy could not reach "+e.Host+".")
	case reasonBusy:
		e.Status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1")
		writeError(w, e.Status, reason, "The proxy has too many open connections. Try again shortly.")
	case reasonBadHost:
		e.Status = http.StatusBadRequest
		writeError(w, e.Status, reason, "The proxy request has a missing or invalid host.")
	default:
		e.Status, e.Blocked = http.StatusForbidden, true
		writeError(w, e.Status, "proxy_blocked", blockMessage(reason, e.Host))
	}
}

func blockMessage(reason, host string) string {
	switch reason {
	case reasonNetworkOff:
		return "Blocked: the EZBox network is off. Run `ezbx net allow " + hostOnly(host) + "` or `ezbx net on` on the host to change it."
	case reasonNotAllowed:
		return "Blocked: " + hostOnly(host) + " is not on the allowlist. Run `ezbx net allow " + hostOnly(host) + "` on the host to approve it."
	case reasonLocalAddress:
		return "Blocked: " + hostOnly(host) + " is a local or private address, which the box may never reach."
	case reasonPort:
		return "Blocked: only ports 80 and 443 are allowed through the proxy."
	}
	return "Blocked by the EZBox network rules."
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// connect handles CONNECT host:port by opening a plain two-way tunnel.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, e *entry) {
	e.Host = r.Host
	host, port, reason := p.check(r.Host, "")
	if reason == "" && port == 0 {
		reason = reasonBadHost
	}
	if reason != "" {
		p.refuse(w, e, reason)
		return
	}
	up, reason := p.dial(r.Context(), host, port)
	if reason != "" {
		p.refuse(w, e, reason)
		return
	}

	client, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = up.Close()
		e.Status, e.Reason = http.StatusInternalServerError, reasonConnect
		return
	}
	t, reason := p.open(host, client, up)
	if reason != "" {
		// The reply has to be written by hand now that the connection is ours.
		_ = up.Close()
		status, line := http.StatusForbidden, "403 Forbidden"
		if reason == reasonBusy {
			status, line = http.StatusServiceUnavailable, "503 Service Unavailable\r\nRetry-After: 1"
		}
		_, _ = io.WriteString(client, "HTTP/1.1 "+line+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = client.Close()
		e.Status, e.Reason, e.Blocked = status, reason, reason != reasonBusy
		return
	}
	defer p.done(t)
	e.Status = http.StatusOK
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		t.close()
		return
	}

	// Bytes the client sent right after its CONNECT line may already be in
	// the buffer, so send those first.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			_, _ = up.Write(b)
		}
		_, _ = io.Copy(up, client)
		closeWrite(up)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, up)
		closeWrite(client)
	}()
	wg.Wait()
	t.close()
	if t.killed.Load() {
		e.Closed = "network_tightened"
	}
}

// closeWrite tells the other side no more data is coming, where the
// connection supports it, so each direction can finish on its own.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// forwardHTTP handles a plain http:// request through the proxy.
func (p *Proxy) forwardHTTP(w http.ResponseWriter, r *http.Request, e *entry) {
	e.Host = r.URL.Host
	if r.Header.Get("Upgrade") != "" {
		e.Status, e.Reason = http.StatusBadRequest, reasonBadHost
		writeError(w, e.Status, "upgrade_not_supported", "The proxy does not forward protocol upgrades.")
		return
	}
	host, port, reason := p.check(r.URL.Host, "80")
	if reason != "" {
		p.refuse(w, e, reason)
		return
	}
	up, reason := p.dial(r.Context(), host, port)
	if reason != "" {
		p.refuse(w, e, reason)
		return
	}
	t, reason := p.open(host, up)
	if reason != "" {
		_ = up.Close()
		p.refuse(w, e, reason)
		return
	}
	defer p.done(t)
	defer t.close()

	// The connection is already checked and open, so the transport just uses it.
	used := false
	tr := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used {
				return nil, errors.New("connection already used")
			}
			used = true
			return up, nil
		},
		ResponseHeaderTimeout: 5 * time.Minute,
	}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	rp := &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Proxy-Connection")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			e.Reason = reasonConnect
			writeError(w, http.StatusBadGateway, reasonConnect, "The proxy could not get an answer from "+e.Host+".")
		},
	}
	rp.ServeHTTP(rec, r)
	e.Status = rec.status
}
