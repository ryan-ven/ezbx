package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log destination that is safe to use from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// start runs a gateway in front of upstream and returns it with its address.
func start(t *testing.T, upstream, key string, log io.Writer) (*Gateway, *httptest.Server) {
	t.Helper()
	g, err := New(Config{Upstream: upstream, EngineID: "demo", Key: key, Log: log, MaxBody: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

func TestForwardsPathQueryAndBody(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotQuery, gotBody = r.URL.Path, r.URL.RawQuery, string(b)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	resp, err := http.Post(gw.URL+"/v1/chat/completions?x=1", "application/json", strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotPath != "/v1/chat/completions" || gotQuery != "x=1" || gotBody != `{"model":"m1"}` {
		t.Errorf("upstream saw path=%q query=%q body=%q", gotPath, gotQuery, gotBody)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("client saw %q", body)
	}
}

func TestStampsEngineAndModelHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	resp, _ := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m1"}`))
	resp.Body.Close()
	if resp.Header.Get("X-Ezbx-Engine") != "demo" || resp.Header.Get("X-Ezbx-Model") != "m1" {
		t.Errorf("headers: %v", resp.Header)
	}
	if resp.Header.Get("X-Ezbx-Request-Id") == "" {
		t.Error("missing request id")
	}

	// A request with no body has no model, so that header is left out.
	resp, _ = http.Get(gw.URL + "/v1/models")
	resp.Body.Close()
	if resp.Header.Get("X-Ezbx-Model") != "" {
		t.Errorf("model header should be absent, got %q", resp.Header.Get("X-Ezbx-Model"))
	}
}

func TestKeyIsInjectedAndTheBoxsPlaceholderNeverReachesTheEngine(t *testing.T) {
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(up.Close)

	// With a key configured, the real key replaces the placeholder.
	_, gw := start(t, up.URL, "real-key", nil)
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer box-placeholder")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if seen != "Bearer real-key" {
		t.Errorf("engine saw Authorization %q", seen)
	}

	// With no key configured, the placeholder is simply dropped.
	_, gw2 := start(t, up.URL, "", nil)
	req, _ = http.NewRequest(http.MethodGet, gw2.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer box-placeholder")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if seen != "" {
		t.Errorf("placeholder leaked to the engine: %q", seen)
	}
}

func TestBoxAddressIsNotForwarded(t *testing.T) {
	var forwarded string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.Header.Get("X-Forwarded-For")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	resp, _ := http.Get(gw.URL + "/v1/models")
	resp.Body.Close()
	if forwarded != "" {
		t.Errorf("X-Forwarded-For leaked: %q", forwarded)
	}
}

// The first chunk must reach the client while the engine is still working.
func TestStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson") // not SSE, so Go would not flush it on its own
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	// A gateway that buffers never sends headers either, so the wait for
	// headers needs its own limit or this test would hang instead of failing.
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Second}}
	resp, err := client.Get(gw.URL + "/v1/chat/completions")
	if err != nil {
		close(release)
		t.Fatalf("no reply started while the engine was still working: the gateway is buffering (%v)", err)
	}
	defer resp.Body.Close()

	first := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: one\n" {
			t.Fatalf("first chunk: %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk did not arrive while the engine was still working: the gateway is buffering")
	}
	close(release)
}

// If the client disconnects, the engine's request must be cancelled so it
// stops generating.
func TestClientDisconnectCancelsTheEngineRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, gw.URL+"/v1/chat/completions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	<-started
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the engine request was not cancelled after the client left")
	}
}

func TestEngineDownGivesAClearOpenAIStyleError(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	addr := up.URL
	up.Close() // nothing listens there now
	_, gw := start(t, addr, "", nil)

	resp, err := http.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Error struct{ Message, Type string }
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if resp.StatusCode != http.StatusBadGateway || body.Error.Type != "engine_unreachable" {
		t.Fatalf("status=%d body=%+v", resp.StatusCode, body)
	}
	if !strings.Contains(body.Error.Message, "ezbx doctor") {
		t.Errorf("message should point at doctor: %q", body.Error.Message)
	}
}

func TestOnlyV1PathsAreServed(t *testing.T) {
	hit := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	for _, p := range []string{"/", "/admin", "/props", "/api/ps", "/v1", "/v2/models"} {
		resp, _ := http.Get(gw.URL + p)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, resp.StatusCode)
		}
	}
	if hit {
		t.Fatal("a non-/v1 path reached the engine")
	}
}

func TestOversizedRequestsAreRefused(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an oversized request reached the engine")
	}))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil) // the limit is 1 KiB in tests

	resp, _ := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(strings.Repeat("x", 4096)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", resp.StatusCode)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	}))
	g, gw := start(t, up.URL, "", nil)
	a, err := NewAdmin(g, nil, testToken)
	if err != nil {
		t.Fatal(err)
	}
	adm := httptest.NewServer(a)
	t.Cleanup(adm.Close)
	ready := func() int {
		code, _ := call(t, adm, http.MethodGet, "/admin/ready", testToken, "")
		return code
	}

	// Readiness is only on the admin API, so the box can't make the gateway
	// ask the engine over and over.
	resp, _ := http.Get(gw.URL + "/readyz")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("readyz on the model route: %d, want 404", resp.StatusCode)
	}

	resp, _ = http.Get(gw.URL + "/healthz")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
	if code := ready(); code != 200 {
		t.Errorf("ready with a working engine: %d", code)
	}

	// An engine that answers but lists no models is not ready.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(empty.Close)
	if err := g.SetUpstream(empty.URL, "demo"); err != nil {
		t.Fatal(err)
	}
	if code := ready(); code != http.StatusServiceUnavailable {
		t.Errorf("ready with no models: %d", code)
	}

	// An engine that is gone is not ready either, but the gateway is still healthy.
	up.Close()
	if err := g.SetUpstream(up.URL, "demo"); err != nil {
		t.Fatal(err)
	}
	if code := ready(); code != http.StatusServiceUnavailable {
		t.Errorf("ready with engine down: %d", code)
	}
	resp, _ = http.Get(gw.URL + "/healthz")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz must stay 200 when only the engine is down: %d", resp.StatusCode)
	}
}

func TestSwitchingEngines(t *testing.T) {
	name := func(n string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, n) }))
		t.Cleanup(s.Close)
		return s
	}
	a, b := name("A"), name("B")
	g, gw := start(t, a.URL, "", nil)

	get := func() (string, string) {
		resp, err := http.Get(gw.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body), resp.Header.Get("X-Ezbx-Engine")
	}
	if body, eng := get(); body != "A" || eng != "demo" {
		t.Fatalf("before: %q %q", body, eng)
	}
	if err := g.SetUpstream(b.URL, "second"); err != nil {
		t.Fatal(err)
	}
	if body, eng := get(); body != "B" || eng != "second" {
		t.Fatalf("after: %q %q", body, eng)
	}
}

func TestUpstreamAddressesAreChecked(t *testing.T) {
	bad := []string{
		"", "127.0.0.1:8080", "ftp://127.0.0.1", "file:///etc/hosts", "http://",
		"http://user:pass@127.0.0.1", "http://127.0.0.1:8080/some/path", "http://127.0.0.1:8080?x=1",
	}
	for _, raw := range bad {
		if _, err := New(Config{Upstream: raw}); err == nil {
			t.Errorf("%q should be refused", raw)
		}
	}
	if _, err := New(Config{Upstream: "http://127.0.0.1:8080/"}); err != nil {
		t.Errorf("a plain address must be accepted: %v", err)
	}
}

func TestLogRecordsTheRequestButNeverContentOrKeys(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"secret-reply":"nope"}`)
	}))
	t.Cleanup(up.Close)
	var logs syncBuffer
	_, gw := start(t, up.URL, "super-secret-key", &logs)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[{"content":"private prompt"}]}`))
	req.Header.Set("Authorization", "Bearer box-placeholder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The log line is written just after the reply, so give it a moment.
	var line string
	for i := 0; i < 50 && line == ""; i++ {
		line = logs.String()
		time.Sleep(20 * time.Millisecond)
	}

	var e entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &e); err != nil {
		t.Fatalf("log line is not JSON: %q (%v)", line, err)
	}
	if e.Route != "model" || e.Method != "POST" || e.Path != "/v1/chat/completions" || e.Model != "m1" || e.Status != 200 || e.Engine != "demo" {
		t.Errorf("log entry: %+v", e)
	}
	for _, forbidden := range []string{"private prompt", "secret-reply", "super-secret-key", "box-placeholder"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("log contains %q", forbidden)
		}
	}
}

func TestModelNamesAreSanitised(t *testing.T) {
	cases := map[string]string{
		"qwen3.8-flash-next-iq2_xs": "qwen3.8-flash-next-iq2_xs",
		"":                          "",
		"bad\nname":                 "",
		"café":                      "",
		strings.Repeat("a", 201):    "",
	}
	for in, want := range cases {
		if got := headerSafe(in); got != want {
			t.Errorf("headerSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
