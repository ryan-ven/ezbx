// Package snapshot keeps undo points for a box's workspace: ezbx snapshot,
// rewind and review. See docs/snapshots.md.
//
// The store lives in the box's state folder, which is never mounted into the
// box, so the agent can't change its own history. Each file's content is
// kept once, compressed and named by its SHA-256 hash, in objects/. Each
// snapshot is one JSON file in snaps/ listing every path with its size,
// time, mode and hash. The list is written last, through a temporary file,
// so a crash never leaves half a snapshot.
package snapshot

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Kinds of snapshot.
const (
	Auto   = "auto"   // taken by ezbx shell
	Manual = "manual" // taken by ezbx snapshot; kept until removed
	Rewind = "rewind" // taken by ezbx rewind just before it changes anything
)

// Entry is one file or symbolic link in a snapshot.
type Entry struct {
	Path  string `json:"path"` // relative, with forward slashes
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // Unix nanoseconds
	Mode  uint32 `json:"mode"`  // permission bits
	Hash  string `json:"hash,omitempty"`
	Link  string `json:"link,omitempty"` // a symbolic link's target, instead of content
}

// Skipped is something a snapshot left out on purpose, and why.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Snapshot is one undo point.
type Snapshot struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Created   string    `json:"created"` // RFC 3339, UTC
	Kind      string    `json:"kind"`
	Note      string    `json:"note,omitempty"`
	Workspace string    `json:"workspace"`
	Files     []Entry   `json:"files"` // sorted by path
	Dirs      []string  `json:"dirs"`  // every folder, so empty ones come back too
	Skipped   []Skipped `json:"skipped,omitempty"`
}

// Size is the total size of the snapshot's files.
func (s Snapshot) Size() int64 {
	var n int64
	for _, e := range s.Files {
		n += e.Size
	}
	return n
}

// Store is the snapshot folder of one box.
type Store struct {
	Dir string
	Now func() time.Time
}

var (
	idRE   = regexp.MustCompile(`^\d{8}-\d{6}(-\d+)?$`)
	hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Open makes sure the store's folders exist.
func Open(dir string) (*Store, error) {
	for _, d := range []string{"objects", "snaps", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) objectPath(hash string) string {
	return filepath.Join(s.Dir, "objects", hash[:2], hash[2:]+".gz")
}

// Has reports whether the content with this hash is stored.
func (s *Store) Has(hash string) bool {
	_, err := os.Stat(s.objectPath(hash))
	return err == nil
}

// put stores a file's content, compressed, and returns its hash. Content
// that is stored already is not written again.
func (s *Store) put(file string) (hash string, written int64, err error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	tmp, err := os.CreateTemp(filepath.Join(s.Dir, "tmp"), "object-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	zw := gzip.NewWriter(tmp)
	n, err := io.Copy(io.MultiWriter(h, zw), f)
	if err == nil {
		err = zw.Close()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	hash = hex.EncodeToString(h.Sum(nil))
	dst := s.objectPath(hash)
	if s.Has(hash) {
		return hash, 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, err
	}
	return hash, n, nil
}

// Open returns a stored file's content.
func (s *Store) open(hash string) (io.ReadCloser, error) {
	if !hashRE.MatchString(hash) {
		return nil, fmt.Errorf("bad content hash %q in a snapshot", hash)
	}
	f, err := os.Open(s.objectPath(hash))
	if err != nil {
		return nil, fmt.Errorf("the snapshot store is missing content %s: %w", hash[:12], err)
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return readCloser{zr, f}, nil
}

type readCloser struct {
	io.Reader
	f *os.File
}

func (r readCloser) Close() error { return r.f.Close() }

// Content returns a stored file's content, for review's diff.
func (s *Store) Content(hash string) ([]byte, error) {
	r, err := s.open(hash)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// List returns every snapshot, oldest first.
func (s *Store) List() ([]Snapshot, error) {
	names, err := filepath.Glob(filepath.Join(s.Dir, "snaps", "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, n := range names {
		snap, err := s.read(strings.TrimSuffix(filepath.Base(n), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns one snapshot; an empty id means the newest.
func (s *Store) Get(id string) (Snapshot, error) {
	if id == "" {
		all, err := s.List()
		if err != nil {
			return Snapshot{}, err
		}
		if len(all) == 0 {
			return Snapshot{}, ErrNone
		}
		return all[len(all)-1], nil
	}
	if !idRE.MatchString(id) {
		return Snapshot{}, fmt.Errorf("%q is not a snapshot id; ezbx snapshot list shows them", id)
	}
	snap, err := s.read(id)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, fmt.Errorf("no snapshot %s; ezbx snapshot list shows them", id)
	}
	return snap, err
}

// ErrNone means the store has no snapshot yet.
var ErrNone = errors.New("there is no snapshot yet. ezbx snapshot takes one, and every ezbx shell takes one first")

func (s *Store) read(id string) (Snapshot, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, "snaps", id+".json"))
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %s: %w", id, err)
	}
	if snap.Version != 1 || snap.ID != id {
		return Snapshot{}, fmt.Errorf("snapshot %s: unknown format", id)
	}
	for _, e := range snap.Files {
		if err := checkPath(e.Path); err != nil {
			return Snapshot{}, fmt.Errorf("snapshot %s: %w", id, err)
		}
	}
	for _, d := range snap.Dirs {
		if err := checkPath(d); err != nil {
			return Snapshot{}, fmt.Errorf("snapshot %s: %w", id, err)
		}
	}
	return snap, nil
}

// checkPath refuses a path that could reach outside the workspace, so even
// a damaged store can't make rewind write elsewhere.
func checkPath(p string) error {
	bad := p == "" || p == "." || p != path.Clean(p) || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../")
	// On Windows a backslash is a separator and a colon starts a drive or a
	// stream, so either could lead somewhere else.
	if runtime.GOOS == "windows" && strings.ContainsAny(p, `\:`) {
		bad = true
	}
	if bad {
		return fmt.Errorf("refused the path %q: it isn't a plain path inside the workspace", p)
	}
	return nil
}

// write saves a snapshot's list through a temporary file.
func (s *Store) write(snap Snapshot) error {
	data, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, "tmp", snap.ID+".json")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, "snaps", snap.ID+".json"))
}

// newID names a snapshot after the time, made unique if needed.
func (s *Store) newID() string {
	base := s.now().UTC().Format("20060102-150405")
	id := base
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(s.Dir, "snaps", id+".json")); errors.Is(err, os.ErrNotExist) {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
}

// lock keeps two ezbx commands from changing the store at once. A lock left
// by a command that crashed more than an hour ago is taken over.
func (s *Store) lock() (func(), error) {
	file := filepath.Join(s.Dir, "lock")
	for try := 0; try < 2; try++ {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(file) }, nil
		}
		st, serr := os.Stat(file)
		if serr != nil || s.now().Sub(st.ModTime()) < time.Hour {
			return nil, fmt.Errorf("another ezbx command is using the snapshots (%s exists). Wait for it; if none is running, delete that file", file)
		}
		os.Remove(file)
	}
	return nil, errors.New("couldn't lock the snapshot store")
}

// Remove deletes one snapshot's list; Prune then frees content no snapshot
// uses.
func (s *Store) Remove(id string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.Get(id); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.Dir, "snaps", id+".json")); err != nil {
		return err
	}
	_, err = s.collect()
	return err
}

// Prune keeps the newest keep automatic snapshots (rewind's safety
// snapshots count as automatic), removes older ones, and frees content no
// snapshot uses. Manual snapshots are never pruned.
func (s *Store) Prune(keep int) (removed []string, freed int64, err error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	return s.prune(keep)
}

func (s *Store) prune(keep int) ([]string, int64, error) {
	all, err := s.List()
	if err != nil {
		return nil, 0, err
	}
	var auto []Snapshot
	for _, snap := range all {
		if snap.Kind != Manual {
			auto = append(auto, snap)
		}
	}
	var removed []string
	for i := 0; i < len(auto)-keep; i++ {
		if err := os.Remove(filepath.Join(s.Dir, "snaps", auto[i].ID+".json")); err != nil {
			return removed, 0, err
		}
		removed = append(removed, auto[i].ID)
	}
	freed, err := s.collect()
	return removed, freed, err
}

// collect deletes stored content that no snapshot uses.
func (s *Store) collect() (int64, error) {
	all, err := s.List()
	if err != nil {
		return 0, err
	}
	used := map[string]bool{}
	for _, snap := range all {
		for _, e := range snap.Files {
			if e.Hash != "" {
				used[e.Hash] = true
			}
		}
	}
	var freed int64
	root := filepath.Join(s.Dir, "objects")
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		hash := filepath.Base(filepath.Dir(p)) + strings.TrimSuffix(d.Name(), ".gz")
		if used[hash] {
			return nil
		}
		if st, err := d.Info(); err == nil {
			freed += st.Size()
		}
		return os.Remove(p)
	})
	return freed, err
}

// StoreSize is how much disk the stored content takes.
func (s *Store) StoreSize() int64 {
	var n int64
	filepath.WalkDir(filepath.Join(s.Dir, "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if st, err := d.Info(); err == nil {
				n += st.Size()
			}
		}
		return nil
	})
	return n
}
