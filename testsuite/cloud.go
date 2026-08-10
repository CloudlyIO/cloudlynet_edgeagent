// Mock CloudlyNet cloud (:9000): the agent's northbound target
// (register/heartbeat/telemetry/poll/ack/config-snapshot) plus the /health gate
// a human/CI reads to decide pass/fail. acsHealthMux is the lighter health used
// in the acs/acsftp debug modes, where the mock cloud is disabled.
package main

import (
	"encoding/json"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cloudlynet_edgeagent/testsuite/loggen"
)

type state struct {
	mu             sync.Mutex
	registered     int
	heartbeats     int
	telemetry      int
	events         int
	failures       int
	snapshots      int
	snapshotParams map[string]any
	acks           map[string]string
	// typedEvents/sawCanonicalTypedEvent are the Health gate extension: proof
	// that A+B actually landed (real-log lines classify to a typed event —
	// never "unclassified" — keyed on the canonical id, never the bare OUI or
	// "unknown").
	typedEvents            map[string]struct{}
	sawCanonicalTypedEvent bool
	// expectedEvent is the typed event the selected scenario must produce — the
	// gate asserts it specifically (not just "some typed event"), so a scenario
	// whose own signal silently stopped classifying can't still go green.
	expectedEvent string
	// seenDedup dedups by dedup_key like the real cloud (ON CONFLICT(dedup_key)),
	// so an alarm arriving via both the ring and the Devicelog — or re-emitted
	// after an agent restart — is counted once, not double.
	seenDedup map[string]struct{}
}

// scenarioEventType maps each scenario to the typed event its staged fault line
// must classify to (kept in lockstep with loggen.scenarioLines / the rules).
var scenarioEventType = map[loggen.Scenario]string{
	loggen.ScenarioHappy:         "ftp_upload_ok",
	loggen.ScenarioFTPPathReject: "ftp_upload_path_reject",
	loggen.ScenarioFTPAuthFail:   "ftp_auth_fail",
	loggen.ScenarioFTPConnFail:   "ftp_conn_fail",
	loggen.ScenarioFTPTimeout:    "ftp_upload_timeout",
	loggen.ScenarioATCFault:      "atc_fault_loop",
	loggen.ScenarioReboot:        "device_reboot",
}

const managedSnapshotParamCount = 24

func cloudMux(st *state, dev *device) http.Handler {
	mux := http.NewServeMux()
	// Fault injection / read-back, in both modes. See deviceParamsHandler.
	mux.HandleFunc("/device/params", deviceParamsHandler(dev))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		_, sawExpected := st.typedEvents[st.expectedEvent]
		sawExpected = sawExpected || st.expectedEvent == ""
		ok := st.registered > 0 && st.heartbeats > 0 && st.telemetry > 0 && st.events > 0 &&
			st.failures > 0 && st.snapshots > 0 && len(st.snapshotParams) == managedSnapshotParamCount && len(st.acks) >= 3 &&
			len(st.typedEvents) > 0 && st.sawCanonicalTypedEvent && sawExpected
		typed := make([]string, 0, len(st.typedEvents))
		for t := range st.typedEvents {
			typed = append(typed, t)
		}
		sort.Strings(typed)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                       ok,
			"registered":               st.registered,
			"heartbeats":               st.heartbeats,
			"telemetry":                st.telemetry,
			"events":                   st.events,
			"failures":                 st.failures,
			"snapshots":                st.snapshots,
			"snapshot_params":          len(st.snapshotParams),
			"acks":                     st.acks,
			"typed_events":             typed,
			"typed_event_on_canonical": st.sawCanonicalTypedEvent,
			"expected_event":           st.expectedEvent,
			"saw_expected_event":       sawExpected,
		})
	})
	mux.HandleFunc("/v1/agent/register", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.registered++
		st.mu.Unlock()
		envelope(w, map[string]any{"edge_id": "11111111-1111-1111-1111-111111111111"})
	})
	mux.HandleFunc("/v1/agent/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.heartbeats++
		st.mu.Unlock()
		envelope(w, map[string]any{})
	})
	mux.HandleFunc("/v1/agent/telemetry", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		if st.failures == 0 {
			// Force one failure to exercise the agent's outbox retry.
			st.failures++
			st.mu.Unlock()
			http.Error(w, "forced telemetry failure", http.StatusServiceUnavailable)
			return
		}
		st.mu.Unlock()
		var body struct {
			Events []struct {
				CWMPID    string `json:"cwmp_id"`
				EventType string `json:"event_type"`
				DedupKey  string `json:"dedup_key"`
			} `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.telemetry++
		for _, e := range body.Events {
			// Dedup by dedup_key like the real cloud (ON CONFLICT(dedup_key)): an
			// alarm line arrives via both the ring and the Devicelog, and a
			// restart re-parses the watch dir — the content-derived key (see
			// rules.dedup) collapses those here instead of double-counting.
			if e.DedupKey != "" {
				if _, dup := st.seenDedup[e.DedupKey]; dup {
					continue
				}
				st.seenDedup[e.DedupKey] = struct{}{}
			}
			st.events++
			if e.EventType == "" || e.EventType == "unclassified" {
				continue
			}
			st.typedEvents[e.EventType] = struct{}{}
			if e.CWMPID == deviceCWMPID {
				st.sawCanonicalTypedEvent = true
			}
		}
		st.mu.Unlock()
		envelope(w, map[string]any{"metrics": 1, "events": 1})
	})
	mux.HandleFunc("/v1/agent/poll", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		commands := []map[string]any{}
		if _, ok := st.acks["00000000-0000-0000-0000-000000000101"]; !ok {
			commands = append(commands, configureCommand())
		}
		if _, ok := st.acks["00000000-0000-0000-0000-000000000102"]; !ok {
			commands = append(commands, queryCommand())
		}
		if _, ok := st.acks["00000000-0000-0000-0000-000000000103"]; !ok {
			commands = append(commands, rebootCommand())
		}
		envelope(w, map[string]any{"server_time": time.Now().UTC().Format(time.RFC3339), "commands": commands})
	})
	mux.HandleFunc("/v1/agent/devices/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config-snapshot") {
			var body struct {
				Params map[string]any `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			st.mu.Lock()
			st.snapshots++
			for p, value := range body.Params {
				st.snapshotParams[p] = value
			}
			st.mu.Unlock()
			envelope(w, map[string]any{})
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/v1/agent/commands/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/ack") {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/agent/commands/"), "/ack")
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.acks[id] = body.Status
		st.mu.Unlock()
		envelope(w, map[string]any{})
	})
	return mux
}

func acsHealthMux(dev *device, ftpDir, mode, agentURL string) http.Handler {
	mux := http.NewServeMux()
	// Fault injection / read-back. This is the mode EPIC-5's e2e and rollback demo run in.
	mux.HandleFunc("/device/params", deviceParamsHandler(dev))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		// Count every real upload shape the emulator now emits: the routine
		// periodic feed (Log_*.gz / ErrorLog_*.gz) plus any reboot-dump .tgz.
		logs, _ := filepath.Glob(path.Join(ftpDir, "Log_*.gz"))
		errLogs, _ := filepath.Glob(path.Join(ftpDir, "ErrorLog_*.gz"))
		tgz, _ := filepath.Glob(path.Join(ftpDir, "*.tgz"))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            dev.informs() > 0,
			"mode":          mode,
			"platform_mock": false,
			"cwmp":          map[string]any{"agent_url": agentURL, "informs_sent": dev.informs()},
			"ftp":           map[string]any{"dir": ftpDir, "uploads": len(logs) + len(errLogs) + len(tgz)},
		})
	})
	return mux
}

// Command fixtures — key the device by its canonical cwmp_id. The agent maps
// cwmp_id -> device IP for CWMP delivery.
const refSignalPowerPath = "Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower"

func command(id, typ string, payload map[string]any) map[string]any {
	return map[string]any{
		"id": id, "device_id": "00000000-0000-0000-0000-000000000201", "cwmp_id": deviceCWMPID, "type": typ, "payload": payload,
	}
}

func configureCommand() map[string]any {
	return command("00000000-0000-0000-0000-000000000101", "configure", map[string]any{
		"writes": []map[string]any{{"path": refSignalPowerPath, "value": "-8", "xsd_type": "xsd:string"}},
	})
}

func queryCommand() map[string]any {
	return command("00000000-0000-0000-0000-000000000102", "query", map[string]any{"read_paths": []string{refSignalPowerPath}})
}

func rebootCommand() map[string]any {
	return command("00000000-0000-0000-0000-000000000103", "reboot", map[string]any{})
}

func envelope(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "timestamp": time.Now().UTC().Format(time.RFC3339), "data": data, "errors": []any{}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
