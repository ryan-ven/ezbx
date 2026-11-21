package snapshot

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// clock gives each call a later time, so snapshot ids differ.
func clock() func() time.Time {
	t := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state", "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	s.Now = clock()
	return s
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

var rules = Rules{Skip: DefaultSkip, MaxFile: 1000}

// workspace makes a small workspace: a project that is a git repository
// itself, an empty folder, a folder skipped by name and a too-large file.
func workspace(t *testing.T) string {
	root := filepath.Join(t.TempDir(), "ws")
	write(t, root, "notes.txt", "one\ntwo\n")
	write(t, root, "same-a.txt", "same content")
	write(t, root, "proj/main.go", "package main\n")
	write(t, root, "proj/.git/HEAD", "ref: refs/heads/main\n")
	write(t, root, "proj/node_modules/x/index.js", "module")
	write(t, root, "big.bin", strings.Repeat("x", 2000))
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func paths(s Snapshot) string {
	var p []string
	for _, e := range s.Files {
		p = append(p, e.Path)
	}
	return strings.Join(p, " ")
}

func TestTake(t *testing.T) {
	st, root := newStore(t), workspace(t)
	snap, stats, err := st.Take(root, Manual, "first", rules)
	if err != nil {
		t.Fatal(err)
	}
	// The nested repository's files and its .git folder are kept.
	if got := paths(snap); got != "notes.txt proj/.git/HEAD proj/main.go same-a.txt" {
		t.Errorf("files: %s", got)
	}
	if got := strings.Join(snap.Dirs, " "); got != "empty proj proj/.git" {
		t.Errorf("dirs: %s", got)
	}
	var skipped []string
	for _, s := range snap.Skipped {
		skipped = append(skipped, s.Path+"="+s.Reason)
	}
	sort.Strings(skipped)
	if got := strings.Join(skipped, ","); got != "big.bin=larger than 1000 B,proj/node_modules=skipped by name" {
		t.Errorf("skipped: %s", got)
	}
	if snap.Kind != Manual || snap.Note != "first" || snap.ID != "20260102-030406" || snap.Workspace != root || stats.Files != 4 || stats.New != 4 {
		t.Errorf("%+v %+v", snap, stats)
	}
	// Content comes back exactly.
	for _, e := range snap.Files {
		b, err := st.Content(e.Hash)
		if err != nil || string(b) != read(t, root, e.Path) {
			t.Errorf("%s: %q %v", e.Path, b, err)
		}
	}

	// Again, unchanged: everything is reused and nothing new is stored.
	write(t, root, "same-b.txt", "same content") // the same content as same-a.txt
	again, stats, err := st.Take(root, Auto, "", rules)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Reused != 4 || stats.New != 0 || again.ID == snap.ID {
		t.Errorf("unchanged files must be reused, and the same content stored once: %+v", stats)
	}
	list, _ := st.List()
	if len(list) != 2 || list[0].ID != snap.ID || list[1].ID != again.ID {
		t.Errorf("list: %v", list)
	}
	if latest, _ := st.Get(""); latest.ID != again.ID {
		t.Error("Get(\"\") is the newest")
	}

	// Same size, new time: read again and stored.
	p := filepath.Join(root, "notes.txt")
	write(t, root, "notes.txt", "ONE\ntwo\n")
	later := time.Now().Add(time.Hour)
	os.Chtimes(p, later, later)
	third, stats, err := st.Take(root, Auto, "", rules)
	if err != nil {
		t.Fatal(err)
	}
	if stats.New != 1 || third.Files[0].Hash == again.Files[0].Hash {
		t.Errorf("a file with a new time must be read again: %+v", stats)
	}
}

func TestRewind(t *testing.T) {
	st, root := newStore(t), workspace(t)
	target, _, err := st.Take(root, Auto, "", rules)
	if err != nil {
		t.Fatal(err)
	}
	// A session: change, delete, add (with a new folder), touch skipped
	// things, and remove the empty folder.
	write(t, root, "notes.txt", "one\nTWO\n")
	os.Remove(filepath.Join(root, "proj", "main.go"))
	write(t, root, "new/dir/file.txt", "agent was here")
	write(t, root, "proj/node_modules/x/index.js", "changed module")
	write(t, root, "big.bin", strings.Repeat("y", 2000))
	os.Remove(filepath.Join(root, "empty"))
	os.MkdirAll(filepath.Join(root, "proj", ".cache"), 0o755) // an empty skipped folder

	cur, err := Current(root, rules)
	if err != nil {
		t.Fatal(err)
	}
	ch := Compare(target, cur)
	if strings.Join(ch.Changed, ",") != "notes.txt" || strings.Join(ch.Deleted, ",") != "proj/main.go" || strings.Join(ch.Added, ",") != "new/dir/file.txt" {
		t.Fatalf("%+v", ch)
	}

	res, err := st.Rewind(root, target, rules)
	if err != nil || len(res.Problems) > 0 {
		t.Fatal(err, res.Problems)
	}
	if read(t, root, "notes.txt") != "one\ntwo\n" || read(t, root, "proj/main.go") != "package main\n" || read(t, root, "new/dir/file.txt") != "<missing>" {
		t.Error("the workspace must match the snapshot")
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Error("folders made since must be removed")
	}
	if fi, err := os.Stat(filepath.Join(root, "empty")); err != nil || !fi.IsDir() {
		t.Error("an empty folder must come back")
	}
	// What the snapshot left out is never touched.
	if read(t, root, "proj/node_modules/x/index.js") != "changed module" || read(t, root, "big.bin") != strings.Repeat("y", 2000) {
		t.Error("skipped paths must be left alone")
	}
	if fi, err := os.Stat(filepath.Join(root, "proj", ".cache")); err != nil || !fi.IsDir() {
		t.Error("a skipped folder must be left alone, even when empty")
	}
	if fi, _ := os.Stat(filepath.Join(root, "notes.txt")); fi.ModTime().UnixNano() != target.Files[0].MTime {
		t.Error("the file's time must come back too")
	}
	if cur, _ := Current(root, rules); !Compare(target, cur).Empty() {
		t.Errorf("after the rewind: %+v", Compare(target, cur))
	}

	// The safety snapshot is saved in the store and holds the session's
	// work, so the rewind can be undone.
	saved, err := st.Get(res.Safety.ID)
	if err != nil || saved.Kind != Rewind || !strings.Contains(saved.Note, target.ID) {
		t.Fatalf("%+v %v", saved, err)
	}
	if _, err := st.Rewind(root, saved, rules); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "new/dir/file.txt") != "agent was here" || read(t, root, "notes.txt") != "one\nTWO\n" || read(t, root, "proj/main.go") != "<missing>" {
		t.Error("rewinding to the safety snapshot must undo the rewind")
	}
}

// A change that keeps a file's size and time exactly (as touch -d could)
// is still found by review and undone by rewind, which read every file.
func TestRewindSeesSameSizeAndTime(t *testing.T) {
	st, root := newStore(t), workspace(t)
	target, _, err := st.Take(root, Auto, "", rules)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "notes.txt")
	fi, _ := os.Stat(p)
	write(t, root, "notes.txt", "ONE\ntwo\n") // same length
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	cur, _ := Current(root, rules)
	if ch := Compare(target, cur); strings.Join(ch.Changed, ",") != "notes.txt" {
		t.Fatalf("review must see it: %+v", ch)
	}
	if _, err := st.Rewind(root, target, rules); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "notes.txt") != "one\ntwo\n" {
		t.Error("rewind must undo it")
	}
}

// An agent could swap a folder for a link to somewhere else, so that a
// rewind writes there. It must refuse.
func TestRewindRefusesLinkedFolder(t *testing.T) {
	st, root := newStore(t), workspace(t)
	target, _, err := st.Take(root, Auto, "", rules)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.MkdirAll(outside, 0o755)
	os.RemoveAll(filepath.Join(root, "proj"))
	if err := os.Symlink(outside, filepath.Join(root, "proj")); err != nil {
		t.Skip("this system can't make links here:", err)
	}
	// The link itself is new, so the rewind removes it, then puts the folder back.
	res, err := st.Rewind(root, target, rules)
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside the workspace: %v", entries)
	}
	if read(t, root, "proj/main.go") != "package main\n" {
		t.Errorf("the folder must come back: %v", res.Problems)
	}
	// And inside() refuses a path through a link directly.
	os.RemoveAll(filepath.Join(root, "proj"))
	os.Symlink(outside, filepath.Join(root, "proj"))
	if _, err := inside(root, "proj/main.go"); err == nil || !strings.Contains(err.Error(), "not a plain folder") {
		t.Error(err)
	}
}

// What the target snapshot left out stays untouched even when today's rules
// would include it (the skip list was edited since).
func TestRewindKeepsWhatTargetLeftOut(t *testing.T) {
	st, root := newStore(t), workspace(t)
	os.MkdirAll(filepath.Join(root, "node_modules"), 0o755) // empty, skipped by name
	target, _, err := st.Take(root, Auto, "", rules)
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "notes.txt", "changed")
	if _, err := st.Rewind(root, target, Rules{MaxFile: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "node_modules")); err != nil || !fi.IsDir() {
		t.Error("a folder the snapshot left out must stay, even if empty")
	}
	if read(t, root, "proj/node_modules/x/index.js") != "module" || read(t, root, "big.bin") != strings.Repeat("x", 2000) {
		t.Error("files the snapshot left out must stay")
	}
	if read(t, root, "notes.txt") != "one\ntwo\n" {
		t.Error("the rest is rewound")
	}
}

func TestCheckPath(t *testing.T) {
	for _, p := range []string{"a", "a/b", "a b/c.txt", ".git/HEAD"} {
		if err := checkPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	bad := []string{"", ".", "..", "../x", "a/../../x", "/etc/passwd", "a//b", "a/./b", "a/"}
	if runtime.GOOS == "windows" {
		bad = append(bad, `a\..\..\x`, "C:x", "a:stream")
	}
	for _, p := range bad {
		if err := checkPath(p); err == nil {
			t.Errorf("%q must be refused", p)
		}
	}
}

// A damaged or edited snapshot file can't make rewind write outside.
func TestGetRefusesBadPaths(t *testing.T) {
	st := newStore(t)
	for _, bad := range []string{`"files": [{"path": "../outside.txt", "hash": "x"}], "dirs": []`, `"files": [], "dirs": ["../up"]`} {
		data := `{"version": 1, "id": "20260102-030405", "kind": "auto", "workspace": "w", ` + bad + `}`
		if err := os.WriteFile(filepath.Join(st.Dir, "snaps", "20260102-030405.json"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Get("20260102-030405"); err == nil || !strings.Contains(err.Error(), "refused the path") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := st.Get("../../etc"); err == nil || !strings.Contains(err.Error(), "not a snapshot id") {
		t.Error(err)
	}
	if _, err := st.Get("20991231-000000"); err == nil || !strings.Contains(err.Error(), "no snapshot") {
		t.Error(err)
	}
	empty := newStore(t)
	if _, err := empty.Get(""); err != ErrNone {
		t.Error(err)
	}
}

func TestPrune(t *testing.T) {
	st, root := newStore(t), workspace(t)
	var ids []string
	for i, kind := range []string{Auto, Manual, Auto, Rewind, Auto} {
		write(t, root, "notes.txt", strings.Repeat("v", i+1)) // new content each time
		s, _, err := st.Take(root, kind, "", rules)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	removed, freed, err := st.Prune(2)
	if err != nil {
		t.Fatal(err)
	}
	// Automatic and rewind snapshots count together; the manual one stays.
	if strings.Join(removed, ",") != ids[0]+","+ids[2] || freed == 0 {
		t.Errorf("removed %v, freed %d", removed, freed)
	}
	left, _ := st.List()
	var got []string
	for _, s := range left {
		got = append(got, s.ID)
	}
	if strings.Join(got, ",") != ids[1]+","+ids[3]+","+ids[4] {
		t.Errorf("left %v", got)
	}
	// Every remaining snapshot can still be read back in full.
	for _, s := range left {
		for _, e := range s.Files {
			if _, err := st.Content(e.Hash); err != nil {
				t.Errorf("%s %s: %v", s.ID, e.Path, err)
			}
		}
	}
	// Removing a snapshot by hand frees what only it used.
	before := st.StoreSize()
	if err := st.Remove(ids[1]); err != nil || st.StoreSize() >= before {
		t.Errorf("%v %d -> %d", err, before, st.StoreSize())
	}
}

func TestLock(t *testing.T) {
	st := newStore(t)
	st.Now = time.Now
	unlock, err := st.lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.lock(); err == nil || !strings.Contains(err.Error(), "another ezbx command") {
		t.Error("a second lock must wait", err)
	}
	if _, _, err := st.Take(t.TempDir(), Auto, "", rules); err == nil {
		t.Error("take must respect the lock")
	}
	unlock()
	// A lock left by a crash over an hour ago is taken over.
	file := filepath.Join(st.Dir, "lock")
	os.WriteFile(file, []byte("1\n"), 0o600)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(file, old, old)
	if u, err := st.lock(); err != nil {
		t.Error(err)
	} else {
		u()
	}
}

func TestCompareLeavesSkippedAlone(t *testing.T) {
	old := Snapshot{Files: []Entry{{Path: "a", Hash: "1"}, {Path: "gone", Hash: "2"}}, Skipped: []Skipped{{Path: "node_modules", Reason: "x"}, {Path: "big", Reason: "y"}}}
	cur := Snapshot{Files: []Entry{{Path: "a", Hash: "1"}, {Path: "node_modules/x", Hash: "3"}, {Path: "big", Hash: "4"}, {Path: "new", Hash: "5"}},
		Skipped: []Skipped{{Path: "gone", Reason: "now too large"}}}
	c := Compare(old, cur)
	if strings.Join(c.Added, ",") != "new" || len(c.Deleted) != 0 || len(c.Changed) != 0 {
		t.Errorf("%+v", c)
	}
	cur.Files[0].Link = "elsewhere"
	if c := Compare(old, cur); strings.Join(c.Changed, ",") != "a" {
		t.Errorf("a file turned into a link is a change: %+v", c)
	}
}

func TestUnified(t *testing.T) {
	got := Unified("f.txt", []byte("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"), []byte("a\nB\nc\nd\ne\nf\ng\nh\ni\nj\nk\n"))
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -1,5 +1,5 @@\n a\n-b\n+B\n c\n d\n e\n@@ -8,3 +8,4 @@\n h\n i\n j\n+k\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Changes close together share a hunk.
	got = Unified("f", []byte("1\n2\n3\n4\n5\n6\n7\n8\n"), []byte("1\nX\n3\n4\n5\nY\n7\n8\n"))
	if strings.Count(got, "@@ -") != 1 || !strings.Contains(got, "-2\n+X\n") || !strings.Contains(got, "-6\n+Y\n") {
		t.Error(got)
	}
	// Windows line endings don't count as a change; a new file is all added.
	if got := Unified("w", []byte("a\r\nb\r\n"), []byte("a\nb\n")); strings.Contains(got, "@@") {
		t.Error(got)
	}
	if got := Unified("n", nil, []byte("x\ny\n")); !strings.Contains(got, "@@ -0,0 +1,2 @@\n+x\n+y\n") {
		t.Error(got)
	}
	if got := Unified("d", []byte("x\n"), nil); !strings.Contains(got, "@@ -1 +0,0 @@\n-x\n") {
		t.Error(got)
	}
	// A huge changed region is shown as removed, then added, without a
	// table (which would have found the lines the two share).
	a, b := make([]string, 3000), make([]string, 3000)
	for i := range a {
		a[i] = "line " + strconv.Itoa(i)
		b[len(b)-1-i] = a[i]
	}
	ops := lineOps(a, b)
	kept := 0
	for _, o := range ops {
		if o.kind == ' ' {
			kept++
		}
	}
	if len(ops) != 6000 || kept != 0 || ops[0].kind != '-' || ops[5999].kind != '+' {
		t.Errorf("%d ops, %d kept", len(ops), kept)
	}
	// Small enough, the table finds what is shared.
	if ops := lineOps(a[:50], b[2950:]); len(ops) >= 100 {
		t.Errorf("a small change must be compared line by line: %d ops", len(ops))
	}
	if !Binary([]byte("a\x00b")) || Binary([]byte("plain text")) {
		t.Error("binary")
	}
}

func TestBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KB", 5 << 20: "5.0 MB", 3 << 30: "3.0 GB"} {
		if got := Bytes(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}
