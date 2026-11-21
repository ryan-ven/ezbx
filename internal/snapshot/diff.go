package snapshot

import (
	"bytes"
	"fmt"
	"strings"
)

// maxDiffCells bounds the line-by-line comparison (lines before x lines
// after, once the common start and end are set aside), so a huge change
// can't use up memory.
const maxDiffCells = 4_000_000

// Binary reports whether content looks like a binary file: a zero byte in
// its first 8 KB, as git decides.
func Binary(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	return bytes.IndexByte(b, 0) >= 0
}

type op struct {
	kind byte // ' ', '-' or '+'
	line string
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// lineOps works out the lines kept, removed and added. The common start and
// end are set aside first; the rest is compared with a longest common
// subsequence table, or shown as removed then added if it is too large.
func lineOps(a, b []string) []op {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []op
	for _, l := range a[:pre] {
		ops = append(ops, op{' ', l})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(ma)*len(mb) > maxDiffCells {
		for _, l := range ma {
			ops = append(ops, op{'-', l})
		}
		for _, l := range mb {
			ops = append(ops, op{'+', l})
		}
	} else {
		ops = append(ops, lcsOps(ma, mb)...)
	}
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, op{' ', l})
	}
	return ops
}

func lcsOps(a, b []string) []op {
	n, m := len(a), len(b)
	// t[i][j] is the length of the longest common subsequence of a[i:], b[j:].
	t := make([][]int32, n+1)
	for i := range t {
		t[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else if t[i+1][j] >= t[i][j+1] {
				t[i][j] = t[i+1][j]
			} else {
				t[i][j] = t[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	return ops
}

// Unified writes the difference between two texts in the familiar unified
// form, with three lines of context around each change. Windows line
// endings are treated like Unix ones.
func Unified(name string, before, after []byte) string {
	ops := lineOps(splitLines(before), splitLines(after))
	const ctx = 3
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", name, name)
	// Positions of changed ops, grouped into hunks that share context.
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-ctx, 0)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// Look ahead: another change within 2*ctx lines joins this hunk.
			k := end
			for k < len(ops) && ops[k].kind == ' ' && k-end < 2*ctx {
				k++
			}
			if k < len(ops) && ops[k].kind != ' ' {
				end = k
				continue
			}
			break
		}
		stop := min(end+ctx, len(ops))
		aLine, bLine := 1, 1
		for _, o := range ops[:start] {
			if o.kind != '+' {
				aLine++
			}
			if o.kind != '-' {
				bLine++
			}
		}
		aCount, bCount := 0, 0
		for _, o := range ops[start:stop] {
			if o.kind != '+' {
				aCount++
			}
			if o.kind != '-' {
				bCount++
			}
		}
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(aLine, aCount), hunkRange(bLine, bCount))
		for _, o := range ops[start:stop] {
			b.WriteByte(o.kind)
			b.WriteString(o.line)
			b.WriteByte('\n')
		}
		i = stop
	}
	return b.String()
}

func hunkRange(line, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", line-1)
	}
	if count == 1 {
		return fmt.Sprintf("%d", line)
	}
	return fmt.Sprintf("%d,%d", line, count)
}
