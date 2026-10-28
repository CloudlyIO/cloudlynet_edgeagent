package buffer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// reclaimThresholdBytes is how much free-page space may sit in the file before
// it is handed back to the filesystem with an incremental vacuum. Free pages are
// reused by later inserts anyway, so this only bounds the slack between the
// file size and the live data; a small threshold would just churn the file.
const reclaimThresholdBytes = 4 << 20

type Buffer struct {
	db       *sql.DB
	path     string
	maxBytes int64
}

func Open(path string, maxBytes int64) (*Buffer, error) {
	if path == "" {
		path = "/var/lib/cloudlynet-agent/buffer.sqlite"
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	b := &Buffer{db: db, path: path, maxBytes: maxBytes}
	if path != ":memory:" {
		b.ensureIncrementalVacuum()
	}
	if err := b.init(); err != nil {
		db.Close()
		return nil, err
	}
	return b, nil
}

func (b *Buffer) init() error {
	_, err := b.db.Exec(`
CREATE TABLE IF NOT EXISTS outbox(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  body BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS applied(
  command_id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  result BLOB,
  applied_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS cwmp_devices(
  device_id TEXT PRIMARY KEY,
  ip TEXT,
  manufacturer TEXT,
  product_class TEXT,
  serial_number TEXT,
  sw_version TEXT,
  last_inform_at INTEGER,
  writability_loaded INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS cwmp_params(
  device_id TEXT NOT NULL,
  path TEXT NOT NULL,
  value TEXT,
  writable INTEGER,
  cached_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, path)
);
CREATE TABLE IF NOT EXISTS cwmp_events(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id TEXT NOT NULL,
  module TEXT,
  event_type TEXT,
  severity TEXT,
  message TEXT,
  ts INTEGER NOT NULL,
  uploaded INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ftp_ingested(
  path TEXT PRIMARY KEY,
  size INTEGER NOT NULL,
  mtime_ns INTEGER NOT NULL,
  ingested_at INTEGER NOT NULL
);`)
	return err
}

// ensureIncrementalVacuum switches the database to auto_vacuum=INCREMENTAL so
// pages freed by drains and evictions can be returned to the filesystem
// without a full VACUUM. A new file takes the mode before its first table is
// created; a file created earlier with auto_vacuum=NONE only changes mode when
// rebuilt, so it gets one VACUUM here, at open, and never again. If that
// rebuild fails (e.g. not enough disk for the copy) the buffer still works:
// SQLite reuses free pages, so the file stays bounded by its peak, it just
// never shrinks.
func (b *Buffer) ensureIncrementalVacuum() {
	const incremental = 2
	mode := func() int {
		var m int
		_ = b.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&m)
		return m
	}
	if mode() == incremental {
		return
	}
	if _, err := b.db.Exec(`PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		log.Printf("buffer: cannot set auto_vacuum=INCREMENTAL: %v", err)
		return
	}
	if mode() == incremental {
		return
	}
	log.Printf("buffer: converting %s to auto_vacuum=INCREMENTAL (one-time VACUUM)", b.path)
	if _, err := b.db.Exec(`VACUUM`); err != nil {
		log.Printf("buffer: one-time VACUUM failed, free pages will be reused but not returned to the filesystem: %v", err)
	}
}

func (b *Buffer) Close() error {
	if b == nil || b.db == nil {
		return nil
	}
	return b.db.Close()
}

// ErrPayloadTooLarge is returned for a single body larger than the whole
// buffer_max_bytes cap: evicting the entire outbox still could not admit it.
var ErrPayloadTooLarge = errors.New("payload exceeds buffer_max_bytes")

// MaxBytes is the configured outbox payload cap (0 = uncapped).
func (b *Buffer) MaxBytes() int64 { return b.maxBytes }

// Enqueue appends body to the outbox. buffer_max_bytes (maxBytes) caps the
// outbox PAYLOAD (the sum of queued bodies), not the database file: the file
// also holds the applied/cwmp_*/ftp_ingested tables and free pages, none of
// which an outbox eviction can shrink. When the new body would push the
// payload over the cap, the oldest rows are evicted first, in one statement
// and the same transaction as the insert, so the outbox never sits over cap.
// A single body larger than the whole cap is refused (ErrPayloadTooLarge)
// rather than evicting everything to make room for it.
func (b *Buffer) Enqueue(ctx context.Context, kind string, body []byte) error {
	return b.EnqueueWithMarks(ctx, kind, body, nil)
}

// EnqueueWithMarks is Enqueue that also records FTP ingest marks in the same
// transaction as the outbox insert: an upload is marked ingested exactly when
// its events are durably queued, so a failed or interrupted enqueue leaves the
// file unmarked and a restart re-parses it instead of losing its events.
func (b *Buffer) EnqueueWithMarks(ctx context.Context, kind string, body []byte, marks []FTPMark) error {
	if b.maxBytes > 0 && int64(len(body)) > b.maxBytes {
		return fmt.Errorf("outbox: %d-byte %s payload: %w (%d)", len(body), kind, ErrPayloadTooLarge, b.maxBytes)
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var evicted int64
	if b.maxBytes > 0 {
		if evicted, err = evictOldest(ctx, tx, int64(len(body)), b.maxBytes); err != nil {
			return err
		}
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(kind, body, created_at) VALUES (?, ?, ?)`, kind, body, now); err != nil {
		return err
	}
	if err := upsertMarks(ctx, tx, marks, 0, len(marks), now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if evicted > 0 {
		log.Printf("outbox over buffer_max_bytes (%d): evicted %d oldest row(s) to admit a %d-byte %s payload", b.maxBytes, evicted, len(body), kind)
		b.reclaimFreePages(ctx)
	}
	return nil
}

// evictOldest deletes the shortest oldest-first run of outbox rows that brings
// the queued payload plus incoming bytes back under maxBytes, and returns how
// many rows it evicted. Under the cap it costs one SUM(length(body)), which
// SQLite answers from record headers without reading the blobs. Over it, the
// run's last id is found by walking (id, length) in id order only as far as
// the excess, again without reading blobs, and the run is deleted in ONE
// bounded statement.
func evictOldest(ctx context.Context, tx *sql.Tx, incoming, maxBytes int64) (int64, error) {
	var queued int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(body)), 0) FROM outbox`).Scan(&queued); err != nil {
		return 0, err
	}
	excess := queued + incoming - maxBytes
	if excess <= 0 {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, length(body) FROM outbox ORDER BY id`)
	if err != nil {
		return 0, err
	}
	var lastID, freed int64
	for freed < excess && rows.Next() {
		var n int64
		if err := rows.Scan(&lastID, &n); err != nil {
			rows.Close()
			return 0, err
		}
		freed += n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id <= ?`, lastID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PayloadBytes is the outbox payload the cap is enforced against.
func (b *Buffer) PayloadBytes(ctx context.Context) (int64, error) {
	var n int64
	err := b.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(body)), 0) FROM outbox`).Scan(&n)
	return n, err
}

// reclaimFreePages returns free pages to the filesystem once enough have
// accumulated. A no-op on a database still in auto_vacuum=NONE.
func (b *Buffer) reclaimFreePages(ctx context.Context) {
	if b.path == ":memory:" {
		return
	}
	var free, pageSize int64
	if err := b.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return
	}
	if err := b.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return
	}
	if free*pageSize < reclaimThresholdBytes {
		return
	}
	// Run as a query and drain it: the pragma frees pages as it is stepped.
	rows, err := b.db.QueryContext(ctx, `PRAGMA incremental_vacuum`)
	if err != nil {
		log.Printf("buffer: incremental_vacuum failed: %v", err)
		return
	}
	for rows.Next() {
	}
	_ = rows.Close()
}

func (b *Buffer) Drain(ctx context.Context, send func(kind string, body []byte) error) error {
	rows, err := b.db.QueryContext(ctx, `SELECT id, kind, body FROM outbox ORDER BY id LIMIT 100`)
	if err != nil {
		return err
	}
	type item struct {
		id   int64
		kind string
		body []byte
	}
	var batch []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.kind, &it.body); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	deleted := 0
	defer func() {
		if deleted > 0 {
			b.reclaimFreePages(ctx)
		}
	}()
	for _, it := range batch {
		if err := send(it.kind, it.body); err != nil {
			return err
		}
		if _, err := b.db.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, it.id); err != nil {
			return err
		}
		deleted++
	}
	return nil
}

func (b *Buffer) AlreadyApplied(ctx context.Context, commandID string) (bool, string, []byte, error) {
	var status string
	var result []byte
	err := b.db.QueryRowContext(ctx, `SELECT status, result FROM applied WHERE command_id = ?`, commandID).Scan(&status, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil, nil
	}
	if err != nil {
		return false, "", nil, err
	}
	return true, status, result, nil
}

func (b *Buffer) MarkApplied(ctx context.Context, commandID, status string, result []byte) error {
	_, err := b.db.ExecContext(ctx, `
INSERT INTO applied(command_id, status, result, applied_at) VALUES (?, ?, ?, ?)
ON CONFLICT(command_id) DO UPDATE SET status = excluded.status, result = excluded.result, applied_at = excluded.applied_at`,
		commandID, status, result, time.Now().Unix())
	return err
}

func (b *Buffer) Count(ctx context.Context) (int, error) {
	var n int
	err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&n)
	return n, err
}
