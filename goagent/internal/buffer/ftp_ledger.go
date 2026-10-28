package buffer

import (
	"context"
	"database/sql"
	"time"
)

// This file persists the FTP collector's ingest progress (ftp_ingested) so an
// agent restart does not re-parse every upload still in the watch directory.
// It lives in the same SQLite file as the outbox but is not outbox payload, so
// it never counts against buffer_max_bytes.

// FTPStamp identifies one version of an uploaded file. A same-named file
// rewritten later carries a different size or mtime and is a new upload.
type FTPStamp struct {
	Size    int64
	ModTime int64 // unix nanoseconds
}

// FTPMark records that the upload at Path, as it was at Stamp, is done with:
// parsed and its events handed off, or given up on after repeated parse
// failures.
type FTPMark struct {
	Path  string
	Stamp FTPStamp
}

// ledgerChunk bounds rows per transaction: one commit (one fsync) per chunk
// rather than per file, without holding the single DB connection, which the
// CWMP store also uses, for a whole first-run backlog of ~350k rows.
const ledgerChunk = 5000

// LoadFTPIngested returns every persisted ingest mark, keyed by path.
func (b *Buffer) LoadFTPIngested() (map[string]FTPStamp, error) {
	rows, err := b.db.Query(`SELECT path, size, mtime_ns FROM ftp_ingested`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FTPStamp{}
	for rows.Next() {
		var p string
		var st FTPStamp
		if err := rows.Scan(&p, &st.Size, &st.ModTime); err != nil {
			return nil, err
		}
		out[p] = st
	}
	return out, rows.Err()
}

// upsertFTPMarkSQL records one mark; a re-ingested (rewritten) path replaces
// its old stamp.
const upsertFTPMarkSQL = `
INSERT INTO ftp_ingested(path, size, mtime_ns, ingested_at) VALUES (?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime_ns=excluded.mtime_ns, ingested_at=excluded.ingested_at`

// MarkFTPIngested upserts marks outside any outbox write: for uploads with no
// events to hand off (nothing parsed, or given up on). Uploads whose events
// go to the outbox are marked by EnqueueWithMarks instead.
func (b *Buffer) MarkFTPIngested(marks []FTPMark) error {
	now := time.Now().Unix()
	return b.inChunks(len(marks), func(tx *sql.Tx, start, end int) error {
		return upsertMarks(context.Background(), tx, marks, start, end, now)
	})
}

// ForgetFTPIngested drops the marks of paths that have left the watch dir.
func (b *Buffer) ForgetFTPIngested(paths []string) error {
	return b.inChunks(len(paths), func(tx *sql.Tx, start, end int) error {
		stmt, err := tx.Prepare(`DELETE FROM ftp_ingested WHERE path = ?`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, p := range paths[start:end] {
			if _, err := stmt.Exec(p); err != nil {
				return err
			}
		}
		return nil
	})
}

// upsertMarks writes marks[start:end] inside tx with one prepared statement.
func upsertMarks(ctx context.Context, tx *sql.Tx, marks []FTPMark, start, end int, now int64) error {
	if start >= end {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, upsertFTPMarkSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range marks[start:end] {
		if _, err := stmt.ExecContext(ctx, m.Path, m.Stamp.Size, m.Stamp.ModTime, now); err != nil {
			return err
		}
	}
	return nil
}

// inChunks runs write over [0,n) in ledgerChunk-sized ranges, one transaction
// per range.
func (b *Buffer) inChunks(n int, write func(tx *sql.Tx, start, end int) error) error {
	for start := 0; start < n; start += ledgerChunk {
		end := min(start+ledgerChunk, n)
		tx, err := b.db.Begin()
		if err != nil {
			return err
		}
		if err := write(tx, start, end); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
