package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func file(id, sha, name string, created int64) *File {
	return &File{
		ID: id, Sha256: sha, Size: 10, Name: name,
		ContentType: "text/plain", Owner: "root", CreatedAt: created, Meta: "{}",
	}
}

func TestFileLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	f := file("f_1", "aa", "one.txt", 1000)
	if err := s.InsertFile(ctx, f); err != nil {
		t.Fatal(err)
	}

	got, err := s.FileByID(ctx, "f_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "one.txt" || got.Sha256 != "aa" || got.ExpiresAt != nil {
		t.Fatalf("round trip lost data: %+v", got)
	}

	if _, err := s.FileByID(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v, want ErrNotFound", err)
	}
}

// Two records can share one content. Deleting either must report that the bytes
// are still referenced until the last one goes — that is the whole reason blobs
// and records are separate tables. Acting on the count is not this call's job;
// it only reports it, for the reasons on DeleteFile.
func TestDeleteReportsRemainingReferences(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.InsertFile(ctx, file("f_1", "same", "a.txt", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertFile(ctx, file("f_2", "same", "b.txt", 1001)); err != nil {
		t.Fatal(err)
	}

	res, err := s.DeleteFile(ctx, "f_1")
	if err != nil {
		t.Fatal(err)
	}
	if res.References != 1 {
		t.Fatalf("References = %d after deleting one of two records, want 1", res.References)
	}

	res, err = s.DeleteFile(ctx, "f_2")
	if err != nil {
		t.Fatal(err)
	}
	if res.References != 0 {
		t.Fatalf("References = %d after the last record went, want 0", res.References)
	}
	if _, err := s.DeleteFile(ctx, "f_2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestListFilesPaging(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for i := 0; i < 5; i++ {
		if err := s.InsertFile(ctx, file(
			[]string{"f_0", "f_1", "f_2", "f_3", "f_4"}[i], "sha", "n", int64(1000+i))); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		files, next, err := s.ListFiles(ctx, ListFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, f := range files {
			if seen[f.ID] {
				t.Fatalf("id %s appeared on two pages", f.ID)
			}
			seen[f.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
		if pages > 5 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d files over %d pages, want 5", len(seen), pages)
	}
	if pages != 3 {
		t.Fatalf("paged in %d pages, want 3", pages)
	}

	// Newest first by default.
	files, _, err := s.ListFiles(ctx, ListFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != "f_4" {
		t.Fatalf("default order is not newest-first: %+v", files)
	}

	if _, _, err := s.ListFiles(ctx, ListFilter{Cursor: "!!not-base64!!"}); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("bad cursor = %v, want ErrBadCursor", err)
	}
}

func TestExpiredFilesAreHiddenUnlessAsked(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	past := time.Now().Add(-time.Hour).Unix()
	f := file("f_old", "aa", "old.txt", 1000)
	f.ExpiresAt = &past
	if err := s.InsertFile(ctx, f); err != nil {
		t.Fatal(err)
	}

	files, _, err := s.ListFiles(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("expired file should be hidden, got %d", len(files))
	}

	files, _, err = s.ListFiles(ctx, ListFilter{Include: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("include_expired should reveal it, got %d", len(files))
	}

	if got, err := s.FileByID(ctx, "f_old"); err != nil || !got.Expired(time.Now()) {
		t.Fatalf("Expired() = %v, %v", got, err)
	}
}

func TestShareDownloadBudget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.InsertFile(ctx, file("f_1", "aa", "a.txt", 1000)); err != nil {
		t.Fatal(err)
	}

	max := int64(2)
	sh := &Share{ID: "s_1", FileID: "f_1", CreatedAt: 1000, MaxDownloads: &max}
	if err := s.InsertShare(ctx, sh); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		ok, err := s.ReserveShareDownload(ctx, "s_1")
		if err != nil || !ok {
			t.Fatalf("download %d: ok=%v err=%v, want true nil", i+1, ok, err)
		}
	}
	ok, err := s.ReserveShareDownload(ctx, "s_1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("third download must be refused: the budget was 2")
	}

	got, err := s.ShareByID(ctx, "s_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Downloads != 2 || !got.Exhausted() {
		t.Fatalf("share state %+v, want 2 downloads and exhausted", got)
	}

	// An unlimited share never refuses.
	unlimited := &Share{ID: "s_2", FileID: "f_1", CreatedAt: 1000}
	if err := s.InsertShare(ctx, unlimited); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		ok, err := s.ReserveShareDownload(ctx, "s_2")
		if err != nil || !ok {
			t.Fatalf("unlimited download %d refused: ok=%v err=%v", i+1, ok, err)
		}
	}
}

func TestDeleteFileCascadesToShares(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.InsertFile(ctx, file("f_1", "aa", "a.txt", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertShare(ctx, &Share{ID: "s_1", FileID: "f_1", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteFile(ctx, "f_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ShareByID(ctx, "s_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("share survived its file: %v", err)
	}
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	tok := &Token{ID: "t_1", Name: "ci", Scopes: []string{"upload", "share"}, CreatedAt: 1000}
	if err := s.InsertToken(ctx, tok, TokenHash("ash_secret")); err != nil {
		t.Fatal(err)
	}

	got, err := s.TokenBySecret(ctx, "ash_secret")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "t_1" || len(got.Scopes) != 2 || got.Scopes[0] != "upload" {
		t.Fatalf("token round trip: %+v", got)
	}
	if _, err := s.TokenBySecret(ctx, "ash_wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong secret = %v, want ErrNotFound", err)
	}

	s.TouchToken(ctx, "t_1")
	got, _ = s.TokenBySecret(ctx, "ash_secret")
	if got.LastUsed == nil {
		t.Fatal("TouchToken did not record a time")
	}

	if err := s.DeleteToken(ctx, "t_1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteToken(ctx, "t_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestSweepExpired(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	past := time.Now().Add(-time.Minute).Unix()
	live := time.Now().Add(time.Hour).Unix()

	expired := file("f_exp", "aa", "dead.txt", 1000)
	expired.ExpiresAt = &past
	alive := file("f_live", "bb", "live.txt", 1000)
	alive.ExpiresAt = &live
	for _, f := range []*File{expired, alive} {
		if err := s.InsertFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertShare(ctx, &Share{ID: "s_exp", FileID: "f_live", CreatedAt: 1000, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertToken(ctx, &Token{ID: "t_exp", CreatedAt: 1000, ExpiresAt: &past}, "hash"); err != nil {
		t.Fatal(err)
	}

	res, err := s.SweepExpired(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Shares != 1 || res.Tokens != 1 {
		t.Fatalf("sweep removed %+v, want 1 of each", res)
	}
	if _, err := s.FileByID(ctx, "f_exp"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired file survived the sweep")
	}
	if _, err := s.FileByID(ctx, "f_live"); err != nil {
		t.Fatal("live file was swept")
	}
}

func TestStatsAndDigests(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.InsertFile(ctx, file("f_1", "aa", "a.txt", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertFile(ctx, file("f_2", "aa", "b.txt", 1001)); err != nil {
		t.Fatal(err)
	}

	st, err := s.StatsFor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.FilesBytes != 20 {
		t.Fatalf("stats %+v, want 2 files and 20 bytes", st)
	}

	digests, err := s.AllDigests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != 1 {
		t.Fatalf("AllDigests returned %d, want 1 distinct digest", len(digests))
	}
}

func TestSearchFilters(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	f := file("f_1", "cc", "build-report.log", 2000)
	f.Tags = []string{"build", "nightly"}
	if err := s.InsertFile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertFile(ctx, file("f_2", "dd", "other.txt", 3000)); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		filter ListFilter
		want   string
	}{
		{"by name substring", ListFilter{Query: "report"}, "f_1"},
		{"by tag", ListFilter{Tag: "nightly"}, "f_1"},
		{"by exact sha", ListFilter{Query: "dd"}, "f_2"},
		{"by exact id", ListFilter{Query: "f_1"}, "f_1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, _, err := s.ListFiles(ctx, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 1 || files[0].ID != tc.want {
				t.Fatalf("got %d rows %v, want exactly %s", len(files), ids(files), tc.want)
			}
		})
	}

	// A LIKE wildcard in the query must be treated as a literal character, not
	// as a pattern; otherwise "%" would match everything.
	files, _, err := s.ListFiles(ctx, ListFilter{Query: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("a literal %% matched %d rows; the LIKE escape is not working", len(files))
	}
}

func ids(files []*File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.ID)
	}
	return out
}
