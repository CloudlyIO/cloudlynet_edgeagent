// Package loggen builds the real-shaped NanoLink log files the mock device
// uploads over FTP each cycle: the routine periodic feed — a single-file gzip
// "Log_<date>.<time>+<tz>_<OUI>.<serial>.gz" (the device's ~60s VendorLog slice)
// plus, occasionally, its error-weighted sibling "ErrorLog_<…>.gz".
//
// Fidelity model (see testsuite/README.md):
//   - The corpus is split into a PERSISTENT operational stream (boot / upload
//     success / config / status) and a BURSTY incident stream (curl failures,
//     ACS alarms, reboot, SCTP/SON faults), keyed on isErrorLine.
//   - Every cycle's Log carries the operational stream with an ADVANCING sequence
//     number + this cycle's timestamp (D2 jitter) — so each ~60s slice is a
//     distinct delta, like a real device, not a byte-identical replay. Its
//     upload-log line is repointed at this cycle's own Log filename.
//   - An INCIDENT WINDOW (IsIncidentCycle) opens occasionally: those cycles' Log
//     ALSO carries the incident lines VERBATIM (sticky seq+ts), and an ErrorLog
//     is emitted containing the same incident lines. Because they are byte-
//     identical in both files (and recur identically across windows), the cloud's
//     content-dedup key collapses the overlap — modelling a real device that
//     writes an alarm to both streams and dumps an ErrorLog around an incident.
//   - Between windows the feed is baseline-clean and no ErrorLog is produced.
//
// These shapes REPLACE the reboot-dump continuouslogging.tgz + bare Devicelog as
// the emulator's routine emission (single device; the agent's dump-shape intake
// is parked — see collector.go).
package loggen

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"regexp"
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

// Incident-window schedule (cycle-based; one cycle == one Log upload). The first
// window opens at incidentFirstCycle and spans incidentWindowLen consecutive
// cycles, then recurs every incidentPeriod cycles; between windows the feed is
// baseline-clean. At a ~60s Log cadence this is first-at-~60s, a ~2-minute burst,
// then ~5 minutes quiet — modelling the real device, which uploads an ErrorLog
// around an incident, not on a fixed timer.
const (
	incidentFirstCycle = 1
	incidentWindowLen  = 2
	incidentPeriod     = 7
)

// IsIncidentCycle reports whether cycle falls inside an incident window.
func IsIncidentCycle(cycle int) bool {
	if cycle < incidentFirstCycle {
		return false
	}
	return (cycle-incidentFirstCycle)%incidentPeriod < incidentWindowLen
}

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
// lockstep by hand — see engine.go / config/rules.yaml). The scenario line is
// staged in EVERY cycle's Log (the guaranteed /health signal), incident or not.
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
	At           time.Time // this cycle's upload time
	Scenario     Scenario
	Cycle        int // monotonic 0-based cycle index — drives seq advance + the incident schedule
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
// cover (e.g. NCM/SM). They ride the operational stream (fresh timestamp each
// cycle).
func syntheticLines(cfg Config) []string {
	ts := cfg.At.Format("2006-01-02 15:04:05.000")
	return []string{
		fmt.Sprintf("0000000002 %s [NCM] Neighbor cell list updated, count=6", ts),
		fmt.Sprintf("0000000004 %s [SM] Session established, imsi=***, bearer=5", ts),
		fmt.Sprintf("0000000006 %s [SCM] Process(pid=1900) ps_l3 started", ts),
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

// isErrorLine reports whether a line carries an alarm/fault/fail/error marker —
// the same predicate the agent's rules.alarmy fallback keys on. It partitions
// the corpus into the bursty incident stream (true) and the persistent
// operational stream (false).
func isErrorLine(l string) bool {
	s := strings.ToLower(l)
	return strings.Contains(s, "alarm") || strings.Contains(s, "fault") ||
		strings.Contains(s, "fail") || strings.Contains(s, "error")
}

// splitCorpus partitions sample lines into the persistent operational stream and
// the bursty incident (error/alarm) stream.
func splitCorpus(lines []string) (operational, incident []string) {
	for _, l := range lines {
		if isErrorLine(l) {
			incident = append(incident, l)
		} else {
			operational = append(operational, l)
		}
	}
	return operational, incident
}

// embeddedLogNameRe matches a "/tmp/Log_<…>.gz" reference inside a curl-command
// line. Anchored on "/tmp/Log_" so it never touches an "/tmp/ErrorLog_" ref.
var embeddedLogNameRe = regexp.MustCompile(`/tmp/Log_[0-9]{8}\.[0-9]{4}[+-][0-9]{4}_[0-9A-Fa-f]+\.[0-9]+\.gz`)

// refreshEmbeddedLogName repoints an operational upload-log line at THIS cycle's
// Log filename — as a real device's curl-success line references the file it just
// uploaded, keeping the content self-consistent per cycle.
func refreshEmbeddedLogName(line, name string) string {
	return embeddedLogNameRe.ReplaceAllString(line, "/tmp/"+name)
}

// restamp rewrites a line's leading seq + timestamp (the D2 jitter) so an
// operational line becomes a fresh delta each cycle. seq keeps the 10-digit
// width; the message (module + text) is untouched.
func restamp(line string, seq int64, at time.Time) string {
	parts := strings.SplitN(line, " ", 4) // seq, date, time, rest
	if len(parts) < 4 {
		return line
	}
	return fmt.Sprintf("%010d %s %s", seq, at.Format("2006-01-02 15:04:05.000"), parts[3])
}

// Generate builds this cycle's Log slice (and, during an incident window, its
// ErrorLog) from the redacted corpus. On a baseline (non-incident) cycle
// errorLogGz is nil. See the package doc for the incident/jitter model.
func Generate(cfg Config, sampleLines []string) (logGz, errorLogGz []byte, err error) {
	scenario := cfg.Scenario
	if scenario == "" {
		scenario = ScenarioHappy
	}
	scenarioLine := fmt.Sprintf("%010d %s %s", 1, cfg.At.Format("2006-01-02 15:04:05.000"), scenarioLines[scenario])

	sampleLines = rewriteIdentity(sampleLines, cfg)
	operational, incident := splitCorpus(sampleLines)

	// Operational stream: advancing seq (climbs with the cycle) + this cycle's
	// timestamp, upload-log line repointed at this cycle's Log filename.
	logName := cfg.LogName()
	seqBase := int64(100 + cfg.Cycle*100)
	lines := make([]string, 0, len(operational)+len(incident)+len(syntheticLines(cfg))+1)
	lines = append(lines, scenarioLine)
	for i, l := range operational {
		lines = append(lines, restamp(refreshEmbeddedLogName(l, logName), seqBase+int64(i), cfg.At))
	}
	lines = append(lines, syntheticLines(cfg)...)

	// Incident window: append the incident lines VERBATIM (sticky) to the Log and
	// emit them as the ErrorLog — identical bytes in both → content-dedup fires.
	var errLines []string
	if IsIncidentCycle(cfg.Cycle) {
		lines = append(lines, incident...)
		errLines = append(errLines, incident...)
		if isErrorLine(scenarioLine) {
			errLines = append(errLines, scenarioLine)
		}
	}

	logGz, err = buildGzipLog(lines)
	if err != nil {
		return nil, nil, err
	}
	if len(errLines) > 0 {
		errorLogGz, err = buildGzipLog(errLines)
		if err != nil {
			return nil, nil, err
		}
	}
	return logGz, errorLogGz, nil
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
