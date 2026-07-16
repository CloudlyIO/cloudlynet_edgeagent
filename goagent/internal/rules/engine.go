package rules

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"time"

	"cloudlynet_edgeagent/goagent/internal/cloud"
	"gopkg.in/yaml.v3"
)

type Rule struct {
	Module    string `yaml:"module"`
	Match     string `yaml:"match"`
	EventType string `yaml:"event_type"`
	Severity  string `yaml:"severity"`
	Message   string `yaml:"message"`
	re        *regexp.Regexp
}

type Engine struct {
	rules []Rule
}

func Load(path string) (*Engine, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Rules []Rule `yaml:"rules"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return compile(cfg.Rules)
}

func DefaultEngine() *Engine {
	e, _ := compile([]Rule{
		// Real ACS-failure lines live in TR69 (not FM); the vendor IP no longer
		// gates the match — IP-agnostic on module+text (see engine.go doc + plan).
		{Module: "TR69", Match: `ACS (Connect Status = 2|connect failed|Disconnect with error)`, EventType: "vendor_acs_unreachable", Severity: "major", Message: "Vendor ACS unreachable"},
		{Module: "TR69", Match: `RPC Unknown received from ACS`, EventType: "atc_fault_loop", Severity: "major", Message: "ACS returned Fault to ATC"},
		{Module: "FILE_TRANS", Match: `File upload success, curl code=\(0\)`, EventType: "ftp_upload_ok", Severity: "info", Message: "FTP upload succeeded"},
		{Module: "FILE_TRANS", Match: `curl code=\(7\)`, EventType: "ftp_conn_fail", Severity: "major", Message: "FTP connection failed"},
		{Module: "FILE_TRANS", Match: `curl code=\(25\)`, EventType: "ftp_upload_path_reject", Severity: "minor", Message: "FTP upload path rejected"},
		{Module: "FILE_TRANS", Match: `curl code=\(28\)`, EventType: "ftp_upload_timeout", Severity: "major", Message: "FTP upload timed out"},
		{Module: "FILE_TRANS", Match: `curl code=\(67\)`, EventType: "ftp_auth_fail", Severity: "major", Message: "FTP authentication failed"},
		{Module: "FM", Match: `(?i)reboot|restart`, EventType: "device_reboot", Severity: "critical", Message: "Device reboot detected"},
	})
	return e
}

func compile(in []Rule) (*Engine, error) {
	for i := range in {
		re, err := regexp.Compile(in[i].Match)
		if err != nil {
			return nil, err
		}
		in[i].re = re
	}
	return &Engine{rules: in}, nil
}

// moduleTagRe extracts the inline "[MODULE]" tag real NanoLink log lines
// carry (e.g. "... [FILE_TRANS] File upload success ..."). Real logs are
// numbered ring files (1…10/index/max) with all modules interleaved — the
// module lives per LINE, not in the (meaningless) entry filename.
var moduleTagRe = regexp.MustCompile(`\[([A-Za-z0-9_]+)\]`)

// moduleFromLine returns the first inline "[MODULE]" tag in line, or "" if the
// line carries none (a genuinely untagged line only ever hits the alarmy()
// fallback — no rule declares an empty Module).
func moduleFromLine(line string) string {
	m := moduleTagRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}

func (e *Engine) Apply(lines []string, deviceHint string) []cloud.EventItem {
	now := time.Now().UTC().Format(time.RFC3339)
	var out []cloud.EventItem
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		module := moduleFromLine(line)
		matched := false
		for _, r := range e.rules {
			if !strings.EqualFold(r.Module, module) || !r.re.MatchString(line) {
				continue
			}
			out = append(out, event(deviceHint, now, module, r.EventType, r.Severity, r.Message, line))
			matched = true
			break
		}
		if !matched && alarmy(line) {
			out = append(out, event(deviceHint, now, module, "unclassified", "warning", "", line))
		}
	}
	return out
}

func event(device, ts, module, eventType, severity, message, raw string) cloud.EventItem {
	if device == "" {
		device = "unknown"
	}
	return cloud.EventItem{
		CWMPID:    device,
		Timestamp: ts,
		Module:    module,
		EventType: eventType,
		Severity:  severity,
		Message:   message,
		Attrs:     map[string]any{"raw": raw},
		DedupKey:  dedup(device, eventType, raw),
	}
}

func alarmy(line string) bool {
	l := strings.ToLower(line)
	return strings.Contains(l, "alarm") || strings.Contains(l, "fault") || strings.Contains(l, "fail") || strings.Contains(l, "error")
}

// dedup builds a CONTENT-derived dedup key: (device, eventType, raw). It
// deliberately omits parse time — the raw line already carries the device's own
// sequence number + timestamp, so the same log line yields the same key whether
// it arrives via the continuous ring or the Devicelog (they overlap on the real
// device) or is re-parsed after an agent restart. The cloud dedups on this key
// (ON CONFLICT(dedup_key)); a parse-time component would defeat that.
func dedup(device, eventType, raw string) string {
	lineHash := sha1.Sum([]byte(raw))
	h := sha256.Sum256([]byte(device + "|" + eventType + "|" + hex.EncodeToString(lineHash[:])))
	return hex.EncodeToString(h[:])
}
