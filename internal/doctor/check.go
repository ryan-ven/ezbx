// Package doctor checks that a box is set up the way EZBox's safety design
// says it must be, and explains how to fix what isn't. Some checks run on
// the host (Docker, the engine, the containers' settings) and some run in a
// throwaway container on the box's inside network, seeing what the box sees.
// See docs/gateway.md, section 7.
package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Status is the outcome of one check.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn" // works, but something deserves attention
	Fail Status = "fail" // broken, or the box is less safe than designed
	Skip Status = "skip" // could not be checked, usually because an earlier check failed
)

// Check is one result.
type Check struct {
	Group  string `json:"group"`
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"` // what to do about a warn or fail
}

// Report is a list of checks in the order they ran.
type Report struct {
	Checks []Check `json:"checks"`
}

func (r *Report) add(group, name string, s Status, detail, fix string) {
	r.Checks = append(r.Checks, Check{Group: group, Name: name, Status: s, Detail: detail, Fix: fix})
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Count returns how many checks had each status.
func (r Report) Count() map[Status]int {
	n := map[Status]int{}
	for _, c := range r.Checks {
		n[c.Status]++
	}
	return n
}

// Print writes the report as text, grouped, with fixes under failures.
func (r Report) Print(w io.Writer) {
	group := ""
	for _, c := range r.Checks {
		if c.Group != group {
			group = c.Group
			fmt.Fprintf(w, "\n%s\n", group)
		}
		mark := map[Status]string{Pass: "ok  ", Warn: "warn", Fail: "FAIL", Skip: "skip"}[c.Status]
		line := fmt.Sprintf("  %s  %s", mark, c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(w, line)
		if c.Fix != "" && (c.Status == Fail || c.Status == Warn) {
			fmt.Fprintf(w, "        fix: %s\n", c.Fix)
		}
	}
	n := r.Count()
	fmt.Fprintf(w, "\n%d passed, %d warnings, %d failed, %d skipped\n", n[Pass], n[Warn], n[Fail], n[Skip])
}

// PrintJSON writes the report as JSON.
func (r Report) PrintJSON(w io.Writer) {
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintln(w, string(b))
}

func joinOr(list []string, none string) string {
	if len(list) == 0 {
		return none
	}
	return strings.Join(list, ", ")
}
