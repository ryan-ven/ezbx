package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	minTokenLength = 32
	maxAdminBody   = 64 << 10
)

// Admin serves the admin API, which the host CLI uses to control the gateway.
//
// It must listen only on the gateway container's own loopback address. The
// box shares a network with the gateway, so a port there would be reachable
// from the box. The CLI reaches the loopback with `docker exec`, and every
// call also needs the token. See docs/gateway.md, sections 2 and 9.
type Admin struct {
	g       *Gateway
	p       *Proxy // nil when the proxy is not running
	token   []byte
	started time.Time
}

// NewAdmin creates the admin API. The token must be at least 32 characters.
func NewAdmin(g *Gateway, p *Proxy, token string) (*Admin, error) {
	token = strings.TrimSpace(token)
	if len(token) < minTokenLength {
		return nil, errors.New("the admin token must be at least 32 characters")
	}
	return &Admin{g: g, p: p, token: []byte(token), started: time.Now()}, nil
}

// Upstream returns the engine address and id the gateway forwards to.
func (g *Gateway) Upstream() (string, string) {
	t := g.target.Load()
	return t.url.String(), t.engine
}

// ServeHTTP handles one admin call.
func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		a.g.log.write(entry{
			Time: start.UTC().Format(time.RFC3339Nano), ID: newID(), Route: "admin",
			Method: r.Method, Path: r.URL.Path, Status: rec.status, Ms: time.Since(start).Milliseconds(),
		})
	}()

	if !a.authorized(r) {
		rec.Header().Set("WWW-Authenticate", "Bearer")
		writeError(rec, http.StatusUnauthorized, "unauthorized", "The admin token is missing or wrong.")
		return
	}

	switch r.URL.Path {
	case "/admin/status":
		a.only(rec, r, http.MethodGet, a.status)
	case "/admin/ready":
		a.only(rec, r, http.MethodGet, a.g.ready)
	case "/admin/engine":
		a.only(rec, r, http.MethodPut, a.setEngine)
	case "/admin/network":
		a.only(rec, r, http.MethodPut, a.setNetwork)
	case "/admin/network/allow":
		a.only(rec, r, http.MethodPost, a.allowDomain)
	default:
		writeError(rec, http.StatusNotFound, "not_found", "Unknown admin call.")
	}
}

func (a *Admin) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), a.token) == 1
}

func (a *Admin) only(w http.ResponseWriter, r *http.Request, method string, h func(http.ResponseWriter, *http.Request)) {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use "+method+" for this call.")
		return
	}
	h(w, r)
}

// decode reads a small JSON body and refuses unknown fields, so a typo in a
// field name is an error rather than silently ignored.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request body is not valid: "+err.Error())
		return false
	}
	return true
}

// Status is the reply to GET /admin/status.
type Status struct {
	Engine   string        `json:"engine"`
	Upstream string        `json:"upstream"`
	Network  *NetworkState `json:"network,omitempty"` // nil when the proxy is not running
	Proxy    *ProxyStats   `json:"proxy,omitempty"`
	UptimeS  int64         `json:"uptime_s"`
}

// NetworkState is the proxy's mode and allowlist.
type NetworkState struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow"`
}

func (a *Admin) status(w http.ResponseWriter, _ *http.Request) {
	up, engine := a.g.Upstream()
	s := Status{Engine: engine, Upstream: up, UptimeS: int64(time.Since(a.started).Seconds())}
	if a.p != nil {
		mode, allow := a.p.Network()
		st := a.p.Stats()
		s.Network, s.Proxy = &NetworkState{Mode: mode, Allow: allow}, &st
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *Admin) setEngine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Upstream string `json:"upstream"`
		Engine   string `json:"engine"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := a.g.SetUpstream(req.Upstream, headerSafe(req.Engine)); err != nil {
		writeError(w, http.StatusBadRequest, "bad_upstream", err.Error())
		return
	}
	a.status(w, r)
}

func (a *Admin) setNetwork(w http.ResponseWriter, r *http.Request) {
	if a.p == nil {
		writeError(w, http.StatusConflict, "proxy_not_running", "The egress proxy is not running.")
		return
	}
	var req NetworkState
	if !decode(w, r, &req) {
		return
	}
	closed, err := a.p.SetNetwork(req.Mode, req.Allow)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_network", err.Error())
		return
	}
	a.networkReply(w, closed)
}

func (a *Admin) allowDomain(w http.ResponseWriter, r *http.Request) {
	if a.p == nil {
		writeError(w, http.StatusConflict, "proxy_not_running", "The egress proxy is not running.")
		return
	}
	var req struct {
		Domain string `json:"domain"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := a.p.Allow(req.Domain); err != nil {
		writeError(w, http.StatusBadRequest, "bad_network", err.Error())
		return
	}
	a.networkReply(w, 0)
}

func (a *Admin) networkReply(w http.ResponseWriter, closed int) {
	mode, allow := a.p.Network()
	writeJSON(w, http.StatusOK, map[string]any{"mode": mode, "allow": allow, "closed": closed})
}
