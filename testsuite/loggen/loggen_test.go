package loggen

import (
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
		At:       time.Date(2024, 6, 2, 23, 50, 10, 0, time.UTC),
		Scenario: scenario,
	}
}

// gunzipText decompresses a single-file gzip log and returns its text.
func gunzipText(t *testing.T, gzBytes []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(gzBytes))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	b, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return string(b)
}

// TestLogAndErrorLogNamesMatchRealShape locks the routine periodic-feed filename
// shapes: "Log_<date>.<time>+<tz>_<OUI>.<serial>.gz" and the "ErrorLog_" sibling
// (single-file .gz, never a .tgz tar).
func TestLogAndErrorLogNamesMatchRealShape(t *testing.T) {
	cfg := testConfig(ScenarioHappy)
	if got, want := cfg.LogName(), "Log_20240602.2350+0000_8C1F64.2205600282.gz"; got != want {
		t.Errorf("LogName() = %q, want %q", got, want)
	}
	if got, want := cfg.ErrorLogName(), "ErrorLog_20240602.2350+0000_8C1F64.2205600282.gz"; got != want {
		t.Errorf("ErrorLogName() = %q, want %q", got, want)
	}
	for _, n := range []string{cfg.LogName(), cfg.ErrorLogName()} {
		if strings.HasSuffix(n, ".tgz") {
			t.Errorf("%q must be a single-file .gz, not a .tgz tar", n)
		}
	}
}

// TestGenerateProducesSingleFileGzips verifies both artifacts are single-file
// gzip (not tar): the Log carries the full slice while the ErrorLog carries only
// the error/alarm subset (the [SCM] operational line is excluded, the [TR69]
// alarm kept).
func TestGenerateProducesSingleFileGzips(t *testing.T) {
	cfg := testConfig(ScenarioHappy)
	const scmLine = "0000000057 2024-06-02 11:25:29.190 [SCM] Process(pid=1843) tr69c started"
	const tr69Alarm = "0000000030 2024-06-02 07:07:38.097 [TR69] Alarm Report, id: 0x18020500 detail: ACS Disconnect with error 1"
	logGz, errGz, err := Generate(cfg, []string{scmLine, tr69Alarm})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	logText := gunzipText(t, logGz)
	errText := gunzipText(t, errGz)

	// Single-file gzip, not a gzip-tar: the decompressed bytes start with the
	// first raw log line (seq 0000000001), never a 512-byte tar header block.
	if !strings.HasPrefix(logText, "0000000001 ") {
		t.Errorf("Log feed must be raw gzipped log lines (single-file gzip), not a tar; got prefix:\n%.80s", logText)
	}

	// Log is the full slice — operational [SCM] line AND the alarm.
	if !strings.Contains(logText, "[SCM]") || !strings.Contains(logText, "ACS Disconnect with error") {
		t.Errorf("Log feed must carry the full slice (operational + alarm); got:\n%s", logText)
	}
	// ErrorLog is the error subset — carries the [TR69] alarm but NOT the
	// non-error operational [SCM] line.
	if !strings.Contains(errText, "ACS Disconnect with error") {
		t.Errorf("ErrorLog missing the alarm-class [TR69] line; got:\n%s", errText)
	}
	if strings.Contains(errText, "[SCM]") {
		t.Errorf("ErrorLog must not carry the non-error operational [SCM] line; got:\n%s", errText)
	}
}

// TestScenarioLineStagedInLogFeed ensures each scenario stages the exact
// module+text its rules.DefaultEngine() rule expects, in the always-uploaded Log
// feed (kept in lockstep by hand with engine.go / config/rules.yaml).
func TestScenarioLineStagedInLogFeed(t *testing.T) {
	cases := map[Scenario]string{
		ScenarioFTPPathReject: "curl code=(25)",
		ScenarioFTPAuthFail:   "curl code=(67)",
		ScenarioFTPConnFail:   "curl code=(7)",
		ScenarioFTPTimeout:    "curl code=(28)",
		ScenarioATCFault:      "RPC Unknown received from ACS",
		ScenarioReboot:        "system reboot will be taken",
	}
	for scenario, want := range cases {
		logGz, _, err := Generate(testConfig(scenario), nil)
		if err != nil {
			t.Fatalf("scenario %s: Generate: %v", scenario, err)
		}
		if got := gunzipText(t, logGz); !strings.Contains(got, want) {
			t.Errorf("scenario %s: Log feed missing staged fault %q", scenario, want)
		}
	}
}
