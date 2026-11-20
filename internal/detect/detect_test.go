package detect

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryan-ven/ezbx/internal/adapter"
	"github.com/ryan-ven/ezbx/providers"
)

// fakeEngine starts a server that answers the given paths with the given JSON.
// Any other path gets a 404, like a real engine without that route.
func fakeEngine(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newDetector(t *testing.T) *Detector {
	t.Helper()
	adapters, err := adapter.LoadFS(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	// SkipHintPorts: tests probe only the fake servers, never real local ports.
	return &Detector{Adapters: adapters, SkipHintPorts: true, Timeout: 2 * time.Second}
}

const (
	strataHealth = `{"status":"ok","max_context":262144,"model":"m1","images":true,"api_key":false}`
	strataModels = `{"object":"list","data":[{"id":"m1","status":{"value":"loaded"},"meta":{"n_ctx":262144},"architecture":{"input_modalities":["text","image"]}}]}`
	plainModels  = `{"object":"list","data":[{"id":"some-model","object":"model"}]}`
)

func TestStrataWinsOverEnginesWithSimilarRoutes(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/health": strataHealth, "/v1/models": strataModels})

	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 {
		t.Fatalf("want 1 engine, got %+v", res.Engines)
	}
	e := res.Engines[0]
	if e.ID != "strata" {
		t.Fatalf("want strata, got %s", e.ID)
	}
	if !strings.Contains(strings.Join(e.AlsoMatched, ","), "llamacpp") {
		t.Errorf("llamacpp should be listed as also matching, got %v", e.AlsoMatched)
	}
	if len(e.Models) != 1 {
		t.Fatalf("want 1 model, got %+v", e.Models)
	}
	m := e.Models[0]
	if m.ID != "m1" || m.ContextWindow != 262144 || strings.Join(m.Input, ",") != "text,image" {
		t.Errorf("model facts wrong: %+v", m)
	}
	if m.Loaded == nil || !*m.Loaded {
		t.Errorf("model should be marked loaded: %+v", m)
	}
}

func TestLlamaCppIsRecognisedByItsHealthRoute(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/health": `{"status":"ok"}`, "/v1/models": plainModels})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "llamacpp" {
		t.Fatalf("want llamacpp, got %+v", res.Engines)
	}
	// This engine doesn't report context size, so it must stay unknown.
	if res.Engines[0].Models[0].ContextWindow != 0 {
		t.Errorf("context window should be unknown (0)")
	}
}

func TestOllamaNeedsItsOwnRoute(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/api/ps": `{"models":[]}`, "/v1/models": plainModels})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "ollama" {
		t.Fatalf("want ollama, got %+v", res.Engines)
	}
}

func TestAPlainOpenAIServerIsNotCalledLMStudio(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/v1/models": plainModels})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "openai" || res.Engines[0].Name != "OpenAI-compatible server" {
		t.Fatalf("want the generic openai adapter, got %+v", res.Engines)
	}
	if res.Engines[0].Verified != "generic" || len(res.Engines[0].Models) != 1 {
		t.Errorf("generic engine: %+v", res.Engines[0])
	}
}

func TestLMStudioIsRecognisedByItsNativeRoute(t *testing.T) {
	srv := fakeEngine(t, map[string]string{
		"/v1/models":     plainModels,
		"/api/v1/models": `{"models":[{"type":"llm","key":"some-model","max_context_length":32768,"loaded_instances":[]}]}`,
	})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "lmstudio" {
		t.Fatalf("want lmstudio, got %+v", res.Engines)
	}
	if strings.Join(res.Engines[0].AlsoMatched, ",") != "openai" {
		t.Errorf("the generic adapter should be listed as also matching: %v", res.Engines[0].AlsoMatched)
	}
}

func TestEnginesWithNoModelsAreStillFound(t *testing.T) {
	empty := `{"object":"list","data":[]}`
	cases := map[string]map[string]string{
		"ollama":   {"/api/ps": `{"models":[]}`, "/v1/models": empty},
		"llamacpp": {"/health": `{"status":"ok"}`, "/v1/models": empty},
		"strata":   {"/health": `{"status":"ok","max_context":0,"model":null}`, "/v1/models": empty},
		"lmstudio": {"/v1/models": empty, "/api/v1/models": `{"models":[]}`},
		"openai":   {"/v1/models": empty},
	}
	for want, routes := range cases {
		srv := fakeEngine(t, routes)
		res := newDetector(t).Detect(context.Background(), []string{srv.URL})
		if len(res.Engines) != 1 || res.Engines[0].ID != want {
			t.Errorf("%s with no models: got %+v", want, res.Engines)
			continue
		}
		if res.Engines[0].Models == nil || len(res.Engines[0].Models) != 0 {
			t.Errorf("%s: models should be an empty list, got %#v", want, res.Engines[0].Models)
		}
	}
}

func TestOllamaReportsWhichModelsAreLoaded(t *testing.T) {
	srv := fakeEngine(t, map[string]string{
		"/api/ps":    `{"models":[{"name":"llama3:latest","model":"llama3:latest"}]}`,
		"/v1/models": `{"object":"list","data":[{"id":"llama3:latest"},{"id":"qwen3:8b"}]}`,
	})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 1 || res.Engines[0].ID != "ollama" {
		t.Fatalf("want ollama, got %+v", res.Engines)
	}
	got := map[string]bool{}
	for _, m := range res.Engines[0].Models {
		if m.Loaded == nil {
			t.Fatalf("%s: loaded should be known", m.ID)
		}
		got[m.ID] = *m.Loaded
	}
	if !got["llama3:latest"] || got["qwen3:8b"] {
		t.Errorf("loaded = %v, want only llama3:latest", got)
	}
}

func TestDetectCanRunFromSeveralPlacesAtOnce(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/health": strataHealth, "/v1/models": strataModels})
	d := newDetector(t)
	d.Timeout = 0 // "use the default": Detect must not write the default back
	d.Run = runnerReturning(statusJSON(srv.URL), nil)
	done := make(chan Result)
	for i := 0; i < 8; i++ {
		go func() { done <- d.Detect(context.Background(), []string{srv.URL}) }()
	}
	for i := 0; i < 8; i++ {
		if res := <-done; len(res.Engines) != 1 || res.Engines[0].ID != "strata" {
			t.Errorf("call %d: %+v", i, res.Engines)
		}
	}
	if d.Timeout != 0 {
		t.Errorf("Detect changed the Detector's timeout to %v", d.Timeout)
	}
}

func TestAModelListWithoutAListIsNotAnEngine(t *testing.T) {
	for _, body := range []string{`{"error":"unauthorized"}`, `{"data":"none"}`, `{"data":{"id":"x"}}`, `[]`} {
		srv := fakeEngine(t, map[string]string{"/v1/models": body})
		if res := newDetector(t).Detect(context.Background(), []string{srv.URL}); len(res.Engines) != 0 {
			t.Errorf("/v1/models answering %s must not count as an engine: %+v", body, res.Engines)
		}
	}
}

func TestNothingRunning(t *testing.T) {
	srv := fakeEngine(t, map[string]string{}) // answers 404 to everything
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 0 {
		t.Fatalf("want no engines, got %+v", res.Engines)
	}
}

func TestAnswersThatAreNotJSONAreIgnored(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/v1/models": "<html>not an engine</html>"})
	res := newDetector(t).Detect(context.Background(), []string{srv.URL})
	if len(res.Engines) != 0 {
		t.Fatalf("a web page must not count as an engine: %+v", res.Engines)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	target := fakeEngine(t, map[string]string{"/v1/models": plainModels})
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	res := newDetector(t).Detect(context.Background(), []string{redirector.URL})
	if len(res.Engines) != 0 {
		t.Fatalf("a redirect must not be followed: %+v", res.Engines)
	}
}

func TestServerThatNeverAnswersTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)

	d := newDetector(t)
	d.Timeout = 100 * time.Millisecond
	start := time.Now()
	res := d.Detect(context.Background(), []string{slow.URL})
	if len(res.Engines) != 0 {
		t.Fatalf("want no engines, got %+v", res.Engines)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("detection should give up quickly, took %v", time.Since(start))
	}
}

// ---- engine status commands ----

func runnerReturning(out string, err error) Runner {
	return func(context.Context, []string) ([]byte, error) { return []byte(out), err }
}

func statusJSON(url string) string {
	b, _ := json.Marshal(map[string]any{"running": []map[string]any{{"url": url}}})
	return string(b)
}

func TestFindsAServerOnACustomPortThroughTheEnginesOwnCommand(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/health": strataHealth, "/v1/models": strataModels})

	d := newDetector(t)
	// Real engines can print extra text after the JSON, so include some.
	d.Run = runnerReturning(statusJSON(srv.URL)+"\nsome-name: ready at "+srv.URL+"\n", nil)

	res := d.Detect(context.Background(), nil) // no explicit address given
	if len(res.Engines) != 1 || res.Engines[0].ID != "strata" {
		t.Fatalf("want strata found via its command, got %+v notes=%v", res.Engines, res.Notes)
	}
}

func TestACommandThatPointsAwayFromThisComputerIsIgnored(t *testing.T) {
	d := newDetector(t)
	d.Run = runnerReturning(statusJSON("http://203.0.113.9:8080"), nil)

	res := d.Detect(context.Background(), nil)
	if len(res.Engines) != 0 {
		t.Fatalf("a remote address from a command must never be probed: %+v", res.Engines)
	}
	if len(res.Notes) == 0 {
		t.Error("the user should be told something was ignored")
	}
}

func TestARemoteAddressDoesNotHideTheLocalOnesAfterIt(t *testing.T) {
	srv := fakeEngine(t, map[string]string{"/health": strataHealth, "/v1/models": strataModels})
	out, _ := json.Marshal(map[string]any{"running": []map[string]any{
		{"url": "http://203.0.113.9:8080"},
		{"url": srv.URL},
		{"url": "http://192.168.1.20:8080"},
	}})
	d := newDetector(t)
	d.Run = runnerReturning(string(out), nil)

	res := d.Detect(context.Background(), nil)
	if len(res.Engines) != 1 || res.Engines[0].URL != srv.URL {
		t.Fatalf("the local server after a remote one must still be found: %+v", res.Engines)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "ignored 2 server(s)") {
		t.Errorf("notes = %v, want one note about 2 ignored servers", res.Notes)
	}
}

func TestACommandThatFailsOrIsGarbageIsReportedNotFatal(t *testing.T) {
	for name, run := range map[string]Runner{
		"missing":  runnerReturning("", errors.New("not found")),
		"garbage":  runnerReturning("this is not json", nil),
		"no_urls":  runnerReturning(`{"running": []}`, nil),
		"wrongtyp": runnerReturning(`{"running": [{"url": 5}]}`, nil),
	} {
		d := newDetector(t)
		d.Run = run
		res := d.Detect(context.Background(), nil)
		if len(res.Engines) != 0 {
			t.Errorf("%s: want no engines, got %+v", name, res.Engines)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8080": true,
		"http://localhost:1234": true,
		"http://[::1]:8080":     true,
		"http://192.168.1.5:80": false,
		"http://example.com":    false,
		"not a url":             false,
	}
	for raw, want := range cases {
		if got := isLoopback(raw); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", raw, got, want)
		}
	}
}
