// Package gateway is the door between the box and the inference engine.
//
// This first version is the model route: it forwards OpenAI-style requests
// (/v1/...) to one fixed engine, streams replies without buffering, adds the
// engine's key, and stamps each reply so `ezbx doctor` can prove which engine
// answered. See docs/gateway.md.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultMaxBody       = 32 << 20  // 32 MiB, enough for several images
	defaultMaxBuffered   = 256 << 20 // all request bodies held at once
	defaultHeaderTimeout = 30 * time.Minute
	defaultIdleTimeout   = 10 * time.Minute
	bodyReadTimeout      = 2 * time.Minute
)

// Config holds the gateway's settings.
type Config struct {
	Upstream string    // engine address, for example http://127.0.0.1:8080
	EngineID string    // engine id shown in X-Ezbx-Engine
	Key      string    // key added to engine requests; empty means none
	MaxBody  int64     // largest request body accepted; 0 means 32 MiB
	Log      io.Writer // JSON lines go here; nil turns logging off

	// MaxBuffered caps the memory used by all request bodies held at the
	// same time. A request that would go over it gets a 503 "busy" reply.
	// 0 means 256 MiB.
	MaxBuffered int64

	// HeaderTimeout is how long to wait for the engine to start its reply.
	// A reply that is not streamed only starts once the answer is complete,
	// so this is long. 0 means 30 minutes.
	HeaderTimeout time.Duration

	// IdleTimeout ends a reply when the engine sends nothing for this long.
	// 0 means 10 minutes.
	IdleTimeout time.Duration

	// UpstreamHost is the Host header sent to the engine. Empty means
	// automatic: localhost:PORT when the upstream is Docker Desktop's host
	// alias (see SetUpstream), otherwise the upstream's own address.
	UpstreamHost string
}

// hostAliases are the names Docker Desktop gives the host. An engine reached
// this way is really listening on the host's loopback, and engines with DNS
// rebinding protection (Strata, for example) refuse any other Host header.
var hostAliases = map[string]bool{
	"host.docker.internal":    true,
	"gateway.docker.internal": true,
}

// target is an engine address and name. It is swapped as one unit so a
// request never sees half of an old target and half of a new one.
type target struct {
	url    *url.URL
	engine string
	host   string // Host header for the engine; "" means the url's host
}

// Gateway implements http.Handler for the model route.
type Gateway struct {
	key       string
	hostSet   string // Config.UpstreamHost
	maxBody   int64
	idle      time.Duration
	buffered  budget
	target    atomic.Pointer[target]
	transport *http.Transport
	proxy     *httputil.ReverseProxy
	log       *logger
}

// reqInfo travels with a request through the proxy. All of its fields are
// used by the one goroutine that serves the request, except idleHit, which
// an idle timer may set from its own goroutine.
type reqInfo struct {
	t       *target
	id      string
	model   string
	start   time.Time
	ttfb    time.Duration
	cancel  context.CancelFunc
	idleHit atomic.Bool
}

type ctxKey struct{}

// New creates a gateway that forwards to cfg.Upstream.
func New(cfg Config) (*Gateway, error) {
	if cfg.UpstreamHost != "" && !validHostHeader(cfg.UpstreamHost) {
		return nil, errors.New("upstream host must be a host name or address with an optional port, such as localhost:8080")
	}
	g := &Gateway{
		key:     cfg.Key,
		hostSet: cfg.UpstreamHost,
		maxBody: cfg.MaxBody,
		idle:    cfg.IdleTimeout,
		log:     newLogger(cfg.Log),
	}
	if g.maxBody <= 0 {
		g.maxBody = defaultMaxBody
	}
	if g.idle <= 0 {
		g.idle = defaultIdleTimeout
	}
	g.buffered.max = cfg.MaxBuffered
	if g.buffered.max <= 0 {
		g.buffered.max = defaultMaxBuffered
	}
	headerTimeout := cfg.HeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = defaultHeaderTimeout
	}
	g.transport = &http.Transport{
		Proxy:                 nil, // never route through proxy settings in the environment
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // pass bodies through untouched
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
	}
	if err := g.SetUpstream(cfg.Upstream, cfg.EngineID); err != nil {
		return nil, err
	}

	g.proxy = &httputil.ReverseProxy{
		Transport:      g.transport,
		FlushInterval:  -1, // send every chunk at once, which streaming needs
		Rewrite:        g.rewrite,
		ModifyResponse: g.modifyResponse,
		ErrorHandler:   g.errorHandler,
	}
	return g, nil
}

// SetUpstream points the gateway at another engine. Requests already running
// finish on the old one. Only http and https addresses are accepted, so the
// route can never be turned into a relay to other kinds of destination.
func (g *Gateway) SetUpstream(raw, engineID string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("upstream must start with http:// or https://")
	case u.Host == "":
		return errors.New("upstream needs a host")
	case u.User != nil:
		return errors.New("upstream must not contain a username or password")
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "":
		return errors.New("upstream must be just an address, without a path or query")
	}
	if engineID == "" {
		engineID = "unknown"
	}
	t := &target{url: &url.URL{Scheme: u.Scheme, Host: u.Host}, engine: engineID, host: g.hostSet}
	if t.host == "" && hostAliases[strings.ToLower(u.Hostname())] {
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
		}
		t.host = net.JoinHostPort("localhost", port)
	}
	g.target.Store(t)
	return nil
}

// validHostHeader accepts host, host:port or [v6]:port and nothing else, so
// the setting can't smuggle extra header text.
func validHostHeader(h string) bool {
	name, port, err := net.SplitHostPort(h)
	if err != nil {
		name, port = h, ""
	}
	if port != "" {
		for _, c := range port {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == ':') {
			return false
		}
	}
	return true
}

// ServeHTTP routes a request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		// Readiness asks the engine, so it lives on the admin API only
		// (GET /admin/ready). The box gets just this cheap check.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		switch {
		case !safePath(r.URL.Path):
			writeError(w, http.StatusBadRequest, "bad_path", "The path is not allowed. Paths must not contain '.', '..', '//', backslashes or control characters.")
		case r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost:
			w.Header().Set("Allow", "GET, HEAD, POST")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "The gateway only forwards GET, HEAD and POST.")
		case r.Header.Get("Upgrade") != "":
			writeError(w, http.StatusBadRequest, "upgrade_not_supported", "The gateway does not forward protocol upgrades.")
		default:
			g.forward(w, r)
		}
	default:
		writeError(w, http.StatusNotFound, "not_found", "Unknown path. The gateway only serves /v1/...")
	}
}

// safePath refuses paths that an engine might resolve to somewhere outside
// /v1/, such as /v1/../props. The path is already decoded here, so encoded
// forms like %2e%2e and %2f are caught too.
func safePath(p string) bool {
	if strings.Contains(p, "//") || strings.ContainsRune(p, '\\') {
		return false
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// forward sends one request to the engine and logs the outcome.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	info := &reqInfo{t: g.target.Load(), id: newID(), start: time.Now(), cancel: cancel}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	defer func() {
		e := entry{
			Time:   info.start.UTC().Format(time.RFC3339Nano),
			ID:     info.id,
			Route:  "model",
			Method: r.Method,
			Path:   r.URL.Path,
			Engine: info.t.engine,
			Model:  info.model,
			Status: rec.status,
			Ms:     time.Since(info.start).Milliseconds(),
			TTFBMs: info.ttfb.Milliseconds(),
		}
		switch {
		case r.Context().Err() != nil:
			// The client left, perhaps in the middle of a stream.
			e.Status, e.End = 499, "client_gone"
		case info.idleHit.Load():
			e.End = "engine_idle"
		}
		g.log.write(e)
	}()

	held, err := g.readModel(rec, r, info)
	defer g.buffered.give(held)
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.Is(err, errBusy):
			rec.Header().Set("Retry-After", "1")
			writeError(rec, http.StatusServiceUnavailable, "gateway_busy", "The gateway is holding too many large requests. Try again shortly.")
		case errors.As(err, &tooBig):
			writeError(rec, http.StatusRequestEntityTooLarge, "request_too_large", "The request body is too large.")
		default:
			writeError(rec, http.StatusBadRequest, "bad_request", "The request body could not be read.")
		}
		return
	}

	ctx = context.WithValue(ctx, ctxKey{}, info)
	g.proxy.ServeHTTP(rec, r.WithContext(ctx))
}

var errBusy = errors.New("gateway busy")

// readModel reads the request body (up to the size limit) to learn which
// model was asked for, then puts the body back for forwarding. It returns how
// many bytes of the shared memory budget it holds; the caller gives them back.
func (g *Gateway) readModel(w http.ResponseWriter, r *http.Request, info *reqInfo) (int64, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return 0, nil
	}
	if r.ContentLength > g.maxBody {
		return 0, &http.MaxBytesError{Limit: g.maxBody}
	}
	// Reserve the size the client announced, or the limit if it didn't say.
	want := r.ContentLength
	if want < 0 {
		want = g.maxBody
	}
	if !g.buffered.take(want) {
		return 0, errBusy
	}

	// A client must not hold the body open forever. The deadline is cleared
	// again once the body is in, so a long streamed answer is not affected.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	var buf bytes.Buffer
	if r.ContentLength > 0 {
		buf.Grow(int(r.ContentLength))
	}
	_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, g.maxBody))
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		return want, err
	}

	// Give back what was reserved but not needed.
	body := buf.Bytes()
	held := int64(len(body))
	g.buffered.give(want - held)

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = held

	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) == nil {
		info.model = headerSafe(probe.Model)
	}
	return held, nil
}

// rewrite builds the request that goes to the engine.
func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	info := pr.In.Context().Value(ctxKey{}).(*reqInfo)
	pr.SetURL(info.t.url)
	if info.t.host != "" {
		pr.Out.Host = info.t.host
	}

	// The box's own key is a placeholder and must never reach the engine.
	pr.Out.Header.Del("Authorization")
	if g.key != "" {
		pr.Out.Header.Set("Authorization", "Bearer "+g.key)
	}
	// Rewrite (unlike the older Director) adds no X-Forwarded-For and removes
	// any Forwarded or X-Forwarded-* headers the box sent.
}

func (g *Gateway) modifyResponse(resp *http.Response) error {
	info := resp.Request.Context().Value(ctxKey{}).(*reqInfo)
	info.ttfb = time.Since(info.start)

	// Only the gateway may set X-Ezbx-* headers, so drop any the engine sent.
	for name := range resp.Header {
		if strings.HasPrefix(name, "X-Ezbx-") {
			delete(resp.Header, name)
		}
	}
	resp.Header.Set("X-Ezbx-Engine", info.t.engine)
	resp.Header.Set("X-Ezbx-Request-Id", info.id)
	if info.model != "" {
		resp.Header.Set("X-Ezbx-Model", info.model)
	}

	resp.Body = newIdleBody(resp.Body, g.idle, func() {
		info.idleHit.Store(true)
		info.cancel()
	})
	return nil
}

func (g *Gateway) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	// The client went away: nobody is left to read an answer.
	if errors.Is(err, context.Canceled) {
		if rec, ok := w.(*statusRecorder); ok {
			rec.status = 499
		}
		return
	}
	var opErr *net.OpError
	var netErr net.Error
	if !(errors.As(err, &opErr) && opErr.Op == "dial") && errors.As(err, &netErr) && netErr.Timeout() {
		writeError(w, http.StatusGatewayTimeout, "engine_timeout",
			"The engine did not start answering in time. Run `ezbx doctor` to check it.")
		return
	}
	writeError(w, http.StatusBadGateway, "engine_unreachable",
		"The engine did not answer. Make sure it is running, then run `ezbx doctor`.")
}

// ready reports whether the engine answers and lists at least one model.
func (g *Gateway) ready(w http.ResponseWriter, r *http.Request) {
	t := g.target.Load()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url.String()+"/v1/models", nil)
	if err == nil && g.key != "" {
		req.Header.Set("Authorization", "Bearer "+g.key)
	}
	if err == nil && t.host != "" {
		req.Host = t.host
	}
	var resp *http.Response
	if err == nil {
		resp, err = (&http.Client{Transport: g.transport}).Do(req)
	}
	notReady := func(reason string) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "engine": t.engine, "reason": reason})
	}
	if err != nil {
		notReady("engine not reachable")
		return
	}
	defer resp.Body.Close()

	// Say what actually happened: a refusal (401, 403) needs a different fix
	// from an engine with no model loaded.
	if resp.StatusCode != http.StatusOK {
		notReady(fmt.Sprintf("engine answered with status %d", resp.StatusCode))
		return
	}
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list) != nil {
		notReady("engine's model list is not valid JSON")
		return
	}
	if len(list.Data) == 0 {
		notReady("engine reports no models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "engine": t.engine, "models": len(list.Data)})
}

// ---- small helpers ----

// budget is a shared byte allowance. take never waits: if the bytes are not
// available it says no, so a flood of large requests is refused, not queued.
type budget struct {
	mu   sync.Mutex
	used int64
	max  int64
}

func (b *budget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.max {
		return false
	}
	b.used += n
	return true
}

func (b *budget) give(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

// idleBody wraps an engine reply. If no data arrives for the idle time, it
// calls onIdle, which cancels the engine request so the reply ends.
type idleBody struct {
	io.ReadCloser
	timer *time.Timer
	idle  time.Duration
}

func newIdleBody(rc io.ReadCloser, idle time.Duration, onIdle func()) *idleBody {
	return &idleBody{ReadCloser: rc, timer: time.AfterFunc(idle, onIdle), idle: idle}
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.ReadCloser.Close()
}

// statusRecorder remembers the status code so it can be logged. Unwrap lets
// Go's flush support reach the real connection, which streaming relies on.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError uses the OpenAI-style error body so harnesses show it sensibly.
func writeError(w http.ResponseWriter, code int, kind, message string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"message": message, "type": kind, "code": kind}})
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// headerSafe keeps a model name only if it is short, printable ASCII, so it
// can be put in a header and a log line without surprises.
func headerSafe(s string) string {
	if s == "" || len(s) > 200 {
		return ""
	}
	for _, c := range s {
		if c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return s
}
