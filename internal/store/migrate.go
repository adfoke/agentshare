package store

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion is the highest migration this code knows how to reach. It is
// stored in SQLite's own user_version field, which costs nothing to read and
// travels inside the database file, so an older binary opening a newer file can
// at least notice.
const schemaVersion = 1

// migrate brings an existing database up to schemaVersion. A database created
// by this build is already current and the function returns immediately.
//
// The base schema in store.go uses CREATE TABLE IF NOT EXISTS, which is enough
// to add a *new* table but cannot change an existing one — so anything that
// alters a column's constraints has to be done here.
func migrate(ctx context.Context, db *sql.DB) error {
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return nil
	}
	if v < 1 {
		if err := migrateToShareItems(ctx, db); err != nil {
			return err
		}
	}
	// user_version takes no placeholder: it is a pragma, not a value.
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return nil
}

// migrateToShareItems moves a database from "one share equals one file" to
// "one share covers a set of files".
//
// Do the two shapes both have to be handled? Yes, and it is worth being
// explicit about why. A database created by this build already has the new
// shape, so the table rebuild below is skipped for it; a database created by an
// earlier build has shares.file_id NOT NULL with a foreign key onto files, and
// that has to be replaced.
func migrateToShareItems(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	legacy, err := columnIsNotNull(ctx, conn, "shares", "file_id")
	if err != nil {
		return err
	}
	hasKind, err := columnExists(ctx, conn, "shares", "kind")
	if err != nil {
		return err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if legacy {
		// SQLite cannot drop a NOT NULL constraint or a foreign key in place,
		// so the table is rebuilt. Nothing references `shares` at this point —
		// share_items is created further down — which is what makes a plain
		// replace safe here rather than a rename-dance with foreign keys
		// disabled.
		//
		// The old file_id column is what carried the membership, so its values
		// are what the backfill below has to read. They are copied into a
		// temporary table first for exactly that reason.
		stmts := []string{
			`CREATE TABLE shares_new (
				id            TEXT PRIMARY KEY,
				label         TEXT NOT NULL DEFAULT '',
				owner         TEXT NOT NULL DEFAULT '',
				created_at    INTEGER NOT NULL,
				expires_at    INTEGER,
				max_downloads INTEGER,
				downloads     INTEGER NOT NULL DEFAULT 0,
				kind          TEXT NOT NULL DEFAULT 'file'
			)`,
			`CREATE TABLE shares_membership AS
			   SELECT id AS share_id, file_id FROM shares WHERE file_id IS NOT NULL AND file_id <> ''`,
			// Every share in a database this old covered exactly one file, so
			// the kind is not in doubt and does not need a column to read it
			// from. The default does the work.
			`INSERT INTO shares_new (id, label, owner, created_at, expires_at, max_downloads, downloads)
			   SELECT id, label, owner, created_at, expires_at, max_downloads, downloads FROM shares`,
			`DROP TABLE shares`,
			`ALTER TABLE shares_new RENAME TO shares`,
			`CREATE INDEX IF NOT EXISTS idx_shares_created ON shares(created_at DESC, id DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_shares_expires ON shares(expires_at)`,
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("rebuild shares: %w", err)
			}
		}
	}

	if !legacy && !hasKind {
		// A database created by a build between the two shapes: the table is
		// already the new one, but predates the kind column. Adding it is a
		// one-line change rather than a rebuild.
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE shares ADD COLUMN kind TEXT NOT NULL DEFAULT 'file'`); err != nil {
			return fmt.Errorf("add shares.kind: %w", err)
		}
	}

	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS share_items (
			share_id TEXT NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
			file_id  TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
			position INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (share_id, file_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_share_items_file ON share_items(file_id)`,
		`CREATE INDEX IF NOT EXISTS idx_share_items_share ON share_items(share_id, position)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create share_items: %w", err)
		}
	}

	if legacy {
		// Every existing share covered exactly one file. A file that has since
		// been deleted is already absent from `files`, and the foreign key would
		// reject it, so the join is part of the copy rather than a check after.
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO share_items (share_id, file_id, position)
			   SELECT m.share_id, m.file_id, 0
			   FROM shares_membership m
			   JOIN files f ON f.id = m.file_id`); err != nil {
			return fmt.Errorf("backfill share_items: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE shares_membership`); err != nil {
			return fmt.Errorf("drop shares_membership: %w", err)
		}
	}

	return tx.Commit()
}

// columnExists reports whether a table has a column at all.
func columnExists(ctx context.Context, conn *sql.Conn, table, column string) (bool, error) {
	cols, err := tableColumns(ctx, conn, table)
	if err != nil {
		return false, err
	}
	for _, c := range cols {
		if c.name == column {
			return true, nil
		}
	}
	return false, nil
}

// columnIsNotNull reports whether a column exists and carries NOT NULL. A
// missing column (or a missing table) is reported as false rather than as an
// error: the question being asked is "is this the old shape?", and a shape
// without the column is not.
func columnIsNotNull(ctx context.Context, conn *sql.Conn, table, column string) (bool, error) {
	cols, err := tableColumns(ctx, conn, table)
	if err != nil {
		return false, err
	}
	for _, c := range cols {
		if c.name == column {
			return c.notNull, nil
		}
	}
	return false, nil
}

type columnInfo struct {
	name    string
	notNull bool
}

func tableColumns(ctx context.Context, conn *sql.Conn, table string) ([]columnInfo, error) {
	// PRAGMA table_info takes no placeholder, hence the concatenation.
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []columnInfo
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &primaryKey); err != nil {
			return nil, err
		}
		out = append(out, columnInfo{name: name, notNull: notNull == 1})
	}
	return out, rows.Err()
}
