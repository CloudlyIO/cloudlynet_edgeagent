package worker

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudlynet_edgeagent/goagent/internal/buffer"
	"cloudlynet_edgeagent/goagent/internal/cloud"
	"cloudlynet_edgeagent/goagent/internal/collector"
	"cloudlynet_edgeagent/goagent/internal/cwmp"
	"cloudlynet_edgeagent/goagent/internal/rules"
)

const (
	testSerial      = "2205609999"
	testCanonicalID = "8C1F64-ENB%2DN03002%2DB3-2205609999"
)

// newTestWorker wires a file-backed buffer (outbox + ingest ledger) and a
// collector watching dir, as main.go does. The cloud client is never used.
func newTestWorker(t *testing.T, dir string, maxBytes int64) (*Worker, *buffer.Buffer) {
	t.Helper()
	buf, err := buffer.Open(filepath.Join(t.TempDir(), "buffer.sqlite"), maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { buf.Close() })
	buf.UpsertDevice(cwmp.DeviceRecord{DeviceID: testCanonicalID, SerialNumber: testSerial, ProductClass: "ENB-N03002-B3"})
	acs := cwmp.NewServer("0.0.0.0:7547", buf)
	col := collector.New(acs, rules.DefaultEngine(), dir)
	if err := col.UseIngestLedger(buf); err != nil {
		t.Fatal(err)
	}
	return &Worker{acs: acs, buf: buf, col: col}, buf
}

// writeUploads writes n periodic uploads, each with `lines` classifiable
// lines padded to make the events large.
func writeUploads(t *testing.T, dir string, n, lines int) {
	t.Helper()
	pad := strings.Repeat("p", 200)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Log_20240602.%04d+0800_8C1F64.%s.gz", i, testSerial)
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		for j := 0; j < lines; j++ {
			fmt.Fprintf(gz, "0000000217 2024-06-02 23:51:22.814 [FILE_TRANS] File upload success, curl code=(0), command (%d-%d %s)\n", i, j, pad)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}

// TestEnqueueTelemetrySplitsOversizedBatchAndMarksAfterInsert: a backlog
// too large for one part (buffer_max_bytes/partsPerCap) is split into bodies
// that each fit, nothing is evicted or lost, and every upload ends up in the
// ledger with its events queued.
func TestEnqueueTelemetrySplitsOversizedBatchAndMarksAfterInsert(t *testing.T) {
	const capBytes = 256 << 10
	const partLimit = capBytes / partsPerCap
	dir := t.TempDir()
	writeUploads(t, dir, 40, 3)
	w, buf := newTestWorker(t, dir, capBytes)
	ctx := context.Background()

	batch := drainAfterScan(t, w)
	whole, _ := json.Marshal(cloud.TelemetryRequest{Events: batch.Events})
	if len(whole) <= partLimit*2 || len(whole) >= capBytes {
		t.Fatalf("test setup: batch body %d bytes should span several %d-byte parts and fit the %d cap", len(whole), partLimit, capBytes)
	}
	if !w.enqueueTelemetry(ctx, cloud.TelemetryRequest{Events: batch.Events}, batch) {
		t.Fatal("nothing queued")
	}
	n, _ := buf.Count(ctx)
	if n < 2 {
		t.Fatalf("outbox rows = %d, want the batch split across several", n)
	}
	var events int
	if err := buf.Drain(ctx, func(_ string, body []byte) error {
		if len(body) > partLimit {
			t.Errorf("body of %d bytes exceeds the %d part limit", len(body), partLimit)
		}
		var req cloud.TelemetryRequest
		_ = json.Unmarshal(body, &req)
		events += len(req.Events)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if events != len(batch.Events) {
		t.Fatalf("queued %d event(s), want %d (none evicted, none lost)", events, len(batch.Events))
	}
	if seen, _ := buf.LoadFTPIngested(); len(seen) != 40 {
		t.Fatalf("ledger rows = %d, want 40", len(seen))
	}
}

// TestEnqueueTelemetryFailureLeavesFilesUnmarkedAndReparses: when the outbox
// insert fails (here: the context is cancelled, as on SIGTERM mid-push), no
// drained upload is recorded as ingested, and the collector parses them again.
func TestEnqueueTelemetryFailureLeavesFilesUnmarkedAndReparses(t *testing.T) {
	dir := t.TempDir()
	writeUploads(t, dir, 5, 1)
	w, buf := newTestWorker(t, dir, 0)

	batch := drainAfterScan(t, w)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if w.enqueueTelemetry(cancelled, cloud.TelemetryRequest{Events: batch.Events}, batch) {
		t.Fatal("enqueue with a cancelled context reported success")
	}
	if seen, _ := buf.LoadFTPIngested(); len(seen) != 0 {
		t.Fatalf("failed enqueue persisted %d mark(s); their events are lost", len(seen))
	}
	again := drainAfterScan(t, w)
	if len(again.Events) != len(batch.Events) {
		t.Fatalf("re-parsed %d event(s), want %d", len(again.Events), len(batch.Events))
	}
	if !w.enqueueTelemetry(context.Background(), cloud.TelemetryRequest{Events: again.Events}, again) {
		t.Fatal("retry not queued")
	}
	if seen, _ := buf.LoadFTPIngested(); len(seen) != 5 {
		t.Fatalf("ledger rows after retry = %d, want 5", len(seen))
	}
}

// TestEnqueueTelemetryDropsAnUnsplittablePartOverTheCap: a single event
// larger than the whole cap can never be queued; it is dropped and its upload
// recorded as done instead of being re-parsed forever.
func TestEnqueueTelemetryDropsAnUnsplittablePartOverTheCap(t *testing.T) {
	dir := t.TempDir()
	writeUploads(t, dir, 1, 1)
	w, buf := newTestWorker(t, dir, 64)
	batch := drainAfterScan(t, w)
	if w.enqueueTelemetry(context.Background(), cloud.TelemetryRequest{Events: batch.Events}, batch) {
		t.Fatal("an over-cap event was reported queued")
	}
	if n, _ := buf.Count(context.Background()); n != 0 {
		t.Fatalf("outbox rows = %d, want 0", n)
	}
	if seen, _ := buf.LoadFTPIngested(); len(seen) != 1 {
		t.Fatalf("ledger rows = %d, want 1 (given up, not retried forever)", len(seen))
	}
}

func TestSplitTelemetryKeepsOrderAndFirstPartCarriesMetrics(t *testing.T) {
	var events []cloud.EventItem
	for i := 0; i < 9; i++ {
		events = append(events, cloud.EventItem{CWMPID: testCanonicalID, Message: fmt.Sprintf("%02d-%s", i, strings.Repeat("m", 300))})
	}
	req := cloud.TelemetryRequest{Metrics: []cloud.MetricSample{{CWMPID: testCanonicalID, Tier: 1}}, Events: events}
	parts := splitTelemetry(req, 1200)
	if len(parts) < 3 {
		t.Fatalf("parts = %d, want the 9 events split", len(parts))
	}
	next := 0
	for i, p := range parts {
		if p.start != next {
			t.Fatalf("part %d starts at %d, want %d (contiguous, in order)", i, p.start, next)
		}
		next = p.end
		var got cloud.TelemetryRequest
		_ = json.Unmarshal(p.body, &got)
		if (len(got.Metrics) > 0) != (i == 0) {
			t.Fatalf("part %d metrics = %d; only the first part carries them", i, len(got.Metrics))
		}
		for j, e := range got.Events {
			if e.Message[:2] != fmt.Sprintf("%02d", p.start+j) {
				t.Fatalf("part %d event %d out of order: %s", i, j, e.Message[:2])
			}
		}
	}
	if next != len(events) {
		t.Fatalf("parts cover %d event(s), want %d", next, len(events))
	}
	if one := splitTelemetry(req, 0); len(one) != 1 || one[0].end != len(events) {
		t.Fatalf("uncapped split = %d part(s), want 1", len(one))
	}
}

// drainAfterScan polls the watch dir once and drains the parsed batch.
func drainAfterScan(t *testing.T, w *Worker) collector.FTPBatch {
	t.Helper()
	w.col.ScanFTP()
	return w.col.DrainFTP()
}
