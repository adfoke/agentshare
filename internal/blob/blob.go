// Package blob implements the on-disk content-addressed store.
//
// A blob is a file's bytes, stored once under its SHA-256:
//
//	<root>/ab/cd/abcdef...      (first 2 hex digits / next 2 / full digest)
//
// Two uploads of identical bytes share one blob; the metadata database decides
// how many logical files point at it. Nothing here ever overwrites a blob that
// already exists, so a blob is immutable once written — which is what makes the
// digest safe to hand out as a cache key.
package blob

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotFound is returned when a digest has no blob on disk.
var ErrNotFound = errors.New("blob not found")

// digestLen is the only shape a blob's name ever takes.
const digestLen = 64

// validDigest reports whether sum names a blob: 64 lowercase hex characters.
//
// This is checked on the way in rather than trusted from the caller because
// Path is the one place in this program where a string becomes a filesystem
// path. Today every caller validates first — the digest routes check the hex at
// the edge, and everything else comes out of the database — but a store whose
// job is "map a string to a file" should not be the component that turns a
// "../../.." into a path traversal because a future caller forgot.
func validDigest(sum string) bool {
	if len(sum) != digestLen {
		return false
	}
	for i := 0; i < len(sum); i++ {
		switch c := sum[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// Store owns <root>/blobs and <root>/tmp.
type Store struct {
	root string
	tmp  string
}

// Open prepares the directory layout under root.
func Open(root string) (*Store, error) {
	s := &Store{
		root: filepath.Join(root, "blobs"),
		tmp:  filepath.Join(root, "tmp"),
	}
	for _, d := range []string{s.root, s.tmp} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Root is the directory holding all blobs.
func (s *Store) Root() string { return s.root }

// Temp creates a file in the staging directory. The caller writes the upload
// into it and then either Commits or Discards it.
func (s *Store) Temp() (*os.File, error) {
	return os.CreateTemp(s.tmp, "up-*")
}

// Path returns the on-disk path of a digest, or "" if sum does not name a
// blob. It does not touch the filesystem.
//
// The empty return is deliberate: there is no path that could be confused for
// a real blob, so a caller that ignores the check cannot accidentally be handed
// a traversable path. Every accessor below tests for it.
func (s *Store) Path(sum string) string {
	sum = normalize(sum)
	if !validDigest(sum) {
		return ""
	}
	return filepath.Join(s.root, sum[0:2], sum[2:4], sum)
}

func normalize(sum string) string { return strings.ToLower(strings.TrimSpace(sum)) }

// Commit moves a fully written temp file into place.
//
// existed reports that identical content was already stored (dedup) — the temp
// file is removed in that case. The move is a rename within one filesystem, so
// readers never observe a partially written blob.
func (s *Store) Commit(tmpPath, sum string) (existed bool, err error) {
	sum = normalize(sum)
	if !validDigest(sum) {
		return false, fmt.Errorf("invalid sha256 %q", sum)
	}
	final := s.Path(sum)

	if fi, statErr := os.Stat(final); statErr == nil && fi.Mode().IsRegular() {
		_ = os.Remove(tmpPath)
		return true, nil
	} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return false, statErr
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return false, err
	}
	// Sync the payload before the rename so a crash cannot leave a
	// zero-length blob under a valid digest.
	if f, openErr := os.Open(tmpPath); openErr == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmpPath, final); err != nil {
		// A concurrent upload may have won the race; if the content is there,
		// that is a success, not a failure.
		if fi, statErr := os.Stat(final); statErr == nil && fi.Mode().IsRegular() {
			_ = os.Remove(tmpPath)
			return true, nil
		}
		return false, err
	}
	_ = os.Chmod(final, 0o640)
	return false, nil
}

// Open returns the blob for reading. The caller closes it.
func (s *Store) Open(sum string) (*os.File, error) {
	path := s.Path(sum)
	if path == "" {
		return nil, ErrNotFound
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Stat reports a blob's size.
func (s *Store) Stat(sum string) (int64, error) {
	path := s.Path(sum)
	if path == "" {
		return 0, ErrNotFound
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Exists reports whether the digest is stored.
func (s *Store) Exists(sum string) bool {
	path := s.Path(sum)
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// Remove deletes one blob. A missing blob is not an error.
func (s *Store) Remove(sum string) error {
	path := s.Path(sum)
	if path == "" {
		return ErrNotFound
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Walk visits every stored blob, reporting each one's digest, size and
// modification time. The modification time is what GC uses to leave recent
// writes alone; see GC.
func (s *Store) Walk(fn func(sum string, size int64, mod time.Time) error) error {
	return filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// <root>/ab/cd/<digest>
		sum := filepath.Base(path)
		if !validDigest(sum) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		return fn(sum, fi.Size(), fi.ModTime())
	})
}

// GCResult reports what a sweep did, including what it deliberately left alone.
type GCResult struct {
	Removed int
	Freed   int64
	// Skipped counts blobs that are unreferenced but were written too recently
	// to touch. A non-zero value here is normal and healthy: it means uploads
	// were in flight while the sweep ran.
	Skipped int
}

// GC removes every blob that is unreferenced and older than untouchedSince.
//
// It is deliberately a full sweep rather than reference counting: it also
// cleans up blobs left behind by a crash, and it runs rarely enough that the
// cost does not matter.
//
// The age test is what makes the sweep safe. Callers pass a snapshot of the
// referenced digests taken *before* the walk, so a blob committed after that
// snapshot is missing from it while being very much referenced — deleting it
// would strand its metadata row and lose the upload for good. Committing a blob
// and inserting its row happen microseconds apart, so refusing to touch
// anything written in the last few minutes removes the race entirely, at the
// cost of reclaiming recent deletions a little later than it otherwise would.
//
// A zero untouchedSince means "no grace period": delete everything
// unreferenced. That is right for a test and for an operator forcing a sweep on
// a quiet store; it is not what the janitor passes.
func (s *Store) GC(referenced func(sum string) bool, untouchedSince time.Time) (GCResult, error) {
	var res GCResult
	err := s.Walk(func(sum string, size int64, mod time.Time) error {
		if referenced(sum) {
			return nil
		}
		if !untouchedSince.IsZero() && mod.After(untouchedSince) {
			res.Skipped++
			return nil
		}
		if rmErr := s.Remove(sum); rmErr != nil {
			return rmErr
		}
		res.Removed++
		res.Freed += size
		return nil
	})
	return res, err
}

// CleanTemp removes staging files left over by an interrupted upload.
func (s *Store) CleanTemp(olderThan time.Time) (removed int, err error) {
	entries, err := os.ReadDir(s.tmp)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || fi.IsDir() {
			continue
		}
		if fi.ModTime().Before(olderThan) {
			if rmErr := os.Remove(filepath.Join(s.tmp, e.Name())); rmErr == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// TotalBytes reports how much disk the blob store occupies.
func (s *Store) TotalBytes() (int64, error) {
	var total int64
	err := s.Walk(func(_ string, size int64, _ time.Time) error {
		total += size
		return nil
	})
	return total, err
}
