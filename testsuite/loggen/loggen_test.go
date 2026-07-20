package loggen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
	"time"
)

func testConfig(scenario Scenario) Config {
	return Config{
		OUI: "8C1F64", Serial: "2205600282", ProductClass: "ENB-N03002-B3",
		PowerOnAt: time.Date(2024, 6, 2, 23, 50, 10, 0, time.UTC),
		Scenario:  scenario,
	}
}

func TestArchiveAndDeviceLogNamesMatchRealShape(t *testing.T) {
	cfg := testConfig(ScenarioHappy)
	if got, want := cfg.ArchiveName(), "8C1F64_2205600282_PowerOn_20240602_235010_continuouslogging.tgz"; got != want {
		t.Errorf("ArchiveName() = %q, want %q", got, want)
	}
	if got, want := cfg.DeviceLogName(), "8C1F64_2205600282_PowerOn_20240602_235010_Devicelog"; got != want {
		t.Errorf("DeviceLogName() = %q, want %q", got, want)
	}
	if strings.HasSuffix(cfg.DeviceLogName(), ".tgz") {
		t.Errorf("DeviceLogName() must not carry a .tgz suffix (bare Devicelog upload)")
	}
}

// TestGenerateRingArchiveHasNumberedEntries locks in the ring layout: entries
// named 1…10/index/max (never a module-named entry), module living only inline
// on each line.
func TestGenerateRingArchiveHasNumberedEntries(t *testing.T) {
	cfg := testConfig(ScenarioHappy)
	const scmLine = "0000000057 2024-06-02 11:25:29.190 [SCM] Process(pid=1843) tr69c started"
	const tr69Alarm = "0000000030 2024-06-02 07:07:38.097 [TR69] Alarm Report, id: 0x18020500 detail: ACS Disconnect with error 1"
	archive, deviceLog, err := Generate(cfg, []string{scmLine, tr69Alarm})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Devicelog is the alarm-class subset: it carries the [TR69] alarm line but
	// NOT the operational [SCM] line (which lives in the full ring only).
	if !strings.Contains(string(deviceLog), "ACS Disconnect with error") {
		t.Errorf("Devicelog missing the alarm-class [TR69] line; got:\n%s", deviceLog)
	}
	if strings.Contains(string(deviceLog), "[SCM]") {
		t.Errorf("Devicelog must not carry operational [SCM] lines (alarm-subset only); got:\n%s", deviceLog)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	var totalLines int
	ringHasSCM := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		names[h.Name] = true
		b, _ := io.ReadAll(tr)
		if strings.Contains(string(b), "[SCM]") {
			ringHasSCM = true
		}
		for _, l := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(l) != "" {
				totalLines++
			}
		}
	}
	// The ring is the full log — all modules interleaved, including the
	// operational [SCM] line the Devicelog excludes.
	if !ringHasSCM {
		t.Errorf("ring must carry the full log incl. operational [SCM] lines")
	}
	for _, want := range []string{"1", "2", "10", "index", "max"} {
		if !names[want] {
			t.Errorf("archive missing expected ring entry %q; got %v", want, names)
		}
	}
	if names["TR69.log"] || names["FILE_TRANS.log"] || names["FM.log"] {
		t.Errorf("archive must not use module-named entries (defeats the A fix's real-shape test)")
	}
	if totalLines == 0 {
		t.Errorf("archive entries carry no lines")
	}
}

// TestScenarioLineMatchesConfiguredFault ensures each scenario stages the
// exact module+text its rules.DefaultEngine() rule expects (kept in lockstep
// by hand with engine.go / config/rules.yaml).
func TestScenarioLineMatchesConfiguredFault(t *testing.T) {
	cases := map[Scenario]string{
		ScenarioFTPPathReject: "curl code=(25)",
		ScenarioFTPAuthFail:   "curl code=(67)",
		ScenarioFTPConnFail:   "curl code=(7)",
		ScenarioFTPTimeout:    "curl code=(28)",
		ScenarioATCFault:      "RPC Unknown received from ACS",
		ScenarioReboot:        "system reboot will be taken",
	}
	for scenario, want := range cases {
		archive, _, err := Generate(testConfig(scenario), nil)
		if err != nil {
			t.Fatalf("scenario %s: Generate: %v", scenario, err)
		}
		// The ring carries the full log (all modules), so every scenario's
		// staged fault line lands there regardless of which stream (ring vs
		// alarm-only Devicelog) it also belongs to.
		if got := ringText(t, archive); !strings.Contains(got, want) {
			t.Errorf("scenario %s: ring missing staged fault %q", scenario, want)
		}
	}
}

// ringText decompresses a ring archive and returns the concatenated text of
// all its entries.
func ringText(t *testing.T, archive []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)
	var buf strings.Builder
	for {
		if _, err := tr.Next(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		b, _ := io.ReadAll(tr)
		buf.Write(b)
	}
	return buf.String()
}
