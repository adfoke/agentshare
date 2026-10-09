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

// Path returns the on-disk path of a digest. It does not touch the filesystem.
func (s *Store) Path(sum string) string {
	sum = normalize(sum)
	if len(sum) < 4 {
		return filepath.Join(s.root, sum)
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
	if len(sum) != 64 {
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
	f, err := os.Open(s.Path(sum))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Stat reports a blob's size.
func (s *Store) Stat(sum string) (int64, error) {
	fi, err := os.Stat(s.Path(sum))
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
	fi, err := os.Stat(s.Path(sum))
	return err == nil && fi.Mode().IsRegular()
}

// Remove deletes one blob. A missing blob is not an error.
func (s *Store) Remove(sum string) error {
	err := os.Remove(s.Path(sum))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Walk visits every stored blob. mk is called with the digest of each entry.
func (s *Store) Walk(fn func(sum string, size int64) error) error {
	return filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// <root>/ab/cd/<digest>
		sum := filepath.Base(path)
		if len(sum) != 64 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		return fn(sum, fi.Size())
	})
}

// GC removes every blob for which referenced returns false. It is deliberately
// a full sweep rather than reference counting: it also cleans up blobs left
// behind by a crash, and it runs rarely enough that the cost does not matter.
func (s *Store) GC(referenced func(sum string) bool) (removed int, freed int64, err error) {
	err = s.Walk(func(sum string, size int64) error {
		if referenced(sum) {
			return nil
		}
		if rmErr := s.Remove(sum); rmErr != nil {
			return rmErr
		}
		removed++
		freed += size
		return nil
	})
	return removed, freed, err
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
	err := s.Walk(func(_ string, size int64) error {
		total += size
		return nil
	})
	return total, err
}
