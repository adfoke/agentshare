// Package store keeps the metadata: which files exist, who may reach them, and
// what share links point at. It is a single SQLite file next to the blobs, so a
// running instance touches exactly two things on disk.
//
// Timestamps are kept as unix seconds in INTEGER columns. Storing them as text
// makes comparisons depend on the textual format, which silently produces wrong
// answers the first time two writers disagree about timezone or separators.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary cross-compiles
)

// ErrNotFound is returned for any unknown id, digest or token.
var ErrNotFound = errors.New("not found")

// Store is the metadata database.
type Store struct {
	db *sql.DB
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS files (
		id           TEXT PRIMARY KEY,
		sha256       TEXT NOT NULL,
		size         INTEGER NOT NULL,
		name         TEXT NOT NULL DEFAULT '',
		content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
		tags         TEXT NOT NULL DEFAULT '',
		meta         TEXT NOT NULL DEFAULT '{}',
		owner        TEXT NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER,
		downloads    INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_files_created ON files(created_at DESC, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_files_sha ON files(sha256)`,
	`CREATE INDEX IF NOT EXISTS idx_files_expires ON files(expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_files_sha_name ON files(sha256, name)`,

	`CREATE TABLE IF NOT EXISTS shares (
		id            TEXT PRIMARY KEY,
		label         TEXT NOT NULL DEFAULT '',
		owner         TEXT NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL,
		expires_at    INTEGER,
		max_downloads INTEGER,
		downloads     INTEGER NOT NULL DEFAULT 0,
		kind          TEXT NOT NULL DEFAULT 'file'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_shares_created ON shares(created_at DESC, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_shares_expires ON shares(expires_at)`,

	// A share covers a set of files, so the membership lives beside it rather
	// than in a column on it. ON DELETE CASCADE on file_id is what makes
	// deleting a file quietly remove it from every share that listed it —
	// and deliberately not the other way round: the share itself survives, so
	// a link to a set of files keeps working when one of them goes away.
	`CREATE TABLE IF NOT EXISTS share_items (
		share_id TEXT NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
		file_id  TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
		position INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (share_id, file_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_share_items_file ON share_items(file_id)`,
	`CREATE INDEX IF NOT EXISTS idx_share_items_share ON share_items(share_id, position)`,

	`CREATE TABLE IF NOT EXISTS tokens (
		id          TEXT PRIMARY KEY,
		name        TEXT NOT NULL DEFAULT '',
		secret_hash TEXT NOT NULL UNIQUE,
		scopes      TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		expires_at  INTEGER,
		last_used   INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_tokens_expires ON tokens(expires_at)`,
}

// Open opens (and creates) the database file at path.
func Open(path string) (*Store, error) {
	// The pragmas are part of the DSN: WAL lets readers run while a write is in
	// flight, the busy timeout turns the remaining lock contention into a short
	// wait instead of an immediate SQLITE_BUSY.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite takes one writer at a time; a small pool keeps readers parallel
	// without piling up connections that all fight for the same write lock.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
	}
	if err := migrate(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for health checks.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- files

// File is one logical upload: a name plus a pointer at content. Several files
// may point at the same digest (same bytes, different names); the blob is only
// released when the last of them is deleted.
type File struct {
	ID          string
	Sha256      string
	Size        int64
	Name        string
	ContentType string
	Tags        []string
	Meta        string
	Owner       string
	CreatedAt   int64
	ExpiresAt   *int64
	Downloads   int64
}

// Expired reports whether the file's lifetime has run out at t.
func (f *File) Expired(t time.Time) bool {
	return f.ExpiresAt != nil && *f.ExpiresAt <= t.Unix()
}

const fileCols = `id, sha256, size, name, content_type, tags, meta, owner, created_at, expires_at, downloads`

func scanFile(sc interface{ Scan(...any) error }) (*File, error) {
	var (
		f        File
		tags     string
		expires  sql.NullInt64
		download int64
	)
	err := sc.Scan(&f.ID, &f.Sha256, &f.Size, &f.Name, &f.ContentType, &tags, &f.Meta,
		&f.Owner, &f.CreatedAt, &expires, &download)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.Tags = splitTags(tags)
	f.Downloads = download
	if expires.Valid {
		v := expires.Int64
		f.ExpiresAt = &v
	}
	return &f, nil
}

// InsertFile adds a metadata record.
func (s *Store) InsertFile(ctx context.Context, f *File) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO files (`+fileCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		f.ID, f.Sha256, f.Size, f.Name, f.ContentType, joinTags(f.Tags), f.Meta,
		f.Owner, f.CreatedAt, nullable(f.ExpiresAt), f.Downloads)
	return err
}

// FileByID looks up one record, expired or not.
func (s *Store) FileByID(ctx context.Context, id string) (*File, error) {
	return scanFile(s.db.QueryRowContext(ctx, `SELECT `+fileCols+` FROM files WHERE id = ?`, id))
}

// FileBySha returns the newest live record holding that content — the natural
// name for a digest when a client asks for /v1/content/<sha> directly.
func (s *Store) FileBySha(ctx context.Context, sha string) (*File, error) {
	return scanFile(s.db.QueryRowContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE sha256 = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, strings.ToLower(sha)))
}

// FileByShaAndName finds an existing live record with the same content and the
// same name; that is what makes a retried upload idempotent instead of piling
// up duplicates.
func (s *Store) FileByShaAndName(ctx context.Context, sha, name string) (*File, error) {
	return scanFile(s.db.QueryRowContext(ctx,
		`SELECT `+fileCols+` FROM files
		 WHERE sha256 = ? AND name = ? AND (expires_at IS NULL OR expires_at > ?)
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		strings.ToLower(sha), name, time.Now().Unix()))
}

// ListFilter describes a page of the file listing.
type ListFilter struct {
	Query   string
	Tag     string
	Owner   string
	Since   *int64
	Until   *int64
	Include bool // include expired records
	Order   string
	Limit   int
	Cursor  string
}

// ListFiles returns one page plus the cursor for the next page ("" when done).
// Paging is keyset-based: a page is identified by the (created_at, id) of its
// last row, so inserting new files never shifts an in-flight listing.
func (s *Store) ListFiles(ctx context.Context, f ListFilter) ([]*File, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	asc := strings.EqualFold(f.Order, "asc")

	var (
		where []string
		args  []any
	)
	if !f.Include {
		where = append(where, `(expires_at IS NULL OR expires_at > ?)`)
		args = append(args, time.Now().Unix())
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, `(name LIKE ? ESCAPE '\' OR id = ? OR sha256 = ?)`)
		args = append(args, "%"+escapeLike(q)+"%", q, strings.ToLower(q))
	}
	if t := strings.TrimSpace(f.Tag); t != "" {
		where = append(where, `(',' || tags || ',') LIKE ?`)
		args = append(args, "%,"+escapeLike(t)+",%")
	}
	if f.Owner != "" {
		where = append(where, `owner = ?`)
		args = append(args, f.Owner)
	}
	if f.Since != nil {
		where = append(where, `created_at >= ?`)
		args = append(args, *f.Since)
	}
	if f.Until != nil {
		where = append(where, `created_at <= ?`)
		args = append(args, *f.Until)
	}
	if f.Cursor != "" {
		at, id, err := DecodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		if asc {
			where = append(where, `(created_at, id) > (?, ?)`)
		} else {
			where = append(where, `(created_at, id) < (?, ?)`)
		}
		args = append(args, at, id)
	}

	order := "DESC"
	if asc {
		order = "ASC"
	}
	q := `SELECT ` + fileCols + ` FROM files`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY created_at ` + order + `, id ` + order + ` LIMIT ?`
	args = append(args, limit+1) // one extra row tells us whether a next page exists

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	next := ""
	if len(out) > limit {
		last := out[limit-1]
		out = out[:limit]
		next = EncodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// DeleteResult reports what removing a file did to the underlying blob.
type DeleteResult struct {
	File *File
	// References is how many file records still point at the same content once
	// this one is gone. Zero means the bytes are now unreachable and eligible
	// for reclamation — but reclamation itself is not this call's job: see the
	// note on Store.DeleteFile.
	References int64
}

// DeleteFile removes a record and reports how many others still reference its
// content.
//
// It deliberately does not delete the blob. Deciding that a blob is unreferenced
// and then acting on that decision cannot be made atomic here: an upload of the
// same bytes can commit its blob and insert its row in the gap, at which point
// the deletion takes out content that a live record points at. The blob store's
// sweep decides this from a snapshot instead, which is the only place the
// question can be answered safely.
func (s *Store) DeleteFile(ctx context.Context, id string) (DeleteResult, error) {
	var res DeleteResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		f, err := scanFile(tx.QueryRowContext(ctx, `SELECT `+fileCols+` FROM files WHERE id = ?`, id))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id); err != nil {
			return err
		}
		var refs int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM files WHERE sha256 = ?`, f.Sha256).Scan(&refs); err != nil {
			return err
		}
		res.File = f
		res.References = refs
		return nil
	})
	return res, err
}

// BumpFileDownloads increments the download counter.
func (s *Store) BumpFileDownloads(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET downloads = downloads + 1 WHERE id = ?`, id)
	return err
}

// ---------------------------------------------------------------- shares

// Kinds of share. A share's shape is fixed when it is created rather than
// derived from how many of its members happen to be alive right now.
//
// That distinction matters more than it looks: a link handed out as a listing
// must keep opening as a listing even after some of its files expire. Deriving
// the shape from the live count instead would silently turn that link into a
// download of whichever file survived, which is not what the recipient
// bookmarked and not what the sender described.
const (
	ShareKindFile = "file" // one file: the URL serves its bytes
	ShareKindSet  = "set"  // several files: the URL serves an index page
)

// Share is one capability URL: whoever holds the id may reach the files it
// covers, with no credential of their own.
type Share struct {
	ID           string
	Label        string
	Owner        string
	CreatedAt    int64
	ExpiresAt    *int64
	MaxDownloads *int64
	Downloads    int64
	// Kind is ShareKindFile or ShareKindSet.
	Kind string

	// ItemCount is how many files the share covers, expired ones included. It
	// is filled in by the queries that ask for it and left zero by the ones
	// that do not.
	ItemCount int
}

// Expired reports whether the share's own lifetime has run out at t.
func (sh *Share) Expired(t time.Time) bool {
	return sh.ExpiresAt != nil && *sh.ExpiresAt <= t.Unix()
}

// Exhausted reports whether the download budget has been spent.
func (sh *Share) Exhausted() bool {
	return sh.MaxDownloads != nil && sh.Downloads >= *sh.MaxDownloads
}

// ServesBytes reports whether the share URL itself serves a file rather than
// an index page.
func (sh *Share) ServesBytes() bool { return sh.Kind != ShareKindSet }

// shareCols is the writable column list: it is used verbatim in INSERT.
const shareCols = `id, label, owner, created_at, expires_at, max_downloads, downloads, kind`

// shareSelect is the same columns for reading, plus the membership count. They
// are kept apart on purpose — a query embedded in an INSERT column list is a
// syntax error, not a convenience.
const shareSelect = shareCols + `,
	(SELECT COUNT(*) FROM share_items WHERE share_items.share_id = shares.id)`

func scanShare(sc interface{ Scan(...any) error }) (*Share, error) {
	var (
		sh  Share
		exp sql.NullInt64
		max sql.NullInt64
	)
	err := sc.Scan(&sh.ID, &sh.Label, &sh.Owner, &sh.CreatedAt, &exp, &max,
		&sh.Downloads, &sh.Kind, &sh.ItemCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if exp.Valid {
		v := exp.Int64
		sh.ExpiresAt = &v
	}
	if max.Valid {
		v := max.Int64
		sh.MaxDownloads = &v
	}
	return &sh, nil
}

// InsertShare stores a new share link and the files it covers, in one
// transaction: a share that exists without its members would be a link that
// resolves to nothing.
//
// The order of fileIDs is preserved, because it is the order the index page
// lists them in and the caller uploaded them in a meaningful sequence.
func (s *Store) InsertShare(ctx context.Context, sh *Share, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return errors.New("a share must cover at least one file")
	}
	if sh.Kind == "" {
		sh.Kind = ShareKindFile
		if len(fileIDs) > 1 {
			sh.Kind = ShareKindSet
		}
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO shares (`+shareCols+`) VALUES (?,?,?,?,?,?,?,?)`,
			sh.ID, sh.Label, sh.Owner, sh.CreatedAt,
			nullable(sh.ExpiresAt), nullable(sh.MaxDownloads), sh.Downloads, sh.Kind); err != nil {
			return err
		}
		// A repeated file id would violate the primary key and abort the whole
		// share; the caller's list is deduplicated before it gets here, and
		// OR IGNORE keeps that from being a load-bearing assumption.
		for pos, id := range fileIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO share_items (share_id, file_id, position) VALUES (?,?,?)`,
				sh.ID, id, pos); err != nil {
				return err
			}
		}
		sh.ItemCount = len(fileIDs)
		return nil
	})
}

// ShareByID looks up a share link.
func (s *Store) ShareByID(ctx context.Context, id string) (*Share, error) {
	return scanShare(s.db.QueryRowContext(ctx, `SELECT `+shareSelect+` FROM shares WHERE id = ?`, id))
}

// ShareItems returns the live files a share covers, in the order they were
// added. Files that have expired are left out: a listing should not offer
// something that can no longer be downloaded. Files that were deleted are gone
// from the table entirely, so their slots simply do not appear.
func (s *Store) ShareItems(ctx context.Context, shareID string) ([]*File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileCols+` FROM files
		 JOIN share_items ON share_items.file_id = files.id
		 WHERE share_items.share_id = ? AND (files.expires_at IS NULL OR files.expires_at > ?)
		 ORDER BY share_items.position, files.id`,
		shareID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Covers reports whether a file is one of the share's members, which is what
// gates a per-file download inside a share.
func (s *Store) ShareCovers(ctx context.Context, shareID, fileID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM share_items WHERE share_id = ? AND file_id = ?`,
		shareID, fileID).Scan(&n)
	return n > 0, err
}

// ReserveShareDownload consumes one download credit, atomically. It returns
// false when the budget was already spent, so two simultaneous requests can
// never both be the one that goes over the limit.
func (s *Store) ReserveShareDownload(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shares SET downloads = downloads + 1
		 WHERE id = ? AND (max_downloads IS NULL OR downloads < max_downloads)`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ListShares returns share links, newest first, optionally only those covering
// a particular file.
func (s *Store) ListShares(ctx context.Context, fileID string, limit int) ([]*Share, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var (
		q    = `SELECT ` + shareSelect + ` FROM shares`
		args []any
	)
	if fileID != "" {
		q += ` WHERE EXISTS (SELECT 1 FROM share_items
		         WHERE share_items.share_id = shares.id AND share_items.file_id = ?)`
		args = append(args, fileID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Share
	for rows.Next() {
		sh, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// DeleteShare revokes a share link. The members go with it by cascade.
func (s *Store) DeleteShare(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM shares WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- tokens

// Token is a scoped API credential. Only the hash of the secret is persisted.
type Token struct {
	ID        string
	Name      string
	Scopes    []string
	CreatedAt int64
	ExpiresAt *int64
	LastUsed  *int64
}

// TokenHash hashes a token secret for lookup and storage.
func TokenHash(secret string) string { return sha256Hex([]byte(strings.TrimSpace(secret))) }

const tokenCols = `id, name, scopes, created_at, expires_at, last_used`

func scanToken(sc interface{ Scan(...any) error }) (*Token, error) {
	var (
		t       Token
		scopes  string
		expires sql.NullInt64
		used    sql.NullInt64
	)
	err := sc.Scan(&t.ID, &t.Name, &scopes, &t.CreatedAt, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Scopes = splitTags(scopes)
	if expires.Valid {
		v := expires.Int64
		t.ExpiresAt = &v
	}
	if used.Valid {
		v := used.Int64
		t.LastUsed = &v
	}
	return &t, nil
}

// Expired reports whether the token's lifetime has run out at t.
func (t *Token) Expired(at time.Time) bool {
	return t.ExpiresAt != nil && *t.ExpiresAt <= at.Unix()
}

// InsertToken stores a new credential.
func (s *Store) InsertToken(ctx context.Context, t *Token, secretHash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (id, name, secret_hash, scopes, created_at, expires_at, last_used)
		 VALUES (?,?,?,?,?,?,?)`,
		t.ID, t.Name, secretHash, joinTags(t.Scopes), t.CreatedAt,
		nullable(t.ExpiresAt), nullable(t.LastUsed))
	return err
}

// TokenBySecret resolves a presented secret.
func (s *Store) TokenBySecret(ctx context.Context, secret string) (*Token, error) {
	return scanToken(s.db.QueryRowContext(ctx,
		`SELECT `+tokenCols+` FROM tokens WHERE secret_hash = ?`, TokenHash(secret)))
}

// TouchToken records the last successful use. Failures are not interesting
// enough to abort a request over.
func (s *Store) TouchToken(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used = ? WHERE id = ?`, time.Now().Unix(), id)
}

// ListTokens returns the credentials, newest first. Secrets are not included:
// they cannot be recovered, only replaced.
func (s *Store) ListTokens(ctx context.Context) ([]*Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+tokenCols+` FROM tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteToken revokes a credential.
func (s *Store) DeleteToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- housekeeping

// Stats is the /v1/stats payload.
type Stats struct {
	Files      int64
	FilesBytes int64
	Shares     int64
	Tokens     int64
}

// StatsFor collects the counters.
func (s *Store) StatsFor(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files`).Scan(&st.Files, &st.FilesBytes); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shares`).Scan(&st.Shares); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens`).Scan(&st.Tokens); err != nil {
		return st, err
	}
	return st, nil
}

// SweepResult reports what a janitor pass removed.
type SweepResult struct {
	Files  int
	Shares int
	Tokens int
}

// SweepExpired drops records whose lifetime has run out. Shares and tokens are
// deleted outright; files are deleted here too so their blobs can be collected.
func (s *Store) SweepExpired(ctx context.Context, now time.Time) (SweepResult, error) {
	var res SweepResult
	ts := now.Unix()
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, step := range []struct {
			sql string
			dst *int
		}{
			{`DELETE FROM tokens WHERE expires_at IS NOT NULL AND expires_at <= ?`, &res.Tokens},
			{`DELETE FROM files  WHERE expires_at IS NOT NULL AND expires_at <= ?`, &res.Files},
			{`DELETE FROM shares WHERE expires_at IS NOT NULL AND expires_at <= ?`, &res.Shares},
		} {
			r, err := tx.ExecContext(ctx, step.sql, ts)
			if err != nil {
				return err
			}
			if n, err := r.RowsAffected(); err == nil {
				*step.dst = int(n)
			}
		}
		return nil
	})
	return res, err
}

// AllDigests returns every digest still referenced by a file record. The blob
// collector uses it as the keep-set.
func (s *Store) AllDigests(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT sha256 FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var sum string
		if err := rows.Scan(&sum); err != nil {
			return nil, err
		}
		out[sum] = struct{}{}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- helpers

func nullable(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func joinTags(tags []string) string {
	clean := make([]string, 0, len(tags))
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			clean = append(clean, t)
		}
	}
	return strings.Join(clean, ",")
}

func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}
