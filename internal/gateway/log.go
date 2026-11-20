package gateway

import (
	"encoding/json"
	"io"
	"sync"
)

// entry is one log line. It records what was asked and how it went, never
// the prompt, the reply or any key.
type entry struct {
	Time   string `json:"time"`
	ID     string `json:"id"`
	Route  string `json:"route"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Engine string `json:"engine"`
	Model  string `json:"model,omitempty"`
	Status int    `json:"status"`
	Ms     int64  `json:"ms"`
	TTFBMs int64  `json:"ttfb_ms"`
	End    string `json:"end,omitempty"` // client_gone or engine_idle when a reply was cut short
}

type logger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newLogger(w io.Writer) *logger {
	if w == nil {
		return &logger{}
	}
	return &logger{enc: json.NewEncoder(w)}
}

func (l *logger) write(e entry) {
	if l.enc == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(e)
}
