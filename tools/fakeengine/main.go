// Command fakeengine is a stand-in inference engine for tests that need a
// real network path but no real model. It answers /v1/models and
// /v1/chat/completions with fixed replies and listens on loopback by default,
// like a real engine would.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// reply is the fixed answer to every chat request.
const reply = "Hello from the fake engine."

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "address to listen on")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"fake-model","object":"model","owned_by":"ezbx"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"fake","object":"chat.completion","model":"fake-model",`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":"`+reply+`"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			return
		}
		// A streamed reply: one chunk per word, then the end marker.
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunk := func(delta, finish string) {
			fmt.Fprintf(w, `data: {"id":"fake","object":"chat.completion.chunk","model":"fake-model",`+
				`"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n", delta, finish)
			if flusher != nil {
				flusher.Flush()
			}
		}
		chunk(`{"role":"assistant","content":""}`, "null")
		for i, word := range strings.Fields(reply) {
			if i > 0 {
				word = " " + word
			}
			b, _ := json.Marshal(map[string]string{"content": word})
			chunk(string(b), "null")
		}
		chunk(`{}`, `"stop"`)
		io.WriteString(w, "data: [DONE]\n\n")
	})

	fmt.Fprintln(os.Stderr, "fake engine listening on", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
