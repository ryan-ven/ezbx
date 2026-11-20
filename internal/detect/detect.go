// Package detect finds inference engines running on this computer.
//
// Ports are only hints. An engine counts as found when its server answers the
// adapter's probes with the fields the adapter expects. See
// docs/engine-adapters.md.
package detect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ryan-ven/ezbx/internal/adapter"
	"github.com/ryan-ven/ezbx/internal/jsonpath"
)

// maxBody stops a misbehaving server from making us read forever.
const maxBody = 1 << 20 // 1 MiB

// Runner runs a command and returns what it printed. It is a field so tests
// can replace it without starting real programs.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// Detector holds everything detection needs. Detect doesn't change it, so
// one Detector can be used by several callers at once.
type Detector struct {
	Adapters      []adapter.Adapter
	Run           Runner        // nil means "never run commands"
	Timeout       time.Duration // per request; 0 means 500ms
	Hosts         []string      // hosts to probe for hint ports; nil means 127.0.0.1
	SkipHintPorts bool          // tests use this to probe only given addresses
}

// Model is one model an engine reports.
type Model struct {
	ID            string   `json:"id"`
	ContextWindow int      `json:"contextWindow,omitempty"`
	Input         []string `json:"input,omitempty"`
	Loaded        *bool    `json:"loaded,omitempty"`
}

// Engine is one engine that was found.
type Engine struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Models      []Model  `json:"models"`
	Specificity int      `json:"specificity"`
	AlsoMatched []string `json:"alsoMatched,omitempty"`
	Verified    string   `json:"verified"`
}

// Result is what Detect returns.
type Result struct {
	Engines []Engine `json:"engines"`
	Notes   []string `json:"notes,omitempty"`
}

// scan holds what one Detect call needs, so the Detector itself never changes.
type scan struct {
	d       *Detector
	client  *http.Client
	timeout time.Duration
}

// Detect looks for engines. extra holds addresses the user gave explicitly.
func (d *Detector) Detect(ctx context.Context, extra []string) Result {
	s := &scan{d: d, timeout: d.Timeout}
	if s.timeout <= 0 {
		s.timeout = 500 * time.Millisecond
	}
	s.client = &http.Client{
		Timeout:   s.timeout,
		Transport: &http.Transport{Proxy: nil}, // probes go straight to this computer
		// A redirect could point somewhere else entirely, so never follow one.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var res Result
	candidates := map[string]bool{}
	add := func(raw string) {
		candidates[strings.TrimRight(raw, "/")] = true
	}

	// 1. Ask engine programs which servers are running (finds custom ports).
	for _, a := range d.Adapters {
		if a.Detect.CLI == nil || d.Run == nil {
			continue
		}
		urls, note := d.askCLI(ctx, a)
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
		for _, u := range urls {
			add(u)
		}
	}

	// 2. Probe the usual ports on this computer only.
	if !d.SkipHintPorts {
		hosts := d.Hosts
		if len(hosts) == 0 {
			hosts = []string{"127.0.0.1"}
		}
		for _, a := range d.Adapters {
			for _, port := range a.Detect.HintPorts {
				for _, h := range hosts {
					add("http://" + net.JoinHostPort(h, fmt.Sprint(port)))
				}
			}
		}
	}

	// 3. Addresses the user typed are trusted as given.
	for _, u := range extra {
		add(u)
	}

	// 4. Check every candidate at the same time.
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for base := range candidates {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			if eng, ok := s.check(ctx, base); ok {
				mu.Lock()
				res.Engines = append(res.Engines, eng)
				mu.Unlock()
			}
		}(base)
	}
	wg.Wait()

	sort.Slice(res.Engines, func(i, j int) bool {
		if res.Engines[i].ID != res.Engines[j].ID {
			return res.Engines[i].ID < res.Engines[j].ID
		}
		return res.Engines[i].URL < res.Engines[j].URL
	})
	return res
}

// askCLI runs an adapter's status command and returns the loopback URLs it
// lists. Addresses on other computers are skipped, never probed, and the user
// is told once.
func (d *Detector) askCLI(ctx context.Context, a adapter.Adapter) ([]string, string) {
	out, err := d.Run(ctx, a.Detect.CLI.Command)
	if err != nil {
		return nil, fmt.Sprintf("%s: its command is not available", a.Name)
	}
	// Some commands print extra text after the JSON, so read only the first
	// JSON value and ignore whatever follows.
	var doc any
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&doc); err != nil {
		return nil, fmt.Sprintf("%s: its command gave unreadable output", a.Name)
	}

	var urls []string
	skipped := 0
	for _, v := range jsonpath.Collect(doc, a.Detect.CLI.Reads) {
		raw, ok := v.(string)
		if !ok {
			continue
		}
		if !isLoopback(raw) {
			skipped++
			continue
		}
		urls = append(urls, raw)
	}
	if skipped > 0 {
		return urls, fmt.Sprintf("%s: ignored %d server(s) that are not on this computer", a.Name, skipped)
	}
	return urls, ""
}

// isLoopback is true for addresses that point at this same computer.
func isLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type match struct {
	adapter     adapter.Adapter
	specificity int
}

// check decides which engine, if any, lives at base.
func (s *scan) check(ctx context.Context, base string) (Engine, bool) {
	cache := map[string]any{} // answers already fetched from this address

	var matches []match
	for _, a := range s.d.Adapters {
		if spec, ok := s.matches(ctx, base, a, cache); ok {
			matches = append(matches, match{a, spec})
		}
	}
	if len(matches) == 0 {
		return Engine{}, false
	}

	// More specific adapters win. Ties go to the alphabetically first id.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].specificity != matches[j].specificity {
			return matches[i].specificity > matches[j].specificity
		}
		return matches[i].adapter.ID < matches[j].adapter.ID
	})

	best := matches[0]
	eng := Engine{
		ID:          best.adapter.ID,
		Name:        best.adapter.Name,
		URL:         base,
		Models:      []Model{},
		Specificity: best.specificity,
		Verified:    best.adapter.Verified.Status,
	}
	for _, m := range matches[1:] {
		eng.AlsoMatched = append(eng.AlsoMatched, m.adapter.ID)
	}
	if doc, ok := s.fetch(ctx, base, best.adapter.OpenAI.ModelsPath, cache); ok {
		eng.Models = readModels(doc, best.adapter.Models)
		s.markLoaded(ctx, base, best.adapter.Models, eng.Models, cache)
	}
	return eng, true
}

// matches runs one adapter's probes against base. Every required probe must
// answer with all of its expected fields and lists. The score counts them.
func (s *scan) matches(ctx context.Context, base string, a adapter.Adapter, cache map[string]any) (int, bool) {
	score := 0
	for _, p := range a.Detect.Probes {
		doc, ok := s.fetch(ctx, base, p.Path, cache)
		if ok {
			for _, field := range p.Expect.HasFields {
				if !jsonpath.Has(doc, field) {
					ok = false
					break
				}
			}
		}
		if ok {
			for _, l := range p.Expect.HasLists {
				if !jsonpath.IsList(doc, l) {
					ok = false
					break
				}
			}
		}
		if !ok {
			if p.Optional {
				continue
			}
			return 0, false
		}
		score += len(p.Expect.HasFields) + len(p.Expect.HasLists)
	}
	return score, true
}

// fetch does a plain GET and decodes the JSON answer. It sends no key and no
// information about the user.
func (s *scan) fetch(ctx context.Context, base, path string, cache map[string]any) (any, bool) {
	if doc, seen := cache[path]; seen {
		return doc, doc != nil
	}
	cache[path] = nil

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ezbx-detect")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}

	var doc any
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&doc); err != nil {
		return nil, false
	}
	cache[path] = doc
	return doc, true
}

// markLoaded handles the "/route list[].field" form of models.loaded: a model
// is loaded when another route lists its id. If that route doesn't answer,
// loaded stays unknown rather than being reported as false.
func (s *scan) markLoaded(ctx context.Context, base string, spec adapter.Models, models []Model, cache map[string]any) {
	route, path, ok := spec.LoadedIn()
	if !ok {
		return
	}
	doc, ok := s.fetch(ctx, base, route, cache)
	if !ok {
		return
	}
	listed := map[string]bool{}
	for _, v := range jsonpath.Collect(doc, path) {
		if str, isStr := v.(string); isStr {
			listed[str] = true
		}
	}
	for i := range models {
		loaded := listed[models[i].ID]
		models[i].Loaded = &loaded
	}
}

// readModels turns an engine's model list into Model values, using only the
// facts the adapter says the engine reports.
func readModels(doc any, spec adapter.Models) []Model {
	out := []Model{}
	arrayPath, idPath, ok := jsonpath.SplitArray(spec.ID)
	if !ok {
		return out
	}
	list, _ := jsonpath.Get(doc, arrayPath)
	items, _ := list.([]any)

	for _, item := range items {
		id, _ := jsonpath.Get(item, idPath)
		name, isString := id.(string)
		if !isString || name == "" {
			continue
		}
		m := Model{ID: name}

		if v, ok := factAt(item, arrayPath, spec.ContextWindow); ok {
			if n, isNum := v.(float64); isNum {
				m.ContextWindow = int(n)
			}
		}
		if v, ok := factAt(item, arrayPath, spec.Input); ok {
			if list, isList := v.([]any); isList {
				for _, s := range list {
					if str, isStr := s.(string); isStr {
						m.Input = append(m.Input, str)
					}
				}
			}
		}
		if spec.Loaded != nil {
			if path, want, found := strings.Cut(*spec.Loaded, " == "); found {
				if rel, ok := relative(path, arrayPath); ok {
					if v, ok := jsonpath.Get(item, rel); ok {
						loaded := fmt.Sprint(v) == want
						m.Loaded = &loaded
					}
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// factAt reads one fact for one list item, when the adapter names a path.
func factAt(item any, arrayPath string, path *string) (any, bool) {
	if path == nil {
		return nil, false
	}
	rel, ok := relative(*path, arrayPath)
	if !ok {
		return nil, false
	}
	return jsonpath.Get(item, rel)
}

// relative turns "data[].meta.n_ctx" into "meta.n_ctx" for a list at "data".
func relative(path, arrayPath string) (string, bool) {
	prefix := arrayPath + "[]."
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}
