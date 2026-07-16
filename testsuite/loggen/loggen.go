// Package loggen builds real-shaped NanoLink log archives: a gzipped tar
// ("continuouslogging.tgz") with the numbered ring layout (entries 1…10 +
// index/max, no per-entry module semantics) and a bare "Devicelog" upload —
// the two real upload shapes finding A/A′ target. Content is a redacted real
// sample plus synthetic per-module lines, so every generated archive exercises
// the agent's inline "[MODULE]" routing exactly like the genuine device.
//
// Fidelity scope (dev-defaulted, see docs/task_docs .../notes.md): ring layout
// without live rotation/wrap, single device, continuouslogging + Devicelog
// only (no hourly Log_*/ErrorLog_* — real NanoLink modules only, out of scope
// here).
package loggen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// ringSize matches the real device's numbered-entry ring (1…10 + index/max).
const ringSize = 10

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

// Config parameterizes one generated log ring for a single device.
type Config struct {
	OUI          string
	Serial       string
	ProductClass string
	PowerOnAt    time.Time
	Scenario     Scenario
}

// ArchiveName is the real continuouslogging upload's filename shape.
func (c Config) ArchiveName() string {
	return fmt.Sprintf("%s_%s_PowerOn_%s_continuouslogging.tgz", c.OUI, c.Serial, c.PowerOnAt.Format("20060102_150405"))
}

// DeviceLogName is the real bare Devicelog upload's filename shape (no .tgz).
func (c Config) DeviceLogName() string {
	return fmt.Sprintf("%s_%s_PowerOn_%s_Devicelog", c.OUI, c.Serial, c.PowerOnAt.Format("20060102_150405"))
}

// syntheticLines fills out module diversity the redacted real sample doesn't
// cover (e.g. NCM/SM), so the interleaved ring reads like a genuine capture.
func syntheticLines(cfg Config) []string {
	ts := cfg.PowerOnAt.Format("2006-01-02 15:04:05.000")
	return []string{
		fmt.Sprintf("0000000002 %s [NCM] Neighbor cell list updated, count=6", ts),
		fmt.Sprintf("0000000004 %s [SM] Session established, imsi=***, bearer=5", ts),
		fmt.Sprintf("0000000006 %s [SCM] Process(pid=1900) ps_l3 started", ts),
		fmt.Sprintf("0000000008 %s [SON] CurrentSyncMode is 5, sync status is success", ts),
	}
}

// Generate builds the gzipped ring archive and the bare Devicelog for cfg,
// seeded from sampleLines (the redacted real corpus) plus per-module
// synthetic filler and the scenario's fault-signature line.
func Generate(cfg Config, sampleLines []string) (archive []byte, deviceLog []byte, err error) {
	seq := fmt.Sprintf("%010d", 1)
	scenarioLine := seq + " " + cfg.PowerOnAt.Format("2006-01-02 15:04:05.000") + " " + scenarioLines[cfg.Scenario]
	if cfg.Scenario == "" {
		scenarioLine = seq + " " + cfg.PowerOnAt.Format("2006-01-02 15:04:05.000") + " " + scenarioLines[ScenarioHappy]
	}

	lines := make([]string, 0, len(sampleLines)+len(syntheticLines(cfg))+1)
	lines = append(lines, scenarioLine)
	lines = append(lines, sampleLines...)
	lines = append(lines, syntheticLines(cfg)...)

	archive, err = buildRingArchive(lines)
	if err != nil {
		return nil, nil, err
	}
	// The bare Devicelog is the device's ALARM log — a distinct, smaller stream
	// than the full continuous ring, NOT a byte-for-byte copy of it. Emitting
	// identical content to both double-ingested every operational line (the
	// bulk: FILE_TRANS curl lines); the alarm subset keeps each upload shape
	// carrying representative-but-distinct content. Alarm lines legitimately
	// appear in BOTH streams on the real device too (an alarm is in the rolling
	// log and the alarm log) — that residual overlap is the agent's dedup
	// domain, keyed on (device, ts, event, raw).
	deviceLog = buildDeviceLog(alarmLines(lines))
	return archive, deviceLog, nil
}

// alarmModules are the modules the device also writes to its separate alarm
// log (Devicelog): fault management, TR-069/ACS alarms, and SCTP connection
// alarms. FILE_TRANS/SON/SCM/NCM/etc. operational chatter stays in the ring only.
var alarmModules = map[string]bool{"FM": true, "TR69": true, "SCTP": true}

var moduleTagRe = regexp.MustCompile(`\[([A-Za-z0-9_]+)\]`)

// alarmLines returns the alarm-class subset of lines — the Devicelog stream —
// selected by the inline "[MODULE]" tag (the same tag the agent routes on).
func alarmLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if m := moduleTagRe.FindStringSubmatch(l); m != nil && alarmModules[m[1]] {
			out = append(out, l)
		}
	}
	return out
}

// buildRingArchive distributes lines round-robin across numbered ring
// entries "1"…"10" (all modules interleaved, as the real device does), plus
// "index"/"max" bookkeeping entries. No live rotation (single-shot fidelity).
func buildRingArchive(lines []string) ([]byte, error) {
	buckets := make([][]string, ringSize)
	for i, l := range lines {
		b := i % ringSize
		buckets[b] = append(buckets[b], l)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	writeEntry := func(name, body string) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return err
		}
		_, err := tw.Write([]byte(body))
		return err
	}
	for i := 0; i < ringSize; i++ {
		name := strconv.Itoa(i + 1)
		body := ""
		for _, l := range buckets[i] {
			body += l + "\n"
		}
		if err := writeEntry(name, body); err != nil {
			return nil, err
		}
	}
	if err := writeEntry("index", "1\n"); err != nil {
		return nil, err
	}
	if err := writeEntry("max", strconv.Itoa(ringSize)+"\n"); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildDeviceLog concatenates the given (alarm-class) lines uncompressed — the
// real Devicelog carries the identical "<seq> <ts> [MODULE] <msg>" format, just
// uploaded bare (A′) and scoped to the alarm stream (see alarmLines).
func buildDeviceLog(lines []string) []byte {
	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}
