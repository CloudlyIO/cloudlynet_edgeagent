package collector

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"cloudlynet_edgeagent/goagent/internal/buffer"
	"cloudlynet_edgeagent/goagent/internal/cwmp"
	"cloudlynet_edgeagent/goagent/internal/rules"
)

const (
	testOUI          = "8C1F64"
	testSerial       = "2205600282"
	testProductClass = "ENB-N03002-B3"
	testCanonicalID  = "8C1F64-ENB%2DN03002%2DB3-2205600282"
)

// writeRingArchive builds a real-shaped gzipped tar with numbered ring entries
// (as the real NanoLink names them: 1…10/index/max), each entry's lines
// carrying the inline "[MODULE]" tag — no per-entry module semantics.
func writeRingArchive(t *testing.T, dir, name string, entries map[string][]string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	f, err := os.Create(full)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for entryName, lines := range entries {
		body := ""
		for _, l := range lines {
			body += l + "\n"
		}
		if err := tw.WriteHeader(&tar.Header{Name: entryName, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatalf("write header %s: %v", entryName, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write body %s: %v", entryName, err)
		}
	}
	return full
}

func writeBareLog(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write bare log: %v", err)
	}
	return full
}

func newTestCollector(t *testing.T, ftpDir string, onboard bool) *Collector {
	t.Helper()
	buf, err := buffer.Open(":memory:", 0)
	if err != nil {
		t.Fatalf("buffer open: %v", err)
	}
	t.Cleanup(func() { buf.Close() })
	if onboard {
		buf.UpsertDevice(cwmp.DeviceRecord{
			DeviceID:     testCanonicalID,
			SerialNumber: testSerial,
			ProductClass: testProductClass,
			IP:           "192.168.8.248",
		})
	}
	acs := cwmp.NewServer("0.0.0.0:7547", buf)
	return New(acs, rules.DefaultEngine(), ftpDir)
}

// TestScanFTPRealArchiveRoutesModuleAndDeviceID is the A+B compound regression
// guard: a real-shaped ring .tgz (numbered entries, inline [MODULE] lines)
// must classify to typed events (not UNKNOWN) keyed on the canonical device
// id (not the bare OUI "8C1F64" a first-`_` split would yield).
func TestScanFTPRealArchiveRoutesModuleAndDeviceID(t *testing.T) {
	dir := t.TempDir()
	c := newTestCollector(t, dir, true)

	writeRingArchive(t, dir, testOUI+"_"+testSerial+"_PowerOn_20240602_235010_continuouslogging.tgz", map[string][]string{
		"1": {
			"0000000217 2024-06-02 23:51:22.814 [FILE_TRANS] File upload success, curl code=(0), command (...)",
			"0000000030 2024-06-02 07:07:38.097 [TR69] RPC Unknown received from ACS",
		},
		"index": {"1"},
		"max":   {"10"},
	})

	c.scanFTP()
	events := c.DrainEvents()
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(events), events)
	}
	byType := map[string]string{}
	for _, e := range events {
		byType[e.EventType] = e.CWMPID
		if e.EventType == "unclassified" {
			t.Fatalf("event fell through to unclassified (module routing broken): %+v", e)
		}
	}
	for _, want := range []string{"ftp_upload_ok", "atc_fault_loop"} {
		device, ok := byType[want]
		if !ok {
			t.Fatalf("missing typed event %q; got %+v", want, byType)
		}
		if device != testCanonicalID {
			t.Errorf("event %q device = %q, want canonical %q (not bare OUI)", want, device, testCanonicalID)
		}
		if device == testOUI {
			t.Errorf("event %q mis-keyed to bare OUI %q — B fix regressed", want, testOUI)
		}
	}
}

// TestScanFTPBareDevicelogIngested is the A′ regression guard: a bare
// "*_Devicelog" upload (no .tgz) must be picked up by scanFTP's glob and have
// its inline-tagged lines classified — not silently dropped by a .tgz-only filter.
func TestScanFTPBareDevicelogIngested(t *testing.T) {
	dir := t.TempDir()
	c := newTestCollector(t, dir, true)

	writeBareLog(t, dir, testOUI+"_"+testSerial+"_PowerOn_20240602_235010_Devicelog", []string{
		"0000000096 2024-06-02 17:45:11.106 [FM] Critical alarm 0x16010400 raised, system reboot will be taken to recover it after 90s.",
	})

	c.scanFTP()
	events := c.DrainEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(events), events)
	}
	if events[0].EventType != "device_reboot" {
		t.Fatalf("EventType = %q, want device_reboot", events[0].EventType)
	}
	if events[0].CWMPID != testCanonicalID {
		t.Errorf("CWMPID = %q, want %q", events[0].CWMPID, testCanonicalID)
	}
}

// TestResolveDeviceIDViaStore covers B directly: OUI+serial from the filename
// resolve to the canonical id via cwmp_devices, never a filename substring.
func TestResolveDeviceIDViaStore(t *testing.T) {
	dir := t.TempDir()
	c := newTestCollector(t, dir, true)

	path := filepath.Join(dir, testOUI+"_"+testSerial+"_PowerOn_20240602_235010_continuouslogging.tgz")
	id, known := c.resolveDeviceID(path)
	if !known {
		t.Fatalf("resolveDeviceID: known = false, want true")
	}
	if id != testCanonicalID {
		t.Errorf("resolveDeviceID = %q, want %q", id, testCanonicalID)
	}
}

// TestResolveDeviceIDRejectsOUISuperstringCollision: a stored record whose OUI
// is a superstring of the incoming filename's OUI (e.g. a corrupted/duplicate
// "8C1F644-..." row) must NOT match "8C1F64" purely as a raw string prefix —
// the comparison must be delimiter-bound ("8C1F64-"). Regression guard for a
// bug the /code-review adversarial pass caught in the initial B fix.
func TestResolveDeviceIDRejectsOUISuperstringCollision(t *testing.T) {
	dir := t.TempDir()
	buf, err := buffer.Open(":memory:", 0)
	if err != nil {
		t.Fatalf("buffer open: %v", err)
	}
	defer buf.Close()
	// Only a corrupted/duplicate row is onboarded — its OUI "8C1F644" is a raw
	// superstring of the real "8C1F64", and it shares the incoming serial.
	buf.UpsertDevice(cwmp.DeviceRecord{
		DeviceID: "8C1F644-OTHERVENDOR-" + testSerial, SerialNumber: testSerial, ProductClass: "OTHERVENDOR",
	})
	acs := cwmp.NewServer("0.0.0.0:7547", buf)
	c := New(acs, rules.DefaultEngine(), dir)

	path := filepath.Join(dir, testOUI+"_"+testSerial+"_PowerOn_20240602_235010_continuouslogging.tgz")
	id, known := c.resolveDeviceID(path)
	if known {
		t.Fatalf("resolveDeviceID matched the corrupted superstring-OUI row %q for OUI %q — want known=false (defer), got id=%q", "8C1F644-OTHERVENDOR-"+testSerial, testOUI, id)
	}
}

// TestScanFTPDefersUnknownDeviceThenExhausts covers the onboard-before-log
// ordering: an upload from a not-yet-onboarded device must be deferred (not
// consumed) each tick, and only processed under the "unresolved:" sentinel
// once the deferral cap is exhausted — never silently dropped or mis-keyed.
func TestScanFTPDefersUnknownDeviceThenExhausts(t *testing.T) {
	dir := t.TempDir()
	c := newTestCollector(t, dir, false) // device NOT onboarded

	path := testOUI + "_" + testSerial + "_PowerOn_20240602_235010_continuouslogging.tgz"
	writeRingArchive(t, dir, path, map[string][]string{
		"1": {"0000000217 2024-06-02 23:51:22.814 [FILE_TRANS] File upload success, curl code=(0), command (...)"},
	})

	for i := 0; i < maxDeferTicks-1; i++ {
		c.scanFTP()
		if events := c.DrainEvents(); len(events) != 0 {
			t.Fatalf("tick %d: events = %+v, want none while deferred", i, events)
		}
	}
	full := filepath.Join(dir, path)
	if _, stillDeferred := c.deferred[full]; !stillDeferred {
		t.Fatalf("expected path still tracked as deferred before cap exhaustion")
	}
	if _, seen := c.seenPaths[full]; seen {
		t.Fatalf("path marked seen before deferral cap exhausted")
	}

	// Final tick exhausts the cap: process under the sentinel id, mark seen.
	c.scanFTP()
	events := c.DrainEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d after exhaustion, want 1: %+v", len(events), events)
	}
	if events[0].CWMPID != "unresolved:"+testOUI+"_"+testSerial {
		t.Errorf("CWMPID = %q, want unresolved sentinel", events[0].CWMPID)
	}
	if _, seen := c.seenPaths[full]; !seen {
		t.Errorf("path not marked seen after deferral exhaustion")
	}
	if _, stillDeferred := c.deferred[full]; stillDeferred {
		t.Errorf("deferred counter not cleared after exhaustion")
	}

	// A subsequent tick must not re-process the now-seen path.
	c.scanFTP()
	if events := c.DrainEvents(); len(events) != 0 {
		t.Fatalf("re-scanned an already-seen path: %+v", events)
	}
}

// TestScanFTPColdStartRace: the log arrives before Inform, then the device
// onboards — the deferred upload must land on the correct device once known,
// not the unresolved sentinel.
func TestScanFTPColdStartRace(t *testing.T) {
	dir := t.TempDir()
	c := newTestCollector(t, dir, false)

	path := testOUI + "_" + testSerial + "_PowerOn_20240602_235010_continuouslogging.tgz"
	writeRingArchive(t, dir, path, map[string][]string{
		"1": {"0000000217 2024-06-02 23:51:22.814 [FILE_TRANS] File upload success, curl code=(0), command (...)"},
	})

	c.scanFTP()
	if events := c.DrainEvents(); len(events) != 0 {
		t.Fatalf("events = %+v before onboard, want none", events)
	}

	// Device Informs (onboards) between ticks.
	c.acs.Store().UpsertDevice(cwmp.DeviceRecord{
		DeviceID: testCanonicalID, SerialNumber: testSerial, ProductClass: testProductClass,
	})

	c.scanFTP()
	events := c.DrainEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d after onboard, want 1: %+v", len(events), events)
	}
	if events[0].CWMPID != testCanonicalID {
		t.Errorf("CWMPID = %q, want canonical id once onboarded (no phantom)", events[0].CWMPID)
	}
}
