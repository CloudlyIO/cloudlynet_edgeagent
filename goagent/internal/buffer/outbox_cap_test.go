package buffer

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openFileBuffer(t *testing.T, maxBytes int64) (*Buffer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "buffer.sqlite")
	b, err := Open(path, maxBytes)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b, path
}

// body returns an n-byte payload tagged with seq so drain order is checkable.
func body(seq, n int) []byte {
	tag := fmt.Sprintf("%06d:", seq)
	return append([]byte(tag), bytes.Repeat([]byte{'x'}, n-len(tag))...)
}

func drainSeqs(t *testing.T, b *Buffer) []string {
	t.Helper()
	var out []string
	for {
		n, err := b.Count(context.Background())
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n == 0 {
			return out
		}
		if err := b.Drain(context.Background(), func(_ string, body []byte) error {
			out = append(out, string(body[:6]))
			return nil
		}); err != nil {
			t.Fatalf("drain: %v", err)
		}
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func autoVacuumMode(t *testing.T, b *Buffer) int {
	t.Helper()
	var m int
	if err := b.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&m); err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	return m
}

// TestOutboxCapCountsPayloadAndEvictsOldestInOneStep: the cap bounds queued
// payload bytes; admitting a body that overflows it evicts exactly the oldest
// rows needed (here 3 for a 3-row-sized body), logs the count, and leaves the
// survivors in strict FIFO order.
func TestOutboxCapCountsPayloadAndEvictsOldestInOneStep(t *testing.T) {
	ctx := context.Background()
	const row = 1000
	b, _ := openFileBuffer(t, 10*row)
	for i := 1; i <= 10; i++ {
		if err := b.Enqueue(ctx, "telemetry", body(i, row)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if n, _ := b.Count(ctx); n != 10 {
		t.Fatalf("count at exactly the cap = %d, want 10 (nothing evicted)", n)
	}
	if p, _ := b.PayloadBytes(ctx); p != 10*row {
		t.Fatalf("payload = %d, want %d", p, 10*row)
	}

	logs := captureLog(t)
	if err := b.Enqueue(ctx, "telemetry", body(11, 3*row)); err != nil {
		t.Fatalf("enqueue over cap: %v", err)
	}
	if n, _ := b.Count(ctx); n != 8 {
		t.Fatalf("count after overflow = %d, want 8 (3 evicted, 1 added)", n)
	}
	if p, _ := b.PayloadBytes(ctx); p > 10*row {
		t.Fatalf("payload after eviction = %d, over the %d cap", p, 10*row)
	}
	if !strings.Contains(logs.String(), "evicted 3 oldest row(s)") {
		t.Fatalf("eviction not logged with its count; log = %q", logs.String())
	}
	want := []string{"000004", "000005", "000006", "000007", "000008", "000009", "000010", "000011"}
	if got := drainSeqs(t, b); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("drain order = %v, want %v", got, want)
	}
}

// TestOutboxCapIgnoresNonOutboxTables: the applied, cwmp_* and ftp_ingested
// tables share the file but are not outbox payload. Filling them far past
// buffer_max_bytes must not evict a single outbox row, and the applied rows
// survive outbox evictions untouched.
func TestOutboxCapIgnoresNonOutboxTables(t *testing.T) {
	ctx := context.Background()
	const capBytes = 8 << 10
	b, path := openFileBuffer(t, capBytes)
	big := bytes.Repeat([]byte{'r'}, 4<<10)
	for i := 0; i < 64; i++ {
		if err := b.MarkApplied(ctx, fmt.Sprintf("cmd-%d", i), "applied", big); err != nil {
			t.Fatalf("mark applied: %v", err)
		}
	}
	marks := make([]FTPMark, 2000)
	for i := range marks {
		marks[i] = FTPMark{Path: fmt.Sprintf("/ftp/Log_%06d.gz", i), Stamp: FTPStamp{Size: 1, ModTime: 1}}
	}
	if err := b.MarkFTPIngested(marks); err != nil {
		t.Fatalf("mark ftp: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() <= capBytes {
		t.Fatalf("test setup: file should already exceed the cap; size=%v err=%v", fi.Size(), err)
	}

	for i := 1; i <= 4; i++ {
		if err := b.Enqueue(ctx, "telemetry", body(i, 1<<10)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if n, _ := b.Count(ctx); n != 4 {
		t.Fatalf("outbox count = %d, want 4: non-outbox tables must not count against the cap", n)
	}
	// Push the outbox itself over the cap: evictions touch only outbox rows.
	for i := 5; i <= 20; i++ {
		if err := b.Enqueue(ctx, "telemetry", body(i, 1<<10)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if n, _ := b.Count(ctx); n != 8 {
		t.Fatalf("outbox count = %d, want 8 (8 KiB cap / 1 KiB rows)", n)
	}
	for i := 0; i < 64; i++ {
		ok, status, result, err := b.AlreadyApplied(ctx, fmt.Sprintf("cmd-%d", i))
		if err != nil || !ok || status != "applied" || !bytes.Equal(result, big) {
			t.Fatalf("applied row cmd-%d changed: ok=%v status=%q err=%v", i, ok, status, err)
		}
	}
	if seen, err := b.LoadFTPIngested(); err != nil || len(seen) != len(marks) {
		t.Fatalf("ftp ledger rows = %d err=%v, want %d", len(seen), err, len(marks))
	}
}

// TestOutboxRefusesASinglePayloadLargerThanTheCap: evicting the whole outbox
// still could not admit it, so it is refused and the queue is left alone.
func TestOutboxRefusesASinglePayloadLargerThanTheCap(t *testing.T) {
	ctx := context.Background()
	b, _ := openFileBuffer(t, 4<<10)
	if err := b.Enqueue(ctx, "telemetry", body(1, 1<<10)); err != nil {
		t.Fatal(err)
	}
	if err := b.Enqueue(ctx, "telemetry", body(2, 5<<10)); err == nil {
		t.Fatalf("oversized payload accepted")
	}
	if got := drainSeqs(t, b); strings.Join(got, ",") != "000001" {
		t.Fatalf("outbox after refusal = %v, want [000001]", got)
	}
}

// TestOutboxAtCapStaysUnderCapAndFileBounded: thousands of enqueues at the
// cap must keep the payload under the cap, and the file bounded near it (free
// pages are reused and incrementally reclaimed), with the database in
// auto_vacuum=INCREMENTAL.
func TestOutboxAtCapStaysUnderCapAndFileBounded(t *testing.T) {
	ctx := context.Background()
	const capBytes = 256 << 10
	b, path := openFileBuffer(t, capBytes)
	if m := autoVacuumMode(t, b); m != 2 {
		t.Fatalf("new database auto_vacuum = %d, want 2 (INCREMENTAL)", m)
	}
	captureLog(t) // one eviction line per enqueue at the cap; keep test output readable
	for i := 1; i <= 1200; i++ {
		if err := b.Enqueue(ctx, "telemetry", body(i, 2<<10)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if p, _ := b.PayloadBytes(ctx); p > capBytes {
		t.Fatalf("payload %d over cap %d", p, capBytes)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Payload plus page/record overhead and the reclaim threshold's slack.
	if limit := int64(capBytes*2 + reclaimThresholdBytes); fi.Size() > limit {
		t.Fatalf("file size %d exceeds %d after sustained eviction", fi.Size(), limit)
	}

	// Draining a large outbox hands the freed pages back to the filesystem.
	b2, path2 := openFileBuffer(t, 0)
	for i := 1; i <= 400; i++ {
		if err := b2.Enqueue(ctx, "telemetry", body(i, 32<<10)); err != nil {
			t.Fatal(err)
		}
	}
	full, _ := os.Stat(path2)
	drainSeqs(t, b2)
	empty, _ := os.Stat(path2)
	if empty.Size() >= full.Size()/2 {
		t.Fatalf("file did not shrink after drain: %d -> %d bytes", full.Size(), empty.Size())
	}
}

// TestOpenUpgradesAutoVacuumNoneDatabase: a buffer file created by an older
// agent (auto_vacuum=NONE) is converted once at open, keeping its outbox rows
// in FIFO order and its applied rows.
func TestOpenUpgradesAutoVacuumNoneDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "buffer.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA auto_vacuum=NONE`,
		`CREATE TABLE outbox(id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, body BLOB NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE TABLE applied(command_id TEXT PRIMARY KEY, status TEXT NOT NULL, result BLOB, applied_at INTEGER NOT NULL)`,
		`INSERT INTO applied VALUES ('cmd-old', 'applied', x'7b7d', 1)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i := 1; i <= 3; i++ {
		if _, err := raw.Exec(`INSERT INTO outbox(kind, body, created_at) VALUES ('telemetry', ?, 1)`, body(i, 64)); err != nil {
			t.Fatal(err)
		}
	}
	var mode int
	_ = raw.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode)
	raw.Close()
	if mode != 0 {
		t.Fatalf("test setup: legacy db auto_vacuum = %d, want 0", mode)
	}

	b, err := Open(path, 1<<20)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer b.Close()
	if m := autoVacuumMode(t, b); m != 2 {
		t.Fatalf("legacy db auto_vacuum after open = %d, want 2 (INCREMENTAL)", m)
	}
	if ok, status, _, err := b.AlreadyApplied(ctx, "cmd-old"); err != nil || !ok || status != "applied" {
		t.Fatalf("applied row lost in upgrade: ok=%v status=%q err=%v", ok, status, err)
	}
	if err := b.Enqueue(ctx, "telemetry", body(4, 64)); err != nil {
		t.Fatal(err)
	}
	if got := drainSeqs(t, b); strings.Join(got, ",") != "000001,000002,000003,000004" {
		t.Fatalf("drain order after upgrade = %v", got)
	}
}

func schemaVersion(t *testing.T, b *Buffer) int64 {
	t.Helper()
	var v int64
	if err := b.db.QueryRow(`PRAGMA schema_version`).Scan(&v); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	return v
}

// TestOutboxSteadyStateRunsNoFullVacuum: the failure this replaces ran a full
// VACUUM per evicted row once the file reached the cap. A full VACUUM always
// bumps the schema cookie (PRAGMA schema_version), whoever issues it, while
// inserts, deletes and incremental_vacuum leave it alone; so sustained
// eviction and drains at the cap must leave it unchanged. That holds for a new
// database and for one converted from auto_vacuum=NONE, after its one-time
// conversion at open.
func TestOutboxSteadyStateRunsNoFullVacuum(t *testing.T) {
	ctx := context.Background()
	const capBytes = 256 << 10
	captureLog(t)
	churn := func(t *testing.T, b *Buffer) {
		t.Helper()
		for i := 1; i <= 600; i++ {
			if err := b.Enqueue(ctx, "telemetry", body(i, 4<<10)); err != nil {
				t.Fatalf("enqueue %d: %v", i, err)
			}
			if i%200 == 0 {
				drainSeqs(t, b)
			}
		}
	}

	b, _ := openFileBuffer(t, capBytes)
	before := schemaVersion(t, b)
	churn(t, b)
	if after := schemaVersion(t, b); after != before {
		t.Fatalf("schema_version %d -> %d: a full VACUUM ran during steady-state enqueue/evict/drain", before, after)
	}
	// The detector itself: a full VACUUM does move it.
	if _, err := b.db.Exec(`VACUUM`); err != nil {
		t.Fatal(err)
	}
	if schemaVersion(t, b) == before {
		t.Fatalf("test detector broken: VACUUM did not change schema_version")
	}

	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA auto_vacuum=NONE`,
		`CREATE TABLE outbox(id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, body BLOB NOT NULL, created_at INTEGER NOT NULL)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	legacy, err := Open(path, capBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	before = schemaVersion(t, legacy)
	churn(t, legacy)
	if after := schemaVersion(t, legacy); after != before {
		t.Fatalf("converted db: schema_version %d -> %d: a full VACUUM ran after open", before, after)
	}
}

// TestEnqueueCostDoesNotScaleWithQueuedBytes: an enqueue must not read the
// queued payload. Summing body lengths and walking (id, length) for an
// eviction read record headers only; materialising bodies (e.g. a window
// function over the body column) read the whole outbox inside the write
// transaction on every push, ~100+ ms per enqueue at a 100 MiB backlog. The
// bound is far above a header-only enqueue (~1 ms) and far below a full read.
func TestEnqueueCostDoesNotScaleWithQueuedBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("writes ~96 MiB")
	}
	ctx := context.Background()
	const rowBytes = 4 << 20
	const rows = 24
	b, _ := openFileBuffer(t, rows*rowBytes+64<<10)
	for i := 1; i <= rows; i++ {
		if err := b.Enqueue(ctx, "telemetry", body(i, rowBytes)); err != nil {
			t.Fatal(err)
		}
	}
	captureLog(t)
	measure := func(n int) time.Duration {
		start := time.Now()
		for i := 0; i < n; i++ {
			if err := b.Enqueue(ctx, "telemetry", body(1000+i, 2<<10)); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start) / time.Duration(n)
	}
	underCap := measure(10) // 20 KiB added: still under the cap
	atCap := measure(40)    // crosses the cap: evicts one 4 MiB row
	const bound = 40 * time.Millisecond
	if underCap > bound || atCap > bound {
		t.Fatalf("enqueue with %d MiB queued: %v under cap, %v at cap per call; want < %v (payload must not be read)", rows*rowBytes>>20, underCap, atCap, bound)
	}
	if n, _ := b.Count(ctx); n >= rows+50 {
		t.Fatalf("count = %d: the at-cap phase evicted nothing", n)
	}
}
