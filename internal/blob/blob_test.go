package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func write(t *testing.T, s *Store, content []byte) (sum string, existed bool) {
	t.Helper()
	f, err := s.Temp()
	if err != nil {
		t.Fatalf("Temp: %v", err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	sum = digest(content)
	existed, err = s.Commit(f.Name(), sum)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return sum, existed
}

func TestPutDedupAndRead(t *testing.T) {
	s := newStore(t)
	content := []byte("hello agentshare")

	sum, existed := write(t, s, content)
	if existed {
		t.Fatal("first write should not report an existing blob")
	}
	if !s.Exists(sum) {
		t.Fatal("blob should exist after Commit")
	}

	// The sharding has to be derived from the digest, so the path is a pure
	// function of the content.
	want := filepath.Join(s.Root(), sum[0:2], sum[2:4], sum)
	if got := s.Path(sum); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}

	// Second write of identical bytes must dedup and leave no temp file behind.
	sum2, existed := write(t, s, content)
	if sum2 != sum || !existed {
		t.Fatalf("second write: sum=%s existed=%v, want %s true", sum2, existed, sum)
	}
	entries, err := os.ReadDir(s.Root() + "/../tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp dir should be empty after a dedup, has %d entries", len(entries))
	}

	f, err := s.Open(sum)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("read back %q, want %q", got, content)
	}

	size, err := s.Stat(sum)
	if err != nil || size != int64(len(content)) {
		t.Fatalf("Stat = %d, %v; want %d, nil", size, err, len(content))
	}
}

func TestOpenMissing(t *testing.T) {
	s := newStore(t)
	if _, err := s.Open(digest([]byte("nope"))); err != ErrNotFound {
		t.Fatalf("Open missing = %v, want ErrNotFound", err)
	}
	if err := s.Remove(digest([]byte("nope"))); err != nil {
		t.Fatalf("Remove missing should be a no-op, got %v", err)
	}
}

func TestCommitRejectsBadDigest(t *testing.T) {
	s := newStore(t)
	f, _ := s.Temp()
	if _, err := s.Commit(f.Name(), "not-a-digest"); err == nil {
		t.Fatal("Commit should refuse a malformed digest")
	}
	_ = os.Remove(f.Name())
}

func TestGC(t *testing.T) {
	s := newStore(t)
	keep, _ := write(t, s, []byte("keep me"))
	drop, _ := write(t, s, []byte("drop me"))

	// A zero cutoff means no grace period, which is what a test or a forced
	// sweep on a quiet store wants.
	res, err := s.GC(func(sum string) bool { return sum == keep }, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || res.Freed != int64(len("drop me")) || res.Skipped != 0 {
		t.Fatalf("GC = %+v, want 1 removed, %d freed, 0 skipped", res, len("drop me"))
	}
	if !s.Exists(keep) || s.Exists(drop) {
		t.Fatal("GC kept the wrong blobs")
	}
}

// The sweep decides from a snapshot of the referenced digests that it read
// before walking. A blob committed after that snapshot is absent from it while
// being very much referenced — its metadata row exists — so reclaiming it would
// strand the record and lose the upload permanently. The age test is what makes
// that safe, and this is the test that keeps it there.
func TestGCLeavesRecentlyWrittenBlobsAlone(t *testing.T) {
	s := newStore(t)

	oldSum, _ := write(t, s, []byte("written long ago"))
	recentSum, _ := write(t, s, []byte("written just now"))

	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(s.Path(oldSum), past, past); err != nil {
		t.Fatal(err)
	}

	res, err := s.GC(func(string) bool { return false }, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || res.Skipped != 1 {
		t.Fatalf("GC = %+v, want 1 removed and 1 skipped", res)
	}
	if s.Exists(oldSum) {
		t.Error("an unreferenced blob older than the cutoff should have been reclaimed")
	}
	if !s.Exists(recentSum) {
		t.Error("a blob written after the cutoff was reclaimed; " +
			"a concurrent upload of that content could have lost it")
	}

	// With no grace period the same sweep takes it, which is what makes the
	// skipped count above meaningful rather than incidental.
	res, err = s.GC(func(string) bool { return false }, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || res.Skipped != 0 {
		t.Fatalf("no-grace GC = %+v, want 1 removed and 0 skipped", res)
	}
	if s.Exists(recentSum) {
		t.Error("the recent blob survived a sweep that was told not to spare it")
	}
}

// Path is the only place in this program where a string becomes a filesystem
// path, which makes it the last line of defence against a traversal. Nothing
// that is not a digest names a blob.
func TestNothingButADigestNamesABlob(t *testing.T) {
	s := newStore(t)

	for _, bad := range []string{
		"",
		"..",
		"../../etc/passwd",
		"../../../root/.ssh/authorized_keys",
		"/etc/passwd",
		"ab/cd",
		"abcdef",                      // too short
		strings.Repeat("a", 65),       // too long
		strings.Repeat("g", 64),       // not hex
		strings.Repeat("a", 63) + "/", // right length, wrong alphabet
		"./../blobs",
	} {
		if got := s.Path(bad); got != "" {
			t.Errorf("Path(%q) = %q, want empty: a traversal must not be addressable", bad, got)
		}
		if _, err := s.Open(bad); err != ErrNotFound {
			t.Errorf("Open(%q) = %v, want ErrNotFound", bad, err)
		}
		if _, err := s.Stat(bad); err != ErrNotFound {
			t.Errorf("Stat(%q) = %v, want ErrNotFound", bad, err)
		}
		if s.Exists(bad) {
			t.Errorf("Exists(%q) = true", bad)
		}
		if err := s.Remove(bad); err != ErrNotFound {
			t.Errorf("Remove(%q) = %v, want ErrNotFound", bad, err)
		}
		if _, err := s.Commit("/nonexistent", bad); err == nil {
			t.Errorf("Commit(%q) accepted a non-digest", bad)
		}
	}

	// An uppercase digest is the same digest, not a different one: it is
	// normalised before it is validated, and still names a real path.
	upper := strings.ToUpper(strings.Repeat("ab", 32))
	got := s.Path(upper)
	if got == "" || !strings.HasSuffix(got, strings.Repeat("ab", 32)) {
		t.Errorf("Path(upper digest) = %q, want the lowercased blob path", got)
	}
}

func TestCleanTempOnlyRemovesOldFiles(t *testing.T) {
	s := newStore(t)
	old, _ := s.Temp()
	fresh, _ := s.Temp()
	_ = old.Close()
	_ = fresh.Close()

	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old.Name(), past, past); err != nil {
		t.Fatal(err)
	}

	n, err := s.CleanTemp(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cleaned %d files, want 1", n)
	}
	if _, err := os.Stat(fresh.Name()); err != nil {
		t.Fatal("the fresh temp file should have survived")
	}
}
