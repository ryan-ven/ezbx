// Command fakeengine is a stand-in inference engine for tests that need a
// real network path but no real model. It answers /v1/models and
// /v1/chat/completions with fixed replies and listens on loopback by default,
// like a real engine would.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "address to listen on")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"fake-model","object":"model","owned_by":"ezbx"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"fake","object":"chat.completion","model":"fake-model",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})

	fmt.Fprintln(os.Stderr, "fake engine listening on", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
