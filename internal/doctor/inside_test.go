package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTinyCompletion(t *testing.T) {
	cases := map[string]struct {
		handler func(w http.ResponseWriter, r *http.Request)
		want    Status
		detail  string
	}{
		"answers": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Ezbx-Model", "m")
			w.Write([]byte(`{}`))
		}, Pass, "answered in"},
		"too slow": {func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		}, Warn, "no answer within"},
		"refuses": {func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, Fail, "status 500"},
		"wrong model": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Ezbx-Model", "other")
		}, Fail, `names model "other"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.handler))
			defer srv.Close()
			s, detail, _ := tinyCompletion(context.Background(), srv.URL, "m", 200*time.Millisecond)
			if s != tc.want || !strings.Contains(detail, tc.detail) {
				t.Errorf("got %s %q, want %s %q", s, detail, tc.want, tc.detail)
			}
		})
	}
	// Nothing listening is a failure, not a slow model.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if s, _, _ := tinyCompletion(context.Background(), url, "m", 5*time.Second); s != Fail {
		t.Errorf("closed port: %s", s)
	}
}
