package buffer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

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
);`)
	return err
}

func (b *Buffer) Close() error {
	if b == nil || b.db == nil {
		return nil
	}
	return b.db.Close()
}

func (b *Buffer) Enqueue(ctx context.Context, kind string, body []byte) error {
	if err := b.trimToCap(ctx); err != nil {
		return err
	}
	_, err := b.db.ExecContext(ctx, `INSERT INTO outbox(kind, body, created_at) VALUES (?, ?, ?)`, kind, body, time.Now().Unix())
	if err != nil {
		return err
	}
	return b.trimToCap(ctx)
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
	for _, it := range batch {
		if err := send(it.kind, it.body); err != nil {
			return err
		}
		if _, err := b.db.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, it.id); err != nil {
			return err
		}
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

func (b *Buffer) trimToCap(ctx context.Context) error {
	if b.maxBytes <= 0 || b.path == ":memory:" {
		return nil
	}
	for {
		info, err := os.Stat(b.path)
		if err != nil || info.Size() <= b.maxBytes {
			return nil
		}
		if _, err := b.db.ExecContext(ctx, `DELETE FROM outbox WHERE id = (SELECT id FROM outbox ORDER BY id LIMIT 1)`); err != nil {
			return err
		}
		_, _ = b.db.ExecContext(ctx, `VACUUM`)
	}
}
