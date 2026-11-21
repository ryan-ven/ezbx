package snapshot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Changes are the differences between a snapshot and a later state.
type Changes struct {
	Added, Changed, Deleted []string // file and link paths, sorted
}

// Empty reports whether nothing differs.
func (c Changes) Empty() bool { return len(c.Added)+len(c.Changed)+len(c.Deleted) == 0 }

// Count is the number of changed paths.
func (c Changes) Count() int { return len(c.Added) + len(c.Changed) + len(c.Deleted) }

// protected reports whether a path was left out of the old snapshot (skipped
// by name or size, unreadable), or lies inside such a folder. Those paths
// are never compared, and rewind never touches them.
func protected(old Snapshot, rel string) bool {
	for _, sk := range old.Skipped {
		if rel == sk.Path || strings.HasPrefix(rel, sk.Path+"/") {
			return true
		}
	}
	return false
}

// Compare lists what differs between an old snapshot and a newer state.
// Paths the old snapshot left out are not compared.
func Compare(old, cur Snapshot) Changes {
	var c Changes
	before := map[string]Entry{}
	for _, e := range old.Files {
		before[e.Path] = e
	}
	now := map[string]bool{}
	for _, e := range cur.Files {
		now[e.Path] = true
		o, ok := before[e.Path]
		switch {
		case !ok:
			if !protected(old, e.Path) {
				c.Added = append(c.Added, e.Path)
			}
		case o.Link != e.Link || o.Hash != e.Hash:
			c.Changed = append(c.Changed, e.Path)
		}
	}
	for _, e := range old.Files {
		if !now[e.Path] && !protected(cur, e.Path) {
			c.Deleted = append(c.Deleted, e.Path)
		}
	}
	sort.Strings(c.Added)
	sort.Strings(c.Changed)
	sort.Strings(c.Deleted)
	return c
}

// Current reads the workspace as it is now, reading every file in full, so
// a change that kept a file's size and time is still seen.
func Current(root string, rules Rules) (Snapshot, error) {
	snap, _, err := scan(root, rules, nil, nil)
	return snap, err
}

// Result is what a rewind did.
type Result struct {
	Safety   Snapshot // taken first, so the rewind can itself be undone
	Changes  Changes  // what was put back
	Problems []string // paths that couldn't be put back, with why
}

// Rewind makes the workspace match the target snapshot: changed and deleted
// files come back, and files made since are removed. First it takes a full
// snapshot of the workspace as it is (kind "rewind"), so nothing is lost and
// the rewind can be undone by rewinding to that one. Paths the target left
// out (skipped folders, large files) are left alone.
func (s *Store) Rewind(root string, target Snapshot, rules Rules) (Result, error) {
	unlock, err := s.lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	// The safety snapshot reads every file in full and stores it.
	safety, _, err := scan(root, rules, nil, s)
	if err != nil {
		return Result{}, fmt.Errorf("the safety snapshot failed, so nothing was changed: %w", err)
	}
	safety.Version, safety.ID, safety.Kind, safety.Workspace = 1, s.newID(), Rewind, root
	safety.Note = "before rewinding to " + target.ID
	safety.Created = s.now().UTC().Format(time.RFC3339)
	if safety.Files == nil {
		safety.Files = []Entry{}
	}
	if safety.Dirs == nil {
		safety.Dirs = []string{}
	}
	if err := s.write(safety); err != nil {
		return Result{}, fmt.Errorf("the safety snapshot failed, so nothing was changed: %w", err)
	}
	res := Result{Safety: safety, Changes: Compare(target, safety)}
	problem := func(p string, err error) { res.Problems = append(res.Problems, p+": "+err.Error()) }

	entries := map[string]Entry{}
	for _, e := range target.Files {
		entries[e.Path] = e
	}

	// 1. Remove what was made since, deepest first.
	added := append([]string(nil), res.Changes.Added...)
	sort.Sort(sort.Reverse(sort.StringSlice(added)))
	for _, p := range added {
		full, err := inside(root, p)
		if err == nil {
			err = os.Remove(full)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			problem(p, err)
		}
	}
	// 2. Remove folders made since, if they are empty now.
	targetDirs := map[string]bool{}
	for _, d := range target.Dirs {
		targetDirs[d] = true
	}
	newDirs := []string{}
	for _, d := range safety.Dirs {
		if !targetDirs[d] && !protected(target, d) {
			newDirs = append(newDirs, d)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(newDirs)))
	for _, d := range newDirs {
		if full, err := inside(root, d); err == nil {
			os.Remove(full) // fails, harmlessly, if something is still in it
		}
	}
	// 3. Make the target's folders.
	for _, d := range target.Dirs {
		full, err := inside(root, d)
		if err == nil {
			err = os.MkdirAll(full, 0o755)
		}
		if err != nil {
			problem(d+"/", err)
		}
	}
	// 4. Put back changed and deleted files.
	back := append(append([]string(nil), res.Changes.Changed...), res.Changes.Deleted...)
	sort.Strings(back)
	for _, p := range back {
		if err := s.restore(root, entries[p]); err != nil {
			problem(p, err)
		}
	}
	return res, nil
}

// inside joins a snapshot path to the workspace, refusing any path that
// could lead out of it: a bad path, or a folder on the way that is a link
// (an agent could plant a link so that a rewind writes somewhere else).
func inside(root, rel string) (string, error) {
	if err := checkPath(rel); err != nil {
		return "", err
	}
	dir := root
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			break // made fresh below, so it can't be a link
		}
		if err != nil {
			return "", err
		}
		if !fi.IsDir() || fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return "", fmt.Errorf("refused: %s is not a plain folder (a link could lead outside the workspace)", filepath.ToSlash(strings.TrimPrefix(dir, root)))
		}
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// restore writes one file or link back, through a temporary file in the
// same folder, then sets its mode and time.
func (s *Store) restore(root string, e Entry) error {
	full, err := inside(root, e.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	// Check again now that the folders exist.
	if full, err = inside(root, e.Path); err != nil {
		return err
	}
	if fi, err := os.Lstat(full); err == nil && fi.IsDir() {
		return errors.New("a folder is in the way; move it, then rewind again")
	}
	if e.Link != "" {
		os.Remove(full)
		return os.Symlink(e.Link, full)
	}
	r, err := s.open(e.Hash)
	if err != nil {
		return err
	}
	defer r.Close()
	tmp, err := os.CreateTemp(filepath.Dir(full), ".ezbx-rewind-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && e.Mode != 0 {
		err = os.Chmod(tmp.Name(), fs.FileMode(e.Mode))
	}
	if err == nil {
		os.Remove(full) // a link in the way is replaced, never followed
		err = os.Rename(tmp.Name(), full)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	t := time.Unix(0, e.MTime)
	return os.Chtimes(full, t, t)
}
