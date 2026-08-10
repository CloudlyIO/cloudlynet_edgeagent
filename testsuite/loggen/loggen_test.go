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
		OUI: "8C1F64", Serial: "2205609999", ProductClass: "ENB-N03002-B3",
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

const (
	opLine    = "0000000057 2024-06-02 11:25:29.190 [SCM] Process(pid=1843) tr69c started"                   // operational (non-error)
	alarmLine = "0000000030 2024-06-02 07:07:38.097 [TR69] Alarm Report detail: ACS Disconnect with error 1" // incident (error)
	uploadOK  = "0000001022 2024-06-03 00:42:14.467 [FILE_TRANS] File upload success, curl code=(0), command (curl -T /tmp/Log_20240603.0042+0800_8C1F64.2205609999.gz ftp://192.168.8.100/)"
)

// TestLogAndErrorLogNamesMatchRealShape locks the routine periodic-feed filename
// shapes: "Log_<date>.<time>+<tz>_<OUI>.<serial>.gz" and the "ErrorLog_" sibling
// (single-file .gz, never a .tgz tar).
func TestLogAndErrorLogNamesMatchRealShape(t *testing.T) {
	cfg := testConfig(ScenarioHappy)
	if got, want := cfg.LogName(), "Log_20240602.2350+0000_8C1F64.2205609999.gz"; got != want {
		t.Errorf("LogName() = %q, want %q", got, want)
	}
	if got, want := cfg.ErrorLogName(), "ErrorLog_20240602.2350+0000_8C1F64.2205609999.gz"; got != want {
		t.Errorf("ErrorLogName() = %q, want %q", got, want)
	}
	for _, n := range []string{cfg.LogName(), cfg.ErrorLogName()} {
		if strings.HasSuffix(n, ".tgz") {
			t.Errorf("%q must be a single-file .gz, not a .tgz tar", n)
		}
	}
}

// TestIsIncidentCycleSchedule locks the incident-window schedule: baseline first,
// then a 2-cycle burst recurring every incidentPeriod cycles.
func TestIsIncidentCycleSchedule(t *testing.T) {
	want := map[int]bool{0: false, 1: true, 2: true, 3: false, 6: false, 7: false, 8: true, 9: true, 10: false}
	for cycle, exp := range want {
		if got := IsIncidentCycle(cycle); got != exp {
			t.Errorf("IsIncidentCycle(%d) = %v, want %v", cycle, got, exp)
		}
	}
}

// TestBaselineCycleIsCleanNoErrorLog: a non-incident cycle emits NO ErrorLog and
// its Log carries no incident (error/alarm) line — only the operational stream +
// the (non-error) scenario signal.
func TestBaselineCycleIsCleanNoErrorLog(t *testing.T) {
	cfg := testConfig(ScenarioHappy) // Cycle 0 → baseline
	logGz, errGz, err := Generate(cfg, []string{opLine, alarmLine})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if errGz != nil {
		t.Errorf("baseline cycle must not emit an ErrorLog")
	}
	logText := gunzipText(t, logGz)
	if !strings.Contains(logText, "[SCM]") {
		t.Errorf("baseline Log missing the operational line")
	}
	if strings.Contains(logText, "ACS Disconnect with error") {
		t.Errorf("baseline Log must NOT carry the incident line (it belongs to an incident window)")
	}
}

// TestIncidentCycleEmitsCorrelatedErrorLog: an incident cycle emits an ErrorLog
// = the incident lines, those same lines also appear in the Log (correlation →
// dedup), and the operational line stays out of the ErrorLog.
func TestIncidentCycleEmitsCorrelatedErrorLog(t *testing.T) {
	cfg := testConfig(ScenarioHappy)
	cfg.Cycle = 1 // incident window
	logGz, errGz, err := Generate(cfg, []string{opLine, alarmLine})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if errGz == nil {
		t.Fatalf("incident cycle must emit an ErrorLog")
	}
	logText, errText := gunzipText(t, logGz), gunzipText(t, errGz)
	if !strings.Contains(logText, "ACS Disconnect with error") {
		t.Errorf("incident Log must carry the incident line")
	}
	if !strings.Contains(errText, "ACS Disconnect with error") {
		t.Errorf("ErrorLog must carry the incident line")
	}
	if strings.Contains(errText, "[SCM]") {
		t.Errorf("ErrorLog must not carry the operational line")
	}
	// Correlation/dedup: the incident line is byte-identical in both files.
	if !strings.Contains(logText, alarmLine) || !strings.Contains(errText, alarmLine) {
		t.Errorf("incident line must be verbatim (sticky) in both Log and ErrorLog for content-dedup")
	}
}

// TestJitterOperationalFreshErrorSticky: across two cycles the operational line
// is re-stamped (fresh seq+ts → distinct), while the incident line stays verbatim
// (sticky → dedup anchor).
func TestJitterOperationalFreshErrorSticky(t *testing.T) {
	a := testConfig(ScenarioHappy)
	a.Cycle = 1
	b := testConfig(ScenarioHappy)
	b.Cycle = 2
	b.At = a.At.Add(60 * time.Second)

	la, _, err := Generate(a, []string{opLine, alarmLine})
	if err != nil {
		t.Fatalf("Generate a: %v", err)
	}
	lb, _, err := Generate(b, []string{opLine, alarmLine})
	if err != nil {
		t.Fatalf("Generate b: %v", err)
	}
	ta, tb := gunzipText(t, la), gunzipText(t, lb)

	// Operational "tr69c started" line: the ORIGINAL verbatim must NOT survive in
	// either cycle (it was re-stamped), and the two cycles differ.
	if strings.Contains(ta, opLine) || strings.Contains(tb, opLine) {
		t.Errorf("operational line must be re-stamped, not emitted verbatim")
	}
	if lineWith(ta, "tr69c started") == lineWith(tb, "tr69c started") {
		t.Errorf("operational line must differ across cycles (jitter): %q", lineWith(ta, "tr69c started"))
	}
	// Incident alarm line: verbatim (sticky) and identical across cycles.
	if !strings.Contains(ta, alarmLine) || !strings.Contains(tb, alarmLine) {
		t.Errorf("incident line must stay verbatim (sticky) for dedup across cycles")
	}
}

// TestFilenameFieldTouch: an operational upload-log line's embedded Log filename
// is repointed at THIS cycle's LogName.
func TestFilenameFieldTouch(t *testing.T) {
	cfg := testConfig(ScenarioHappy)
	logGz, _, err := Generate(cfg, []string{uploadOK})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	logText := gunzipText(t, logGz)
	if !strings.Contains(logText, "/tmp/"+cfg.LogName()) {
		t.Errorf("upload-log line not repointed at this cycle's LogName %q; got:\n%s", cfg.LogName(), logText)
	}
	if strings.Contains(logText, "Log_20240603.0042+0800") {
		t.Errorf("stale embedded Log filename survived the filename touch")
	}
}

// TestScenarioLineStagedEveryCycle ensures each scenario's signature line is
// staged in the Log every cycle (the guaranteed /health signal), incident or not.
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
		cfg := testConfig(scenario) // Cycle 0 (baseline) — signal must still appear
		logGz, _, err := Generate(cfg, nil)
		if err != nil {
			t.Fatalf("scenario %s: Generate: %v", scenario, err)
		}
		if got := gunzipText(t, logGz); !strings.Contains(got, want) {
			t.Errorf("scenario %s: Log feed missing staged fault %q", scenario, want)
		}
	}
}

// lineWith returns the first line of text containing sub (for per-line compares).
func lineWith(text, sub string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}
