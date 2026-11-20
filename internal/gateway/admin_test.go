package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// testToken is long enough to be accepted, and plainly not a real secret.
var testToken = strings.Repeat("test-token-", 4)

func newAdminEnv(t *testing.T) (*Gateway, *Proxy, *httptest.Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	g, err := New(Config{Upstream: "http://127.0.0.1:1", EngineID: "first", Log: logs})
	if err != nil {
		t.Fatal(err)
	}
	p, err := g.NewProxy(ProxyConfig{Mode: ModeOff})
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAdmin(g, p, testToken+"\n") // a token file usually ends in a newline
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return g, p, srv, logs
}

func call(t *testing.T, srv *httptest.Server, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := quick.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestAdminNeedsTheToken(t *testing.T) {
	_, _, srv, logs := newAdminEnv(t)
	for _, tok := range []string{"", "wrong", testToken[:31], testToken + "x", strings.ToUpper(testToken)} {
		if code, _ := call(t, srv, http.MethodGet, "/admin/status", tok, ""); code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", tok, code)
		}
	}
	// Basic auth and a token in the query string are not accepted.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/status?token="+testToken, nil)
	req.SetBasicAuth("admin", testToken)
	resp, err := quick.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("basic auth and query token: status %d, want 401", resp.StatusCode)
	}
	if strings.Contains(logs.String(), testToken) {
		t.Error("the token appears in the log")
	}
}

func TestAdminRefusesShortTokens(t *testing.T) {
	g, err := New(Config{Upstream: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"", "short", "   " + testToken[:20] + "   "} {
		if _, err := NewAdmin(g, nil, tok); err == nil {
			t.Errorf("token %q accepted", tok)
		}
	}
}

func TestAdminStatusAndEngineSwitch(t *testing.T) {
	g, _, srv, _ := newAdminEnv(t)
	code, st := call(t, srv, http.MethodGet, "/admin/status", testToken, "")
	if code != 200 || st["engine"] != "first" || st["upstream"] != "http://127.0.0.1:1" {
		t.Fatalf("status: %d %v", code, st)
	}
	if net, _ := st["network"].(map[string]any); net["mode"] != ModeOff {
		t.Errorf("status network: %v", st["network"])
	}

	code, st = call(t, srv, http.MethodPut, "/admin/engine", testToken, `{"upstream":"http://127.0.0.1:2","engine":"second"}`)
	if code != 200 || st["engine"] != "second" {
		t.Fatalf("engine switch: %d %v", code, st)
	}
	if up, id := g.Upstream(); up != "http://127.0.0.1:2" || id != "second" {
		t.Errorf("gateway upstream after switch: %s %s", up, id)
	}

	for _, body := range []string{
		`{"upstream":"file:///etc/passwd","engine":"x"}`,
		`{"upstream":"http://127.0.0.1:2/v1","engine":"x"}`,
		`{"upstream":"http://127.0.0.1:2","engine":"x","extra":1}`,
		`not json`,
	} {
		if code, _ := call(t, srv, http.MethodPut, "/admin/engine", testToken, body); code != http.StatusBadRequest {
			t.Errorf("engine body %s: status %d, want 400", body, code)
		}
	}
	if up, _ := g.Upstream(); up != "http://127.0.0.1:2" {
		t.Errorf("a refused switch changed the upstream to %s", up)
	}
}

func TestAdminNetwork(t *testing.T) {
	_, p, srv, _ := newAdminEnv(t)
	code, out := call(t, srv, http.MethodPut, "/admin/network", testToken, `{"mode":"allow","allow":["registry.npmjs.org"]}`)
	if code != 200 || out["mode"] != ModeAllow {
		t.Fatalf("set network: %d %v", code, out)
	}
	code, _ = call(t, srv, http.MethodPost, "/admin/network/allow", testToken, `{"domain":"*.github.com"}`)
	if code != 200 {
		t.Fatalf("allow domain: %d", code)
	}
	if mode, list := p.Network(); mode != ModeAllow || strings.Join(list, ",") != "*.github.com,registry.npmjs.org" {
		t.Errorf("network now %s %v", mode, list)
	}

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/admin/network", `{"mode":"open"}`},
		{http.MethodPut, "/admin/network", `{"mode":"on","allow":["10.0.0.1"]}`},
		{http.MethodPost, "/admin/network/allow", `{"domain":"localhost"}`},
		{http.MethodPost, "/admin/network/allow", `{"domain":"192.168.1.1"}`},
	} {
		if code, _ := call(t, srv, c.method, c.path, testToken, c.body); code != http.StatusBadRequest {
			t.Errorf("%s %s %s: status %d, want 400", c.method, c.path, c.body, code)
		}
	}
	if code, _ := call(t, srv, http.MethodGet, "/admin/network", testToken, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET /admin/network: status %d, want 405", code)
	}
	if code, _ := call(t, srv, http.MethodGet, "/admin/unknown", testToken, ""); code != http.StatusNotFound {
		t.Errorf("unknown call: status %d, want 404", code)
	}
}

func TestAdminAllowIsSafeWhenCalledAtOnce(t *testing.T) {
	_, p, srv, _ := newAdminEnv(t)
	names := []string{"a.example", "b.example", "c.example", "d.example", "e.example", "f.example", "g.example", "h.example"}
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			call(t, srv, http.MethodPost, "/admin/network/allow", testToken, `{"domain":"`+n+`"}`)
		}(n)
	}
	wg.Wait()
	if _, list := p.Network(); len(list) != len(names) {
		t.Errorf("allowlist has %d entries after %d approvals at once: %v", len(list), len(names), list)
	}
}

func TestAdminWithoutProxy(t *testing.T) {
	g, err := New(Config{Upstream: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAdmin(g, nil, testToken)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	defer srv.Close()
	if code, st := call(t, srv, http.MethodGet, "/admin/status", testToken, ""); code != 200 || st["network"] != nil {
		t.Errorf("status without proxy: %d %v", code, st)
	}
	if code, _ := call(t, srv, http.MethodPut, "/admin/network", testToken, `{"mode":"on"}`); code != http.StatusConflict {
		t.Errorf("network without proxy: status %d, want 409", code)
	}
}
