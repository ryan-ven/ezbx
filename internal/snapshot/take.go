package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"
)

// DefaultSkip lists folders and files that can be made again (installed
// packages, caches), so they are left out of snapshots by default.
var DefaultSkip = []string{"node_modules", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".next", ".cache"}

// DefaultMaxFile is the largest file a snapshot keeps.
const DefaultMaxFile = 100 << 20

// DefaultKeep is how many automatic snapshots are kept.
const DefaultKeep = 20

// Rules say what a snapshot leaves out. Skip holds names (with * and ? as
// wildcards) matched against each folder's and file's own name, at any depth.
type Rules struct {
	Skip    []string
	MaxFile int64
}

func (r Rules) skipName(name string) bool {
	for _, pat := range r.Skip {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// Stats say what taking a snapshot did.
type Stats struct {
	Files, Reused, New int
	NewBytes           int64 // uncompressed size of the content stored this time
}

// scan walks the workspace. With a store, it saves new content; without
// one, it only works out hashes (for review and rewind). A file with the
// same size and time as in prev is taken to be unchanged, which keeps repeat
// snapshots fast; prev is nil when every file must be read in full, because
// a change that keeps a file's size and time exactly would otherwise be
// missed.
func scan(root string, rules Rules, prev map[string]Entry, st *Store) (Snapshot, Stats, error) {
	var snap Snapshot
	var stats Stats
	info, err := os.Stat(root)
	if err != nil {
		return snap, stats, fmt.Errorf("the workspace %s: %w", root, err)
	}
	if !info.IsDir() {
		return snap, stats, fmt.Errorf("the workspace %s is not a folder", root)
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			rel := relSlash(root, p)
			snap.Skipped = append(snap.Skipped, Skipped{rel, "can't be read: " + err.Error()})
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel := relSlash(root, p)
		if checkPath(rel) != nil {
			snap.Skipped = append(snap.Skipped, Skipped{rel, "a name this system can't restore"})
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if rules.skipName(d.Name()) {
			snap.Skipped = append(snap.Skipped, Skipped{rel, "skipped by name"})
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch t := d.Type(); {
		case t.IsDir():
			snap.Dirs = append(snap.Dirs, rel)
			return nil
		case t&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				snap.Skipped = append(snap.Skipped, Skipped{rel, "link can't be read: " + err.Error()})
				return nil
			}
			snap.Files = append(snap.Files, Entry{Path: rel, Link: target})
			stats.Files++
			return nil
		case !t.IsRegular():
			snap.Skipped = append(snap.Skipped, Skipped{rel, "not a regular file"})
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			snap.Skipped = append(snap.Skipped, Skipped{rel, "can't be read: " + err.Error()})
			return nil
		}
		if rules.MaxFile > 0 && fi.Size() > rules.MaxFile {
			snap.Skipped = append(snap.Skipped, Skipped{rel, fmt.Sprintf("larger than %s", Bytes(rules.MaxFile))})
			return nil
		}
		e := Entry{Path: rel, Size: fi.Size(), MTime: fi.ModTime().UnixNano(), Mode: uint32(fi.Mode().Perm())}
		if old, ok := prev[rel]; ok && old.Link == "" && old.Size == e.Size && old.MTime == e.MTime && old.Hash != "" &&
			(st == nil || st.Has(old.Hash)) {
			e.Hash = old.Hash
			stats.Reused++
		} else {
			var n int64
			if st != nil {
				e.Hash, n, err = st.put(p)
			} else {
				e.Hash, err = hashFile(p)
			}
			if err != nil {
				snap.Skipped = append(snap.Skipped, Skipped{rel, "can't be read: " + err.Error()})
				return nil
			}
			if n > 0 {
				stats.New++
				stats.NewBytes += n
			}
		}
		snap.Files = append(snap.Files, e)
		stats.Files++
		return nil
	})
	sort.Slice(snap.Files, func(i, j int) bool { return snap.Files[i].Path < snap.Files[j].Path })
	sort.Strings(snap.Dirs)
	return snap, stats, err
}

func relSlash(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Take snapshots the workspace.
func (s *Store) Take(root, kind, note string, rules Rules) (Snapshot, Stats, error) {
	unlock, err := s.lock()
	if err != nil {
		return Snapshot{}, Stats{}, err
	}
	defer unlock()
	return s.take(root, kind, note, rules)
}

func (s *Store) take(root, kind, note string, rules Rules) (Snapshot, Stats, error) {
	prev := map[string]Entry{}
	if last, err := s.Get(""); err == nil {
		for _, e := range last.Files {
			prev[e.Path] = e
		}
	} else if err != ErrNone {
		return Snapshot{}, Stats{}, err
	}
	snap, stats, err := scan(root, rules, prev, s)
	if err != nil {
		return Snapshot{}, stats, err
	}
	snap.Version, snap.ID, snap.Kind, snap.Note, snap.Workspace = 1, s.newID(), kind, note, root
	snap.Created = s.now().UTC().Format(time.RFC3339)
	if snap.Files == nil {
		snap.Files = []Entry{}
	}
	if snap.Dirs == nil {
		snap.Dirs = []string{}
	}
	return snap, stats, s.write(snap)
}

// Bytes writes a size for people: 512 B, 3.4 KB, 12 MB.
func Bytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}
