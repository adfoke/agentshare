package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
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

	removed, freed, err := s.GC(func(sum string) bool { return sum == keep })
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || freed != int64(len("drop me")) {
		t.Fatalf("GC removed=%d freed=%d, want 1 and %d", removed, freed, len("drop me"))
	}
	if !s.Exists(keep) || s.Exists(drop) {
		t.Fatal("GC kept the wrong blobs")
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
