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
		{Module: "FM", Match: `ACS .*124\.93\.160\.157.*unreachable|connect.*124\.93\.160\.157`, EventType: "vendor_acs_unreachable", Severity: "major", Message: "Hardcoded vendor ACS unreachable"},
		{Module: "TR69", Match: `RPC Unknown received from ACS`, EventType: "atc_fault_loop", Severity: "major", Message: "ACS returned Fault to ATC"},
		{Module: "FILE_TRANS", Match: `File upload success, curl code=\(0\)`, EventType: "ftp_upload_ok", Severity: "info", Message: "FTP upload succeeded"},
		{Module: "FILE_TRANS", Match: `curl code=\(25\)`, EventType: "ftp_upload_path_reject", Severity: "minor", Message: "FTP upload path rejected"},
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

func (e *Engine) Apply(module string, lines []string, deviceHint string) []cloud.EventItem {
	now := time.Now().UTC().Format(time.RFC3339)
	var out []cloud.EventItem
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
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
		GenieACSID: device,
		Timestamp:  ts,
		Module:     module,
		EventType:  eventType,
		Severity:   severity,
		Message:    message,
		Attrs:      map[string]any{"raw": raw},
		DedupKey:   dedup(device, ts, eventType, raw),
	}
}

func alarmy(line string) bool {
	l := strings.ToLower(line)
	return strings.Contains(l, "alarm") || strings.Contains(l, "fault") || strings.Contains(l, "fail") || strings.Contains(l, "error")
}

func dedup(device, ts, eventType, raw string) string {
	lineHash := sha1.Sum([]byte(raw))
	h := sha256.Sum256([]byte(device + "|" + ts + "|" + eventType + "|" + hex.EncodeToString(lineHash[:])))
	return hex.EncodeToString(h[:])
}
