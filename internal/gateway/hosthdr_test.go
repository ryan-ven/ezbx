package gateway

// Tests for the Host header sent to the engine. Engines with DNS rebinding
// protection, such as Strata, refuse a request whose Host isn't local, and
// on Docker Desktop the gateway reaches them as host.docker.internal.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostAliasIsPresentedAsLocalhost(t *testing.T) {
	g, err := New(Config{Upstream: "http://host.docker.internal:18080"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"http://host.docker.internal:18080":    "localhost:18080",
		"http://HOST.docker.internal:8080":     "localhost:8080",
		"http://gateway.docker.internal:11434": "localhost:11434",
		"http://host.docker.internal":          "localhost:80",
		"https://host.docker.internal":         "localhost:443",
		"http://127.0.0.1:8080":                "",
		"http://engine.example:8080":           "",
	}
	for up, want := range cases {
		if err := g.SetUpstream(up, "x"); err != nil {
			t.Fatal(err)
		}
		if got := g.target.Load().host; got != want {
			t.Errorf("%s: Host %q, want %q", up, got, want)
		}
	}
}

func TestUpstreamHostIsSentToTheEngine(t *testing.T) {
	var got []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Host)
		io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	}))
	t.Cleanup(up.Close)

	g, err := New(Config{Upstream: up.URL, UpstreamHost: "localhost:18080"})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	resp, err := quick.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	a, _ := NewAdmin(g, nil, testToken)
	adm := httptest.NewServer(a)
	t.Cleanup(adm.Close)
	if code, _ := call(t, adm, http.MethodGet, "/admin/ready", testToken, ""); code != 200 {
		t.Errorf("ready: %d", code)
	}

	if len(got) != 2 || got[0] != "localhost:18080" || got[1] != "localhost:18080" {
		t.Errorf("engine saw Host %v, want localhost:18080 for the model route and readiness", got)
	}

	for _, bad := range []string{"localhost:18080\r\nX-Evil: 1", "local host", "localhost:port", "a/b", ""} {
		if bad == "" {
			continue
		}
		if _, err := New(Config{Upstream: up.URL, UpstreamHost: bad}); err == nil {
			t.Errorf("upstream host %q accepted", bad)
		}
	}
}

func TestReadinessSaysWhyTheEngineRefused(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"type":"forbidden"}}`)
	}))
	t.Cleanup(up.Close)
	g, err := New(Config{Upstream: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := NewAdmin(g, nil, testToken)
	adm := httptest.NewServer(a)
	t.Cleanup(adm.Close)
	code, out := call(t, adm, http.MethodGet, "/admin/ready", testToken, "")
	if code != http.StatusServiceUnavailable || !strings.Contains(out["reason"].(string), "status 403") {
		t.Errorf("ready with a refusing engine: %d %v", code, out)
	}
}
