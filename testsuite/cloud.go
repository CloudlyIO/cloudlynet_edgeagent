// Mock CloudlyNet cloud (:9000): the agent's northbound target
// (register/heartbeat/telemetry/poll/ack/config-snapshot) plus the /health gate
// a human/CI reads to decide pass/fail. acsHealthMux is the lighter health used
// in the acs/acsftp debug modes, where the mock cloud is disabled.
package main

import (
	"encoding/json"
	"fmt"
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
	// canonicalIDs is the expected canonical cwmp_id set (one entry in
	// single-device mode) — the "typed event on canonical id" check matches any
	// of them. Replaces the old package-global deviceCWMPID comparison.
	canonicalIDs map[string]struct{}
	// fleet is non-nil only in fleet mode (devices: list in the conf); it adds
	// per-device tracking + the /health "fleet" section without touching the
	// single-device gate.
	fleet *fleetHealth
}

// fleetHealth tracks per-device onboarding progress, keyed by canonical
// cwmp_id. Guarded by state.mu like everything else in state.
type fleetHealth struct {
	expected  []string // canonical ids in conf order (stable /health output)
	perDevice map[string]*fleetDeviceStat
	// actuation is the configure-command attribution verdict: "pending" until
	// the configure ack lands, then "verified" (the write is present on the
	// TARGETED device's own store) or "misrouted" (acked applied, but the
	// device it was addressed to never received it — the shared-session
	// scrambling failure mode).
	actuation string
}

type fleetDeviceStat struct {
	registered bool // seen in a heartbeat inventory (the agent onboarded it)
	telemetry  int  // metric samples received for this cwmp_id
	events     int  // deduped events received for this cwmp_id
	snapshots  int  // config-snapshot posts for this cwmp_id
	// Attribution counters — presence alone cannot distinguish "device X's data"
	// from "somebody's data filed under X" (the agent used to fold same-IP
	// devices into one CWMP session). Snapshot posts and metric samples are
	// therefore cross-checked against the claimed device's OWN live param store
	// on its static RF identity (PhyCellID/EARFCN); any mismatch is proof of
	// cross-device misattribution and fails the gate.
	snapshotVerified    int // snapshot posts whose RF identity matched the claimed device
	snapshotMismatches  int // snapshot posts carrying ANOTHER device's RF identity
	telemetryMismatches int // metric samples carrying another device's pci/earfcn_inuse
}

func newFleetHealth(canonicalIDs []string) *fleetHealth {
	per := make(map[string]*fleetDeviceStat, len(canonicalIDs))
	for _, id := range canonicalIDs {
		per[id] = &fleetDeviceStat{}
	}
	return &fleetHealth{expected: canonicalIDs, perDevice: per, actuation: "pending"}
}

// snapshotAttributionPaths are the static RF-identity params (both in the
// agent's managed snapshot catalogue) a posted config snapshot is checked
// against. Deliberately NOT ReferenceSignalPower — that is the closed loop's
// actuation knob and legitimately changes mid-run.
var snapshotAttributionPaths = []string{phyCellIDConfigPath, earfcnDLConfigPath}

// telemetryAttribution maps canonical metric keys to the device-store path
// carrying the same value (the agent's T2 reads the *InUse reporting paths).
var telemetryAttribution = map[string]string{
	"pci_inuse":       phyCellIDInUsePath,
	"earfcn_dl_inuse": earfcnDLInUsePath,
}

// deviceByCWMPID finds the in-process mock device a cwmp_id claims to be.
func deviceByCWMPID(devices []*device, id string) *device {
	for _, d := range devices {
		if d.cwmpID == id {
			return d
		}
	}
	return nil
}

// checkAttribution compares posted (path->value) data against the claimed
// device's live store for the given paths. verified means at least one
// attribution path was present and matched; mismatched means at least one
// present path carried a value that is NOT the claimed device's — i.e. the data
// belongs to a different device. Paths absent on either side prove nothing and
// are skipped. Attribution power comes from per-device-unique values (distinct
// PhyCellIDs in the fleet conf — see FLEET.md).
func checkAttribution(dev *device, posted map[string]any, paths []string) (verified, mismatched bool) {
	for _, p := range paths {
		got, ok := posted[p]
		if !ok {
			continue
		}
		want := dev.get([]string{p})
		if len(want) != 1 {
			continue
		}
		if fmt.Sprint(got) == want[0][1] {
			verified = true
		} else {
			mismatched = true
		}
	}
	return verified, mismatched
}

// stateForSpecs wires the fleet-aware fields: canonical id set always, the
// fleet tracker only when the conf actually declared a fleet.
func stateForSpecs(st *state, specs []deviceSpec, fleetMode bool) *state {
	st.canonicalIDs = make(map[string]struct{}, len(specs))
	ids := make([]string, 0, len(specs))
	for _, s := range specs {
		st.canonicalIDs[s.CWMPID] = struct{}{}
		ids = append(ids, s.CWMPID)
	}
	if fleetMode {
		st.fleet = newFleetHealth(ids)
	}
	return st
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

// cloudMux serves the mock cloud. Variadic devices keeps every historical
// single-device call site (cloudMux(st, dev)) compiling and behaving
// byte-for-byte; fleet mode passes the whole fleet. The command fixtures target
// devices[0] in both modes.
func cloudMux(st *state, devices ...*device) http.Handler {
	dev0 := devices[0]
	mux := http.NewServeMux()
	// Fault injection / read-back, in both modes. See deviceParamsMux: with one
	// device it IS the historical deviceParamsHandler; with a fleet it adds an
	// optional ?device=<serial|cwmp_id> selector.
	mux.HandleFunc("/device/params", deviceParamsMux(devices))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		_, sawExpected := st.typedEvents[st.expectedEvent]
		sawExpected = sawExpected || st.expectedEvent == ""
		// The single-device gate — unchanged. Fleet mode ANDs its own condition on
		// top (all N onboarded with telemetry+snapshot) and reports a "fleet"
		// section; single-device output stays byte-for-byte identical.
		ok := st.registered > 0 && st.heartbeats > 0 && st.telemetry > 0 && st.events > 0 &&
			st.failures > 0 && st.snapshots > 0 && len(st.snapshotParams) == managedSnapshotParamCount && len(st.acks) >= 3 &&
			len(st.typedEvents) > 0 && st.sawCanonicalTypedEvent && sawExpected
		typed := make([]string, 0, len(st.typedEvents))
		for t := range st.typedEvents {
			typed = append(typed, t)
		}
		sort.Strings(typed)
		resp := map[string]any{
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
		}
		if st.fleet != nil {
			onboarded := 0
			fleetOK := true
			per := make(map[string]any, len(st.fleet.expected))
			for _, id := range st.fleet.expected {
				s := st.fleet.perDevice[id]
				// complete is attribution-checked, not merely presence-checked: at
				// least one snapshot must carry THIS device's own RF identity, and
				// no snapshot/metric filed under this id may carry another
				// device's — green fleet health implies correctly-attributed
				// per-device data, not just N of everything.
				complete := s.registered && s.telemetry > 0 && s.snapshots > 0 &&
					s.snapshotVerified > 0 && s.snapshotMismatches == 0 && s.telemetryMismatches == 0
				if s.registered {
					onboarded++
				}
				if !complete {
					fleetOK = false
				}
				per[id] = map[string]any{
					"registered":           s.registered,
					"telemetry":            s.telemetry,
					"events":               s.events,
					"snapshots":            s.snapshots,
					"snapshot_verified":    s.snapshotVerified,
					"snapshot_mismatches":  s.snapshotMismatches,
					"telemetry_mismatches": s.telemetryMismatches,
					"complete":             complete,
				}
			}
			if st.fleet.actuation == "misrouted" {
				fleetOK = false
			}
			resp["fleet"] = map[string]any{
				"expected":   len(st.fleet.expected),
				"onboarded":  onboarded,
				"ok":         fleetOK,
				"actuation":  st.fleet.actuation,
				"per_device": per,
			}
			resp["ok"] = ok && fleetOK
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("/v1/agent/register", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.registered++
		st.mu.Unlock()
		envelope(w, map[string]any{"edge_id": "11111111-1111-1111-1111-111111111111"})
	})
	mux.HandleFunc("/v1/agent/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		// The heartbeat carries the agent's device inventory; in fleet mode a
		// device appearing there means the agent onboarded it ("registered").
		var body struct {
			Devices []struct {
				CWMPID string `json:"cwmp_id"`
			} `json:"devices"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.heartbeats++
		if st.fleet != nil {
			for _, d := range body.Devices {
				if s, ok := st.fleet.perDevice[d.CWMPID]; ok {
					s.registered = true
				}
			}
		}
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
			Metrics []struct {
				CWMPID  string         `json:"cwmp_id"`
				Metrics map[string]any `json:"metrics"`
			} `json:"metrics"`
			Events []struct {
				CWMPID    string `json:"cwmp_id"`
				EventType string `json:"event_type"`
				DedupKey  string `json:"dedup_key"`
			} `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.telemetry++
		if st.fleet != nil {
			for _, ms := range body.Metrics {
				s, ok := st.fleet.perDevice[ms.CWMPID]
				if !ok {
					continue
				}
				s.telemetry++
				// Attribution: a sample filed under X carrying another device's
				// in-use PCI/EARFCN is scrambled telemetry, not X's telemetry.
				if dev := deviceByCWMPID(devices, ms.CWMPID); dev != nil {
					for key, path := range telemetryAttribution {
						v, present := ms.Metrics[key]
						if !present {
							continue
						}
						if want := dev.get([]string{path}); len(want) == 1 && fmt.Sprint(v) != want[0][1] {
							s.telemetryMismatches++
						}
					}
				}
			}
		}
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
			if st.fleet != nil {
				if s, ok := st.fleet.perDevice[e.CWMPID]; ok {
					s.events++
				}
			}
			if e.EventType == "" || e.EventType == "unclassified" {
				continue
			}
			st.typedEvents[e.EventType] = struct{}{}
			if _, canonical := st.canonicalIDs[e.CWMPID]; canonical {
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
		if _, ok := st.acks[configureCommandID]; !ok {
			commands = append(commands, configureCommand(dev0.cwmpID))
		}
		if _, ok := st.acks[queryCommandID]; !ok {
			commands = append(commands, queryCommand(dev0.cwmpID))
		}
		if _, ok := st.acks[rebootCommandID]; !ok {
			commands = append(commands, rebootCommand(dev0.cwmpID))
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
			if st.fleet != nil {
				// The path segment is the cwmp_id the agent PathEscape'd; net/http
				// decodes it once, so r.URL.Path carries the literal %2D form —
				// exactly the canonical id.
				id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/agent/devices/"), "/config-snapshot")
				if s, ok := st.fleet.perDevice[id]; ok {
					s.snapshots++
					// Attribution: the snapshot claims to be a read OF this device —
					// its static RF identity must match the device's own live store,
					// or the agent answered the read from a different device.
					if dev := deviceByCWMPID(devices, id); dev != nil {
						verified, mismatched := checkAttribution(dev, body.Params, snapshotAttributionPaths)
						if verified {
							s.snapshotVerified++
						}
						if mismatched {
							s.snapshotMismatches++
						}
					}
				}
			}
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
		if st.fleet != nil && id == configureCommandID && body.Status == "applied" {
			// Actuation attribution: an "applied" ack is only trustworthy if the
			// write actually landed on the device the command ADDRESSED (dev0).
			// Under scrambled per-IP session routing, any device could have
			// executed the SPV and acked — this is the check that catches it.
			if got := dev0.get([]string{refSignalPowerPath}); len(got) == 1 && got[0][1] == configuredRSPower {
				st.fleet.actuation = "verified"
			} else {
				st.fleet.actuation = "misrouted"
			}
		}
		st.mu.Unlock()
		envelope(w, map[string]any{})
	})
	return mux
}

func acsHealthMux(dev *device, ftpDir, mode, agentURL string) http.Handler {
	return acsHealthMuxFleet([]*device{dev}, ftpDir, mode, agentURL)
}

// acsHealthMuxFleet is the acs/acsftp health for N devices. With one device it
// is byte-for-byte the historical acsHealthMux output (no "fleet" key,
// informs_sent is that device's count, ok == informs>0); with a fleet, ok
// requires EVERY device to have informed and a "fleet" section reports
// per-device inform counts. This is the gate SCOPE.md's B2 (14 femtocells
// against the real local stack) watches — the platform mock is off in acsftp.
func acsHealthMuxFleet(devices []*device, ftpDir, mode, agentURL string) http.Handler {
	mux := http.NewServeMux()
	// Fault injection / read-back. This is the mode EPIC-5's e2e and rollback demo run in.
	mux.HandleFunc("/device/params", deviceParamsMux(devices))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		// Count every real upload shape the emulator now emits: the routine
		// periodic feed (Log_*.gz / ErrorLog_*.gz) plus any reboot-dump .tgz.
		logs, _ := filepath.Glob(path.Join(ftpDir, "Log_*.gz"))
		errLogs, _ := filepath.Glob(path.Join(ftpDir, "ErrorLog_*.gz"))
		tgz, _ := filepath.Glob(path.Join(ftpDir, "*.tgz"))
		informed := 0
		var informsTotal int64
		per := make(map[string]any, len(devices))
		for _, d := range devices {
			n := d.informs()
			informsTotal += n
			if n > 0 {
				informed++
			}
			per[d.cwmpID] = n
		}
		resp := map[string]any{
			"ok":            informed == len(devices),
			"mode":          mode,
			"platform_mock": false,
			"cwmp":          map[string]any{"agent_url": agentURL, "informs_sent": informsTotal},
			"ftp":           map[string]any{"dir": ftpDir, "uploads": len(logs) + len(errLogs) + len(tgz)},
		}
		if len(devices) > 1 {
			resp["fleet"] = map[string]any{
				"expected":   len(devices),
				"informed":   informed,
				"per_device": per,
			}
		}
		writeJSON(w, http.StatusOK, resp)
	})
	return mux
}

// Command fixtures — key the device by its canonical cwmp_id (the poll
// handler's target device — devices[0] in both modes; the agent routes CWMP
// delivery by cwmp_id). In fleet mode the configure ack doubles as the
// actuation-attribution probe: the acked write must be present on dev0's own
// store (see the ack handler).
const refSignalPowerPath = "Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower"

const (
	configureCommandID = "00000000-0000-0000-0000-000000000101"
	queryCommandID     = "00000000-0000-0000-0000-000000000102"
	rebootCommandID    = "00000000-0000-0000-0000-000000000103"
	// configuredRSPower is the value configureCommand writes — what the
	// actuation-attribution check expects to find on dev0 after an applied ack.
	configuredRSPower = "-8"
)

func command(cwmpID, id, typ string, payload map[string]any) map[string]any {
	return map[string]any{
		"id": id, "device_id": "00000000-0000-0000-0000-000000000201", "cwmp_id": cwmpID, "type": typ, "payload": payload,
	}
}

func configureCommand(cwmpID string) map[string]any {
	return command(cwmpID, configureCommandID, "configure", map[string]any{
		"writes": []map[string]any{{"path": refSignalPowerPath, "value": configuredRSPower, "xsd_type": "xsd:string"}},
	})
}

func queryCommand(cwmpID string) map[string]any {
	return command(cwmpID, queryCommandID, "query", map[string]any{"read_paths": []string{refSignalPowerPath}})
}

func rebootCommand(cwmpID string) map[string]any {
	return command(cwmpID, rebootCommandID, "reboot", map[string]any{})
}

func envelope(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "timestamp": time.Now().UTC().Format(time.RFC3339), "data": data, "errors": []any{}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
