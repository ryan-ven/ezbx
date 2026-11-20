package gateway

// Tests for the hardening that followed the first audit: path checks,
// allowed methods, upgrades, header spoofing, timeouts, disconnect logging
// and the shared memory budget for request bodies.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// startWith runs a gateway with a full config.
func startWith(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv
}

// rawStatus sends one request exactly as written and returns the status line.
// Go's own client would tidy some of these paths, so a plain socket is used.
func rawStatus(t *testing.T, srv *httptest.Server, request string) string {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(c, request)
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}

// recorder collects something from the fake engine safely.
type recorder struct {
	mu   sync.Mutex
	list []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.list = append(r.list, s); r.mu.Unlock() }
func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.list...)
}

// quick is a client whose limit turns a hang into a test failure.
var quick = &http.Client{Timeout: 3 * time.Second}

// waitForLog returns the first log line once it has been written.
func waitForLog(t *testing.T, logs *syncBuffer) entry {
	t.Helper()
	var line string
	for i := 0; i < 100 && line == ""; i++ {
		line = strings.TrimSpace(logs.String())
		if line == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	var e entry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("log line is not JSON: %q (%v)", line, err)
	}
	return e
}

func TestPathsThatCouldLeaveV1AreRefused(t *testing.T) {
	var hits recorder
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.add(r.URL.Path) }))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	for _, p := range []string{
		"/v1/../props", "/v1/%2e%2e/props", "/v1/..%2fprops", "/v1/./models",
		"/v1//models", "/v1/..%5cprops", "/v1/models/%00",
	} {
		got := rawStatus(t, gw, "GET "+p+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		if !strings.Contains(got, " 400 ") {
			t.Errorf("%s: got %q, want 400", p, got)
		}
	}
	if h := hits.all(); len(h) != 0 {
		t.Fatalf("refused paths reached the engine: %v", h)
	}

	// An encoded slash inside a model name is fine: it cannot climb out of /v1/.
	resp, err := http.Get(gw.URL + "/v1/models/org%2Fmodel")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("model id with an encoded slash: status %d", resp.StatusCode)
	}
}

func TestOnlyGetHeadAndPostAreForwarded(t *testing.T) {
	var methods recorder
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { methods.add(r.Method) }))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "TRACE", "OPTIONS"} {
		req, _ := http.NewRequest(m, gw.URL+"/v1/models", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		allowed := m == "GET" || m == "HEAD" || m == "POST"
		if !allowed && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", m, resp.StatusCode)
		}
	}
	if got := strings.Join(methods.all(), ","); got != "GET,HEAD,POST" {
		t.Errorf("methods that reached the engine: %s", got)
	}
}

func TestProtocolUpgradesAreRefused(t *testing.T) {
	var hits recorder
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.add(r.URL.Path) }))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	got := rawStatus(t, gw, "GET /v1/models HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	if !strings.Contains(got, " 400 ") {
		t.Errorf("got %q, want 400", got)
	}
	if len(hits.all()) != 0 {
		t.Error("an upgrade request reached the engine")
	}
}

func TestEngineCannotSetGatewayHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ezbx-Model", "spoofed")
		w.Header().Set("X-Ezbx-Engine", "spoofed")
		w.Header().Set("X-Ezbx-Something-Else", "spoofed")
	}))
	t.Cleanup(up.Close)
	_, gw := start(t, up.URL, "", nil)

	resp, err := http.Get(gw.URL + "/v1/models") // no body, so no model
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for name, v := range resp.Header {
		if strings.HasPrefix(name, "X-Ezbx-") && v[0] == "spoofed" {
			t.Errorf("engine set %s", name)
		}
	}
}

func TestClientLeavingMidStreamIsLoggedAs499(t *testing.T) {
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(up.Close)
	var logs syncBuffer
	_, gw := start(t, up.URL, "", &logs)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, gw.URL+"/v1/chat/completions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	resp.Body.Close()

	if e := waitForLog(t, &logs); e.Status != 499 || e.End != "client_gone" {
		t.Errorf("log entry: %+v", e)
	}
}

func TestSilentEngineGivesATimeout(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answers
	}))
	t.Cleanup(up.Close)
	gw := startWith(t, Config{Upstream: up.URL, HeaderTimeout: 200 * time.Millisecond})

	// The client's own limit makes a missing timeout fail the test instead of hanging it.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatalf("no reply: the gateway did not time out (%v)", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status %d, want 504", resp.StatusCode)
	}
}

func TestEngineThatStallsMidStreamIsCutOff(t *testing.T) {
	engineStopped := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stalls until the gateway gives up
		close(engineStopped)
	}))
	t.Cleanup(up.Close)
	var logs syncBuffer
	gw := startWith(t, Config{Upstream: up.URL, IdleTimeout: 200 * time.Millisecond, Log: &logs})

	resp, err := http.Get(gw.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the reply did not end after the engine went quiet")
	}
	resp.Body.Close()
	select {
	case <-engineStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the engine request was not cancelled")
	}
	if e := waitForLog(t, &logs); e.End != "engine_idle" {
		t.Errorf("log entry: %+v", e)
	}
}

func TestLargeBodiesShareOneMemoryBudget(t *testing.T) {
	release := make(chan struct{})
	inEngine := make(chan struct{}, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inEngine <- struct{}{}
		<-release
	}))
	t.Cleanup(up.Close)
	gw := startWith(t, Config{Upstream: up.URL, MaxBody: 1024, MaxBuffered: 1500})
	// Registered last so it runs first: if the test stops early, the waiting
	// engine handler is released and the servers can shut down.
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	body := strings.Repeat("x", 1000)

	// The first request holds 1000 bytes of the 1500-byte budget while the engine works.
	first := make(chan int, 1)
	go func() {
		resp, err := quick.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			first <- 0
			return
		}
		resp.Body.Close()
		first <- resp.StatusCode
	}()
	<-inEngine

	// A second one would go over the budget, so it is refused at once.
	resp, err := quick.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Errorf("second request: status %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}

	// Once the first one finishes, its share is given back.
	letGo()
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first request: status %d", code)
	}
	resp, err = quick.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("request after the budget was freed: status %d", resp.StatusCode)
	}
}

func TestBusyStreamIsNotCutOffByTheIdleTimer(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 6; i++ { // runs for about 600 ms, longer than the idle time
			_, _ = io.WriteString(w, "data: chunk\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)
	gw := startWith(t, Config{Upstream: up.URL, IdleTimeout: 250 * time.Millisecond})

	resp, err := quick.Get(gw.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
		t.Fatalf("an active stream was cut short (err %v, got %q)", err, body)
	}
}
