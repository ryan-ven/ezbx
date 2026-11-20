package box

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Blocked is a host the proxy refused, with how often.
type Blocked struct {
	Host   string `json:"host"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	Last   string `json:"last"` // time of the last refusal, as logged
}

// RecentBlocked reads a gateway log and lists the hosts its proxy refused,
// most often first. Only refusals that approving the host would fix are
// listed (not on the allowlist, or the network off), not local addresses.
func RecentBlocked(log []byte) []Blocked {
	byHost := map[string]*Blocked{}
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e struct {
			Time    string `json:"time"`
			Route   string `json:"route"`
			Host    string `json:"host"`
			Blocked bool   `json:"blocked"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Route != "proxy" || !e.Blocked {
			continue
		}
		if e.Reason != "not_on_allowlist" && e.Reason != "network_off" {
			continue
		}
		h := strings.ToLower(e.Host)
		if name, _, err := net.SplitHostPort(h); err == nil {
			h = name
		}
		if h == "" || net.ParseIP(h) != nil {
			continue
		}
		b := byHost[h]
		if b == nil {
			b = &Blocked{Host: h}
			byHost[h] = b
		}
		b.Count++
		b.Reason, b.Last = e.Reason, e.Time
	}
	out := make([]Blocked, 0, len(byHost))
	for _, b := range byHost {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// TailFile reads at most the last n bytes of a file, starting at a line.
func TailFile(file string, n int64) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := st.Size() - n
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return data, nil
}

// LogFileFor is the gateway log in a box's state folder.
func LogFileFor(stateDir string) string {
	return filepath.Join(Plan{StateDir: stateDir}.LogDir(), "gateway.log")
}
