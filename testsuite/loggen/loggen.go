// Package loggen builds the real-shaped NanoLink log files the mock device
// uploads over FTP each cycle: the routine periodic feed — a single-file gzip
// "Log_<date>.<time>+<tz>_<OUI>.<serial>.gz" (the device's ~60s VendorLog slice)
// plus its error-weighted sibling "ErrorLog_<…>.gz". Content is a redacted real
// sample plus synthetic per-module lines, so every generated file exercises the
// agent's inline "[MODULE]" routing exactly like the genuine device.
//
// These two shapes REPLACE the earlier continuouslogging.tgz ring + bare
// Devicelog as the emulator's routine emission: the real device only dumps those
// on a reboot/power-on/manual pull, whereas Log_*.gz is what it pushes every
// minute. The agent still ingests all shapes (.tgz, bare Devicelog, and now
// Log_*.gz/ErrorLog_*.gz); the emulator now drives the routine path a real box
// actually uses.
//
// Fidelity scope: a fixed corpus replayed each cycle (no live per-minute deltas
// or ring rotation), single device, Log_*.gz + ErrorLog_*.gz only.
package loggen

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
	"time"
)

// The redacted corpus was captured from a real device; these are the identity
// tokens baked into its log CONTENT. rewriteIdentity swaps them for the
// configured device so the FTP logs read consistently (filenames already do).
const (
	corpusOUI          = "8C1F64"
	corpusSerial       = "2205600282"
	corpusProductClass = "ENB-N03002-B3"
)

// Scenario selects which fault-signature line the generator appends, so a
// scenario run has a deterministic typed event to assert on /health.
type Scenario string

const (
	ScenarioHappy         Scenario = "happy"
	ScenarioFTPPathReject Scenario = "ftp-path-reject"
	ScenarioFTPAuthFail   Scenario = "ftp-auth-fail"
	ScenarioFTPConnFail   Scenario = "ftp-conn-fail"
	ScenarioFTPTimeout    Scenario = "ftp-timeout"
	ScenarioATCFault      Scenario = "atc-fault"
	ScenarioReboot        Scenario = "reboot"
)

// scenarioLines maps a scenario to the log line it stages, keyed on the real
// module + text the corresponding rules.DefaultEngine() rule matches (kept in
// lockstep by hand — see engine.go / config/rules.yaml).
var scenarioLines = map[Scenario]string{
	ScenarioHappy:         "[FILE_TRANS] File upload success, curl code=(0), command (curl -T ... -u nybsys:*** ftp://192.168.8.100/)",
	ScenarioFTPPathReject: "[FILE_TRANS] File upload failure, curl code=(25), command (curl -T ... -u nybsys:*** ftp://192.168.8.100/uploads)",
	ScenarioFTPAuthFail:   "[FILE_TRANS] File upload failure, curl code=(67), command (curl -T ... -u nybsys:*** ftp://192.168.8.100/)",
	ScenarioFTPConnFail:   "[FILE_TRANS] File upload failure, curl code=(7), command (curl -T ... -u nybsys:*** ftp://192.168.8.100/)",
	ScenarioFTPTimeout:    "[FILE_TRANS] File upload failure, curl code=(28), command (curl -T ... -u nybsys:*** ftp://192.168.8.100/)",
	ScenarioATCFault:      "[TR69] RPC Unknown received from ACS",
	ScenarioReboot:        "[FM] Critical alarm 0x16010400 raised, system reboot will be taken to recover it after 90s.",
}

// Config parameterizes one generated periodic-feed slice for a single device.
type Config struct {
	OUI          string
	Serial       string
	ProductClass string
	At           time.Time // when this slice is captured/uploaded (per cycle)
	Scenario     Scenario
}

// LogName is the routine periodic feed's filename shape:
// "Log_<YYYYMMDD>.<HHMM>+<tz>_<OUI>.<serial>.gz". The agent extracts OUI+serial
// from the dot-joined tail; the timestamp/tz are cosmetic (the parser ignores
// them). Minute (HHMM) granularity matches the real device.
func (c Config) LogName() string {
	return fmt.Sprintf("Log_%s_%s.%s.gz", c.At.Format("20060102.1504-0700"), c.OUI, c.Serial)
}

// ErrorLogName is the error-slice sibling's filename shape (same tail, "ErrorLog_" prefix).
func (c Config) ErrorLogName() string {
	return fmt.Sprintf("ErrorLog_%s_%s.%s.gz", c.At.Format("20060102.1504-0700"), c.OUI, c.Serial)
}

// syntheticLines fills out module diversity the redacted real sample doesn't
// cover (e.g. NCM/SM), so the generated slice reads like a genuine capture.
func syntheticLines(cfg Config) []string {
	ts := cfg.At.Format("2006-01-02 15:04:05.000")
	return []string{
		fmt.Sprintf("0000000002 %s [NCM] Neighbor cell list updated, count=6", ts),
		fmt.Sprintf("0000000004 %s [SM] Session established, imsi=***, bearer=5", ts),
		fmt.Sprintf("0000000006 %s [SCM] Process(pid=1900) ps_l3 started", ts),
		fmt.Sprintf("0000000008 %s [SON] CurrentSyncMode is 5, sync status is success", ts),
	}
}

// rewriteIdentity swaps the corpus's captured OUI/serial/product-class for the
// configured device's, so the FTP log content matches the emulated identity.
func rewriteIdentity(lines []string, cfg Config) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if cfg.OUI != "" {
			l = strings.ReplaceAll(l, corpusOUI, cfg.OUI)
		}
		if cfg.Serial != "" {
			l = strings.ReplaceAll(l, corpusSerial, cfg.Serial)
		}
		if cfg.ProductClass != "" {
			l = strings.ReplaceAll(l, corpusProductClass, cfg.ProductClass)
		}
		out[i] = l
	}
	return out
}

// Generate builds the periodic-feed slice (Log_*.gz) and its error-weighted
// sibling (ErrorLog_*.gz) for cfg, seeded from sampleLines (the redacted real
// corpus) plus per-module synthetic filler and the scenario's fault line.
//
// Both are single-file gzip (NOT a tar). The ErrorLog is the alarm/error subset
// of the same lines (see errorLogLines); those lines therefore also appear in
// the full Log feed — the real device logs an alarm to both streams, and that
// overlap collapses at the cloud on the content-derived dedup key (rules.dedup).
func Generate(cfg Config, sampleLines []string) (logGz, errorLogGz []byte, err error) {
	scenario := cfg.Scenario
	if scenario == "" {
		scenario = ScenarioHappy
	}
	seq := fmt.Sprintf("%010d", 1)
	scenarioLine := seq + " " + cfg.At.Format("2006-01-02 15:04:05.000") + " " + scenarioLines[scenario]

	sampleLines = rewriteIdentity(sampleLines, cfg)
	lines := make([]string, 0, len(sampleLines)+len(syntheticLines(cfg))+1)
	lines = append(lines, scenarioLine)
	lines = append(lines, sampleLines...)
	lines = append(lines, syntheticLines(cfg)...)

	logGz, err = buildGzipLog(lines)
	if err != nil {
		return nil, nil, err
	}
	errorLogGz, err = buildGzipLog(errorLogLines(lines))
	if err != nil {
		return nil, nil, err
	}
	return logGz, errorLogGz, nil
}

// errorLogLines selects the error/alarm subset for the ErrorLog stream — lines
// whose text carries an alarm/fault/fail/error marker (the same predicate the
// agent's rules.alarmy fallback keys on). The happy-path curl(0) success line is
// deliberately excluded (a success is not an error); background alarms already in
// the corpus keep ErrorLog non-empty even on the happy scenario, as on a real box.
func errorLogLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if isErrorLine(l) {
			out = append(out, l)
		}
	}
	return out
}

func isErrorLine(l string) bool {
	s := strings.ToLower(l)
	return strings.Contains(s, "alarm") || strings.Contains(s, "fault") ||
		strings.Contains(s, "fail") || strings.Contains(s, "error")
}

// buildGzipLog gzip-compresses the given lines into a single file (one line per
// "\n") — the real periodic-feed shape (Log_*.gz / ErrorLog_*.gz), NOT a tar.
func buildGzipLog(lines []string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, l := range lines {
		if _, err := gz.Write([]byte(l + "\n")); err != nil {
			return nil, err
		}
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
