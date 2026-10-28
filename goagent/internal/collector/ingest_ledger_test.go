package collector

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"cloudlynet_edgeagent/goagent/internal/buffer"
	"cloudlynet_edgeagent/goagent/internal/cloud"
	"cloudlynet_edgeagent/goagent/internal/cwmp"
	"cloudlynet_edgeagent/goagent/internal/rules"
)

// A second synthetic device that is NOT onboarded during the first run.
const (
	lateSerial      = "2205609998"
	lateCanonicalID = "8C1F64-ENB%2DN03002%2DB3-2205609998"
)

// agentRun is one agent lifetime: a file-backed buffer (the real buffer_db)
// and a collector wired to it as main.go does.
type agentRun struct {
	buf *buffer.Buffer
	c   *Collector
}

func startAgent(t *testing.T, dbPath, ftpDir string, devices ...cwmp.DeviceRecord) *agentRun {
	t.Helper()
	buf, err := buffer.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("buffer open: %v", err)
	}
	t.Cleanup(func() { buf.Close() })
	for _, d := range devices {
		buf.UpsertDevice(d)
	}
	c := New(cwmp.NewServer("0.0.0.0:7547", buf), rules.DefaultEngine(), ftpDir)
	if err := c.UseIngestLedger(buf); err != nil {
		t.Fatalf("UseIngestLedger: %v", err)
	}
	return &agentRun{buf: buf, c: c}
}

func (r *agentRun) stop() { r.buf.Close() }

// drain is the worker's successful hand-off: the batch's events are queued
// to the outbox together with their uploads' marks.
func (r *agentRun) drain(t *testing.T) []cloud.EventItem {
	t.Helper()
	batch := r.c.DrainFTP()
	if len(batch.Events) == 0 {
		return nil
	}
	if err := r.buf.EnqueueWithMarks(context.Background(), "telemetry", []byte("{}"), batch.MarksIn(0, len(batch.Events))); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return batch.Events
}

var (
	onboarded = cwmp.DeviceRecord{DeviceID: testCanonicalID, SerialNumber: testSerial, ProductClass: testProductClass}
	lateDev   = cwmp.DeviceRecord{DeviceID: lateCanonicalID, SerialNumber: lateSerial, ProductClass: testProductClass}
)

// uploadLine is a classifiable line whose raw text names its file, so events
// can be traced back to the upload that produced them.
func uploadLine(tag string) string {
	return "0000000217 2024-06-02 23:51:22.814 [FILE_TRANS] File upload success, curl code=(0), command (" + tag + ")"
}

func logName(tag, serial string) string {
	return "Log_20240602." + tag + "+0800_" + testOUI + "." + serial + ".gz"
}

// eventTags returns the sorted file tags carried by events' raw lines.
func eventTags(events []cloud.EventItem) []string {
	var out []string
	for _, e := range events {
		raw, _ := e.Attrs["raw"].(string)
		if i := strings.LastIndexByte(raw, '('); i >= 0 {
			out = append(out, strings.TrimSuffix(raw[i+1:], ")"))
		}
	}
	sort.Strings(out)
	return out
}

func writeTruncatedGzip(t *testing.T, dir, name string) {
	t.Helper()
	full := writeGzipLog(t, dir, name, []string{uploadLine("partial")})
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, b[:len(b)/2], 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFTPIngestResumesAfterRestart is the restart contract: with a
// pre-populated watch dir, a restarted agent ingests nothing it already
// ingested or gave up on, but does ingest files that arrived while it was
// down, uploads deferred as unresolved, uploads whose parse was still being
// retried, and a same-named file rewritten while it was down; and it forgets
// ledger rows for files that rotated away.
func TestFTPIngestResumesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "buffer.sqlite")

	// --- First lifetime.
	run1 := startAgent(t, dbPath, dir, onboarded)
	writeGzipLog(t, dir, logName("0001", testSerial), []string{uploadLine("done")})
	writeGzipLog(t, dir, logName("0002", testSerial), []string{uploadLine("rotates")})
	writeGzipLog(t, dir, logName("0003", testSerial), []string{uploadLine("rewritten-v1")})
	writeGzipLog(t, dir, logName("0004", lateSerial), []string{uploadLine("deferred")})
	corrupt := filepath.Join(dir, logName("0006", testSerial))
	if err := os.WriteFile(corrupt, []byte("not gzip at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	run1.c.scanFTP()
	got := run1.drain(t)
	// Arrives one scan later, so it is still inside its parse-retry budget
	// when the agent stops, while the corrupt file has exhausted its own.
	retrying := filepath.Join(dir, logName("0005", testSerial))
	writeTruncatedGzip(t, dir, logName("0005", testSerial))
	for i := 1; i < maxParseRetries; i++ {
		run1.c.scanFTP()
		got = append(got, run1.drain(t)...)
	}
	if tags := eventTags(got); strings.Join(tags, ",") != "done,rewritten-v1,rotates" {
		t.Fatalf("first run ingested %v", tags)
	}
	if _, given := run1.c.seenPaths[corrupt]; !given {
		t.Fatalf("test setup: corrupt upload should have been given up on")
	}
	if _, seen := run1.c.seenPaths[retrying]; seen || run1.c.parseFails[retrying] == 0 {
		t.Fatalf("test setup: truncated upload should still be retrying (fails=%d)", run1.c.parseFails[retrying])
	}
	run1.stop()

	// --- While the agent is down.
	if err := os.Remove(filepath.Join(dir, logName("0002", testSerial))); err != nil {
		t.Fatal(err)
	}
	writeGzipLog(t, dir, logName("0007", testSerial), []string{uploadLine("arrived-while-down")})
	writeGzipLog(t, dir, logName("0005", testSerial), []string{uploadLine("retry-completed")})
	rewritten := writeGzipLog(t, dir, logName("0003", testSerial), []string{uploadLine("rewritten-v2"), uploadLine("rewritten-v2b")})
	// Make sure the rewrite is visible even on a filesystem with coarse mtimes.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(rewritten, later, later); err != nil {
		t.Fatal(err)
	}

	// --- Second lifetime; the late device has onboarded meanwhile.
	run2 := startAgent(t, dbPath, dir, onboarded, lateDev)
	run2.c.scanFTP()
	tags := eventTags(run2.drain(t))
	want := []string{"arrived-while-down", "deferred", "retry-completed", "rewritten-v2", "rewritten-v2b"}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Fatalf("after restart ingested %v, want %v (nothing already ingested, all new/deferred/retrying/rewritten)", tags, want)
	}
	for i := 0; i < rewriteCheckTicks+1; i++ {
		run2.c.scanFTP()
		if extra := run2.drain(t); len(extra) != 0 {
			t.Fatalf("scan %d re-ingested %v", i, eventTags(extra))
		}
	}

	ledger, err := run2.buf.LoadFTPIngested()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger[filepath.Join(dir, logName("0002", testSerial))]; ok {
		t.Errorf("rotated-away path still in the ledger")
	}
	for _, tag := range []string{"0001", "0003", "0005", "0006", "0007"} {
		if _, ok := ledger[filepath.Join(dir, logName(tag, testSerial))]; !ok {
			t.Errorf("ledger missing %s", tag)
		}
	}
	if _, ok := ledger[filepath.Join(dir, logName("0004", lateSerial))]; !ok {
		t.Errorf("ledger missing the formerly deferred upload")
	}
	if len(ledger) != 6 {
		t.Errorf("ledger rows = %d, want 6 (bounded by the directory)", len(ledger))
	}
}

// TestFTPIngestUnresolvedSurvivesRestartUnmarked: an upload still unresolved
// at shutdown is never persisted as seen, so the next lifetime keeps
// deferring it and ingests it once the device informs.
func TestFTPIngestUnresolvedSurvivesRestartUnmarked(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "buffer.sqlite")
	writeGzipLog(t, dir, logName("0001", lateSerial), []string{uploadLine("unresolved")})

	run1 := startAgent(t, dbPath, dir)
	for i := 0; i < maxDeferTicks+1; i++ {
		run1.c.scanFTP()
	}
	if ev := run1.drain(t); len(ev) != 0 {
		t.Fatalf("unresolved upload emitted %v", eventTags(ev))
	}
	run1.stop()

	run2 := startAgent(t, dbPath, dir)
	run2.c.scanFTP()
	if ev := run2.drain(t); len(ev) != 0 {
		t.Fatalf("unresolved upload emitted after restart: %v", eventTags(ev))
	}
	if seen, _ := run2.buf.LoadFTPIngested(); len(seen) != 0 {
		t.Fatalf("unresolved upload persisted as seen: %v", seen)
	}
	run2.c.acs.Store().UpsertDevice(lateDev)
	run2.c.scanFTP()
	if tags := eventTags(run2.drain(t)); strings.Join(tags, ",") != "unresolved" {
		t.Fatalf("after onboarding ingested %v, want [unresolved]", tags)
	}
}

// TestFTPIngestMarkPersistsOnlyWithTheOutboxInsert: a parsed file's mark
// reaches the ledger only in the transaction that queues its events, so a
// crash after the drain, or a failed insert, re-parses the file instead of
// losing its events.
func TestFTPIngestMarkPersistsOnlyWithTheOutboxInsert(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "buffer.sqlite")
	writeGzipLog(t, dir, logName("0001", testSerial), []string{uploadLine("in-flight")})

	// Crash after the drain, before any insert.
	run1 := startAgent(t, dbPath, dir, onboarded)
	run1.c.scanFTP()
	if got := run1.c.DrainFTP(); len(got.Events) != 1 {
		t.Fatalf("drained %d event(s), want 1", len(got.Events))
	}
	if seen, _ := run1.buf.LoadFTPIngested(); len(seen) != 0 {
		t.Fatalf("mark persisted before its events were queued: %v", seen)
	}
	run1.stop()

	// The insert fails (SIGTERM cancelled the context): nothing persisted,
	// and the handed-back marks make the next scan re-parse the file.
	run2 := startAgent(t, dbPath, dir, onboarded)
	run2.c.scanFTP()
	batch := run2.c.DrainFTP()
	if tags := eventTags(batch.Events); strings.Join(tags, ",") != "in-flight" {
		t.Fatalf("after crash re-ingested %v, want [in-flight]", tags)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	marks := batch.MarksIn(0, len(batch.Events))
	if err := run2.buf.EnqueueWithMarks(cancelled, "telemetry", []byte("{}"), marks); err == nil {
		t.Fatalf("enqueue with a cancelled context succeeded")
	}
	if seen, _ := run2.buf.LoadFTPIngested(); len(seen) != 0 {
		t.Fatalf("failed enqueue left marks in the ledger: %v", seen)
	}
	run2.c.Requeue(marks)
	run2.c.scanFTP()
	if tags := eventTags(run2.drain(t)); strings.Join(tags, ",") != "in-flight" {
		t.Fatalf("requeued upload re-ingested %v, want [in-flight]", tags)
	}
	if seen, _ := run2.buf.LoadFTPIngested(); len(seen) != 1 {
		t.Fatalf("ledger rows after a queued hand-off = %d, want 1", len(seen))
	}
	run2.c.scanFTP()
	if extra := run2.drain(t); len(extra) != 0 {
		t.Fatalf("queued upload ingested again: %v", eventTags(extra))
	}
}

// TestFTPBatchMarksFollowTheirLastEvent: each upload's mark belongs to the
// slice of events holding its last event, and an event-less (given-up)
// upload is persisted by the scan itself.
func TestFTPBatchMarksFollowTheirLastEvent(t *testing.T) {
	dir := t.TempDir()
	run := startAgent(t, filepath.Join(t.TempDir(), "buffer.sqlite"), dir, onboarded)
	a := writeGzipLog(t, dir, logName("0001", testSerial), []string{uploadLine("a1"), uploadLine("a2")})
	b := writeGzipLog(t, dir, logName("0002", testSerial), []string{uploadLine("b1")})
	empty := writeGzipLog(t, dir, logName("0003", testSerial), []string{"no classifiable line"})
	run.c.scanFTP()
	batch := run.c.DrainFTP()
	if len(batch.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(batch.Events))
	}
	paths := func(ms []buffer.FTPMark) string {
		var out []string
		for _, m := range ms {
			out = append(out, filepath.Base(m.Path))
		}
		return strings.Join(out, ",")
	}
	if got := paths(batch.MarksIn(0, 1)); got != "" {
		t.Errorf("marks for events[0:1] = %q, want none (upload a continues)", got)
	}
	if got := paths(batch.MarksIn(1, 3)); got != filepath.Base(a)+","+filepath.Base(b) {
		t.Errorf("marks for events[1:3] = %q", got)
	}
	seen, _ := run.buf.LoadFTPIngested()
	if _, ok := seen[empty]; !ok || len(seen) != 1 {
		t.Fatalf("ledger = %v, want only the event-less upload", seen)
	}
}

// TestFTPIngestDetectsRewriteWhileRunning: a same-named upload rewritten
// after ingest (new size/mtime) is re-ingested within one rewrite-check
// cycle; an untouched one never is.
func TestFTPIngestDetectsRewriteWhileRunning(t *testing.T) {
	dir := t.TempDir()
	run := startAgent(t, filepath.Join(t.TempDir(), "buffer.sqlite"), dir, onboarded)
	writeGzipLog(t, dir, logName("0001", testSerial), []string{uploadLine("v1")})
	writeGzipLog(t, dir, logName("0002", testSerial), []string{uploadLine("steady")})
	run.c.scanFTP()
	if tags := eventTags(run.drain(t)); strings.Join(tags, ",") != "steady,v1" {
		t.Fatalf("initial ingest %v", tags)
	}

	p := writeGzipLog(t, dir, logName("0001", testSerial), []string{uploadLine("v2"), uploadLine("v2-more")})
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	var got []cloud.EventItem
	for i := 0; i < rewriteCheckTicks; i++ {
		run.c.scanFTP()
		got = append(got, run.drain(t)...)
	}
	if tags := eventTags(got); strings.Join(tags, ",") != "v2,v2-more" {
		t.Fatalf("rewrite re-ingested %v, want [v2 v2-more]", tags)
	}
	ledger, _ := run.buf.LoadFTPIngested()
	fi, _ := os.Stat(p)
	if st := ledger[p]; st.Size != fi.Size() || st.ModTime != fi.ModTime().UnixNano() {
		t.Fatalf("ledger stamp %+v not updated to the rewritten file (size %d)", st, fi.Size())
	}
}
