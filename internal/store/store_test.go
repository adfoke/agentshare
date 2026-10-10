package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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
	sh := &Share{ID: "s_1", CreatedAt: 1000, MaxDownloads: &max}
	if err := s.InsertShare(ctx, sh, []string{"f_1"}); err != nil {
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
	unlimited := &Share{ID: "s_2", CreatedAt: 1000}
	if err := s.InsertShare(ctx, unlimited, []string{"f_1"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		ok, err := s.ReserveShareDownload(ctx, "s_2")
		if err != nil || !ok {
			t.Fatalf("unlimited download %d refused: ok=%v err=%v", i+1, ok, err)
		}
	}
}

// Deleting a file takes it out of every share that listed it, and leaves the
// share itself alone. The share has to survive because it may cover other files
// — and even a share of one should answer "this link no longer has any files"
// rather than "no such link", which would suggest the recipient has the wrong
// URL.
func TestDeletingAFileRemovesItFromItsShares(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, f := range []*File{file("f_1", "aa", "a.txt", 1000), file("f_2", "bb", "b.txt", 1001)} {
		if err := s.InsertFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertShare(ctx, &Share{ID: "s_1", CreatedAt: 1000}, []string{"f_1", "f_2"}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DeleteFile(ctx, "f_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ShareByID(ctx, "s_1"); err != nil {
		t.Fatalf("the share should outlive one of its files: %v", err)
	}
	items, err := s.ShareItems(ctx, "s_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "f_2" {
		t.Fatalf("after deleting f_1 the share holds %d items, want just f_2", len(items))
	}

	// The share survives losing its last file too; it simply resolves to none.
	if _, err := s.DeleteFile(ctx, "f_2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ShareByID(ctx, "s_1"); err != nil {
		t.Fatalf("the share should still exist with no files: %v", err)
	}
	if items, err := s.ShareItems(ctx, "s_1"); err != nil || len(items) != 0 {
		t.Fatalf("ShareItems = %d items, %v; want 0, nil", len(items), err)
	}
}

func TestShareItemsKeepsOrderAndSkipsExpired(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	past := time.Now().Add(-time.Hour).Unix()
	for _, f := range []*File{
		file("f_c", "cc", "c.txt", 1000),
		file("f_a", "aa", "a.txt", 1000),
		file("f_b", "bb", "b.txt", 1000),
	} {
		if err := s.InsertFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	dead := file("f_dead", "dd", "dead.txt", 1000)
	dead.ExpiresAt = &past
	if err := s.InsertFile(ctx, dead); err != nil {
		t.Fatal(err)
	}

	// Deliberately not alphabetical: the index page must list them in the order
	// the caller named, which is the order they care about.
	order := []string{"f_c", "f_a", "f_b", "f_dead"}
	if err := s.InsertShare(ctx, &Share{ID: "s_1", CreatedAt: 1000}, order); err != nil {
		t.Fatal(err)
	}

	items, err := s.ShareItems(ctx, "s_1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range items {
		got = append(got, f.ID)
	}
	want := "f_c,f_a,f_b"
	if joined := strings.Join(got, ","); joined != want {
		t.Fatalf("ShareItems = %s, want %s (insertion order, expired left out)", joined, want)
	}

	// ItemCount still counts what was shared, which is what lets the index page
	// say how many files are hidden.
	sh, err := s.ShareByID(ctx, "s_1")
	if err != nil {
		t.Fatal(err)
	}
	if sh.ItemCount != 4 {
		t.Fatalf("ItemCount = %d, want 4", sh.ItemCount)
	}
	if !s.shareCovers(t, "s_1", "f_dead") {
		t.Fatal("ShareCovers should answer for every member, expired or not")
	}
}

// shareCovers is a tiny helper so the assertion above reads as one line.
func (s *Store) shareCovers(t *testing.T, shareID, fileID string) bool {
	t.Helper()
	ok, err := s.ShareCovers(context.Background(), shareID, fileID)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestShareRefusesToCoverNothing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.InsertShare(ctx, &Share{ID: "s_1", CreatedAt: 1000}, nil); err == nil {
		t.Fatal("a share with no files was accepted; it would be a link to nothing")
	}
}

// The deployed database was created before a share could hold more than one
// file, so its shares table has file_id NOT NULL and nothing records membership
// anywhere else. Opening that file has to move the data, and this is the test
// that says so — the alternative is finding out on the server.
func TestMigrationMovesSingleFileSharesToShareItems(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Build the old shape by hand, exactly as the previous release created it.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE files (
			id TEXT PRIMARY KEY, sha256 TEXT NOT NULL, size INTEGER NOT NULL,
			name TEXT NOT NULL DEFAULT '', content_type TEXT NOT NULL DEFAULT '',
			tags TEXT NOT NULL DEFAULT '', meta TEXT NOT NULL DEFAULT '{}',
			owner TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
			expires_at INTEGER, downloads INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE shares (
			id TEXT PRIMARY KEY,
			file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
			label TEXT NOT NULL DEFAULT '', owner TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, expires_at INTEGER,
			max_downloads INTEGER, downloads INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO files (id, sha256, size, name, content_type, created_at) VALUES
			('f_1','aa',10,'one.txt','text/plain',1000),
			('f_2','bb',20,'two.txt','text/plain',1000)`,
		`INSERT INTO shares (id, file_id, label, created_at, max_downloads) VALUES
			('s_1','f_1','legacy label',1000,5),
			('s_2','f_2','',1001,NULL)`,
		`PRAGMA user_version = 0`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("building the legacy schema: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening with this build is what migrates it.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a legacy database: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	for _, tc := range []struct{ share, fileID, label string }{
		{"s_1", "f_1", "legacy label"},
		{"s_2", "f_2", ""},
	} {
		sh, err := st.ShareByID(ctx, tc.share)
		if err != nil {
			t.Fatalf("%s did not survive the migration: %v", tc.share, err)
		}
		if sh.Label != tc.label {
			t.Errorf("%s label = %q, want %q", tc.share, sh.Label, tc.label)
		}
		if sh.ItemCount != 1 {
			t.Errorf("%s item count = %d, want 1", tc.share, sh.ItemCount)
		}
		items, err := st.ShareItems(ctx, tc.share)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].ID != tc.fileID {
			t.Errorf("%s covers %v, want exactly %s", tc.share, items, tc.fileID)
		}
	}

	// The single-file share kept its download budget, which lived in a column
	// the rebuild had to carry across.
	if sh, _ := st.ShareByID(ctx, "s_1"); sh.MaxDownloads == nil || *sh.MaxDownloads != 5 {
		t.Errorf("max_downloads did not survive: %+v", sh.MaxDownloads)
	}

	// And the new capability works on the migrated database.
	if err := st.InsertShare(ctx, &Share{ID: "s_3", CreatedAt: 2000},
		[]string{"f_1", "f_2"}); err != nil {
		t.Fatal(err)
	}
	if items, err := st.ShareItems(ctx, "s_3"); err != nil || len(items) != 2 {
		t.Fatalf("a multi-file share on a migrated database holds %d items, %v", len(items), err)
	}

	// Re-opening must be a no-op rather than migrating twice.
	st.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("re-opening the migrated database: %v", err)
	}
	defer again.Close()
	if items, err := again.ShareItems(ctx, "s_3"); err != nil || len(items) != 2 {
		t.Fatalf("after a second open the share holds %d items, %v", len(items), err)
	}
	var version int
	if err := again.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
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
	if err := s.InsertShare(ctx, &Share{ID: "s_exp", CreatedAt: 1000, ExpiresAt: &past}, []string{"f_live"}); err != nil {
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
