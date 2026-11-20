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
	"sync/atomic"
	"time"
)

const defaultMaxBody = 32 << 20 // 32 MiB

// Config holds the gateway's settings.
type Config struct {
	Upstream string    // engine address, for example http://127.0.0.1:8080
	EngineID string    // engine id shown in X-Ezbx-Engine
	Key      string    // key added to engine requests; empty means none
	MaxBody  int64     // largest request body accepted; 0 means 32 MiB
	Log      io.Writer // JSON lines go here; nil turns logging off
}

// target is an engine address and name. It is swapped as one unit so a
// request never sees half of an old target and half of a new one.
type target struct {
	url    *url.URL
	engine string
}

// Gateway implements http.Handler for the model route.
type Gateway struct {
	key       string
	maxBody   int64
	target    atomic.Pointer[target]
	transport *http.Transport
	proxy     *httputil.ReverseProxy
	log       *logger
}

// reqInfo travels with a request through the proxy.
type reqInfo struct {
	t     *target
	id    string
	model string
	start time.Time
	ttfb  time.Duration
}

type ctxKey struct{}

// New creates a gateway that forwards to cfg.Upstream.
func New(cfg Config) (*Gateway, error) {
	g := &Gateway{
		key:     cfg.Key,
		maxBody: cfg.MaxBody,
		log:     newLogger(cfg.Log),
		transport: &http.Transport{
			Proxy:               nil, // never route through proxy settings in the environment
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConns:        16,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true, // pass bodies through untouched
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	if g.maxBody <= 0 {
		g.maxBody = defaultMaxBody
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
	g.target.Store(&target{url: &url.URL{Scheme: u.Scheme, Host: u.Host}, engine: engineID})
	return nil
}

// ServeHTTP routes a request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.URL.Path == "/readyz":
		g.ready(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		g.forward(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "Unknown path. The gateway only serves /v1/...")
	}
}

// forward sends one request to the engine and logs the outcome.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request) {
	info := &reqInfo{t: g.target.Load(), id: newID(), start: time.Now()}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	defer func() {
		g.log.write(entry{
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
		})
	}()

	if err := g.readModel(rec, r, info); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(rec, http.StatusRequestEntityTooLarge, "request_too_large", "The request body is too large.")
		} else {
			writeError(rec, http.StatusBadRequest, "bad_request", "The request body could not be read.")
		}
		return
	}

	ctx := context.WithValue(r.Context(), ctxKey{}, info)
	g.proxy.ServeHTTP(rec, r.WithContext(ctx))
}

// readModel reads the request body (up to the size limit) to learn which
// model was asked for, then puts the body back for forwarding.
func (g *Gateway) readModel(w http.ResponseWriter, r *http.Request, info *reqInfo) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.maxBody))
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) == nil {
		info.model = headerSafe(probe.Model)
	}
	return nil
}

// rewrite builds the request that goes to the engine.
func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	info := pr.In.Context().Value(ctxKey{}).(*reqInfo)
	pr.SetURL(info.t.url)

	// The box's own key is a placeholder and must never reach the engine.
	pr.Out.Header.Del("Authorization")
	if g.key != "" {
		pr.Out.Header.Set("Authorization", "Bearer "+g.key)
	}
	// Rewrite (unlike the older Director) adds no X-Forwarded-For, so the
	// box's address is not passed along.
}

func (g *Gateway) modifyResponse(resp *http.Response) error {
	info := resp.Request.Context().Value(ctxKey{}).(*reqInfo)
	info.ttfb = time.Since(info.start)

	resp.Header.Set("X-Ezbx-Engine", info.t.engine)
	resp.Header.Set("X-Ezbx-Request-Id", info.id)
	if info.model != "" {
		resp.Header.Set("X-Ezbx-Model", info.model)
	}
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
	var resp *http.Response
	if err == nil {
		resp, err = (&http.Client{Transport: g.transport}).Do(req)
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "engine": t.engine, "reason": "engine not reachable"})
		return
	}
	defer resp.Body.Close()

	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list) != nil || len(list.Data) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "engine": t.engine, "reason": "engine reports no models"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "engine": t.engine, "models": len(list.Data)})
}

// ---- small helpers ----

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
