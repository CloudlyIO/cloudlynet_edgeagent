// Command testsuite drives a local functional test of the edge agent.
//
// Post-CWMP-cutover the agent IS the ACS: it binds :7547 and the device dials
// it. So this suite plays two roles:
//   - the mock CloudlyNet cloud on :9000 (register/heartbeat/telemetry/poll/ack/
//     config-snapshot), with a health gate a human/CI reads via GET /health
//   - a mock NanoLink CWMP device that dials the agent's :7547 on a fast loop
//     (Inform -> ATC -> drain the ACS's queued GPV/SPV/GPN/Reboot), plus a
//     connection-request listener on :30005 that pokes an immediate dial.
//
// The critical behaviour under test: the agent answers AutonomousTransferComplete
// with an empty response (never a Fault), so the session survives to the ACS's
// read/write turn — the entire reason the CWMP epic exists.
package main

import (
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
}

const managedSnapshotParamCount = 24

// Device identity is config-driven (nanolinkConfig.Identity) — these vars are
// set once in main() before any goroutine starts, then read everywhere the
// old hardcoded consts used to be.
var (
	deviceOUI          string
	deviceProductClass string
	deviceSerial       string
	deviceCWMPID       string // canonical (%2D-encoded) id, derived from identity
)

//go:embed fixtures/real_sample.log
var realSampleFixture string

// sampleLines returns the redacted real-log corpus lines loggen replays
// alongside its synthetic per-module filler (decision D3: both).
func sampleLines() []string {
	var out []string
	for _, l := range strings.Split(realSampleFixture, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func main() {
	mode := strings.ToLower(strings.TrimSpace(env("TESTSUITE_MODE", "full")))

	cfg, err := loadNanolinkConfig(env("NANOLINK_CONF", "conf/nanolink.conf"))
	if err != nil {
		log.Fatalf("nanolink config load failed: %v", err)
	}
	cfg = cfg.withEnvOverrides()
	deviceOUI, deviceProductClass, deviceSerial = cfg.Identity.OUI, cfg.Identity.ProductClass, cfg.Identity.Serial
	deviceCWMPID = canonicalID(deviceOUI, deviceProductClass, deviceSerial)

	m, err := loadManifest()
	if err != nil {
		log.Fatalf("nanolink manifest load failed: %v", err)
	}

	st := &state{acks: map[string]string{}, snapshotParams: map[string]any{}, typedEvents: map[string]struct{}{}}
	dev := newDevice(m)

	ftpDir := env("FTP_DIR", "/ftp")
	_ = os.MkdirAll(ftpDir, 0o755)
	ftpCfg := ftpUploadConfig{
		host:     cfg.FTP.Host,
		user:     cfg.FTP.User,
		pass:     cfg.FTP.Pass,
		interval: cfg.FTP.UploadInterval,
		scenario: loggen.Scenario(cfg.Scenario),
	}

	// The mock device dials the agent's ACS. It retries until the agent is up.
	agentURL := env("AGENT_CWMP_URL", "http://cloudlynet-edgeagent:7547/")
	dialNow := make(chan struct{}, 1)
	go runConnRequestListener(":30005", dialNow)
	go runDeviceLoop(agentURL, dev, dialNow, m)

	// The device Informs (onboards) before its first log upload — the agent's
	// device-id resolution depends on cwmp_devices being populated first.
	go runFTPUploadLoop(dev, ftpCfg)

	if mode == "acsftp" || mode == "acs" {
		log.Printf("mock cwmp-device+ftp health listening on :9000 (platform mock disabled); dialing agent at %s", agentURL)
		log.Fatal(http.ListenAndServe(":9000", acsHealthMux(dev, ftpDir, mode, agentURL)))
	}
	log.Printf("mock cloud listening on :9000; mock device dialing agent at %s", agentURL)
	log.Fatal(http.ListenAndServe(":9000", cloudMux(st)))
}

// ── mock cloud (:9000) ──────────────────────────────────────────────────────

func cloudMux(st *state) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		ok := st.registered > 0 && st.heartbeats > 0 && st.telemetry > 0 && st.events > 0 &&
			st.failures > 0 && st.snapshots > 0 && len(st.snapshotParams) == managedSnapshotParamCount && len(st.acks) >= 3 &&
			len(st.typedEvents) > 0 && st.sawCanonicalTypedEvent
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
			} `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.telemetry++
		st.events += len(body.Events)
		for _, e := range body.Events {
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
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		archives, _ := filepath.Glob(path.Join(ftpDir, "*.tgz"))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            dev.informs() > 0,
			"mode":          mode,
			"platform_mock": false,
			"cwmp":          map[string]any{"agent_url": agentURL, "informs_sent": dev.informs()},
			"ftp":           map[string]any{"dir": ftpDir, "archives": len(archives)},
		})
	})
	return mux
}

// Command fixtures — key the device by its canonical cwmp_id. The agent maps
// cwmp_id -> device IP for CWMP delivery.
func configureCommand() map[string]any {
	return map[string]any{
		"id": "00000000-0000-0000-0000-000000000101", "device_id": "00000000-0000-0000-0000-000000000201", "cwmp_id": deviceCWMPID, "type": "configure",
		"payload": map[string]any{"writes": []map[string]any{{"path": "Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower", "value": "-8", "xsd_type": "xsd:string"}}},
	}
}

func queryCommand() map[string]any {
	return map[string]any{
		"id": "00000000-0000-0000-0000-000000000102", "device_id": "00000000-0000-0000-0000-000000000201", "cwmp_id": deviceCWMPID, "type": "query",
		"payload": map[string]any{"read_paths": []string{"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower"}},
	}
}

func rebootCommand() map[string]any {
	return map[string]any{
		"id": "00000000-0000-0000-0000-000000000103", "device_id": "00000000-0000-0000-0000-000000000201", "cwmp_id": deviceCWMPID, "type": "reboot",
		"payload": map[string]any{},
	}
}

// ── mock NanoLink CWMP device (dials the agent's :7547) ─────────────────────

type device struct {
	mu          sync.Mutex
	params      map[string]string
	informsSent int64
}

// newDevice seeds the mock device's entire param store from the NanoLink
// manifest (20,260 params) instead of the ~33 hand-picked values it used to
// hardcode (finding D) — a GetParameterValues for any managed path now
// returns a realistic value + xsi:type (see manifest.go's xsdType). Only
// fields the manifest doesn't carry at all (test-harness IPs) are overridden;
// identity stays in lockstep with the active config (main()'s deviceOUI/
// deviceProductClass/deviceSerial), never the manifest's own snapshot values.
func newDevice(m *manifest) *device {
	params := m.seedParams()
	params["Device.LAN.IPAddress"] = "192.168.8.248"
	params["Device.WAN.IPAddress"] = "10.0.0.10"
	params["Device.DeviceInfo.SerialNumber"] = deviceSerial
	params["Device.DeviceInfo.ProductClass"] = deviceProductClass
	return &device{params: params}
}

func (d *device) get(paths []string) [][2]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][2]string, 0, len(paths))
	for _, p := range paths {
		if v, ok := d.params[p]; ok {
			out = append(out, [2]string{p, v})
		}
	}
	return out
}

func (d *device) set(writes [][2]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, w := range writes {
		d.params[w[0]] = w[1]
	}
}

func (d *device) incInform() { atomic.AddInt64(&d.informsSent, 1) }
func (d *device) informs() int64 {
	return atomic.LoadInt64(&d.informsSent)
}

func runConnRequestListener(addr string, dialNow chan<- struct{}) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The agent's connection request pokes an immediate out-of-cycle dial.
		select {
		case dialNow <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("connection-request listener stopped: %v", err)
	}
}

func runDeviceLoop(agentURL string, dev *device, dialNow <-chan struct{}, m *manifest) {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		runSession(client, agentURL, dev, m)
		select {
		case <-ticker.C:
		case <-dialNow:
		}
	}
}

// runSession performs one CWMP session against the agent: Inform -> GetRPCMethods
// -> TransferComplete -> ATC -> drain the ACS's queued tasks -> 204. GetRPCMethods
// and TransferComplete are device-initiated notifications the agent's CWMP
// session already answers (session.go); sending them exercises that surface
// (ST-4 "harden") without disturbing the task-queue drain loop below.
func runSession(client *http.Client, agentURL string, dev *device, m *manifest) {
	msgID := nextMsgID()
	if _, ok := post(client, agentURL, informEnvelope(msgID, dev)); !ok {
		return // agent not up yet; retry next tick
	}
	dev.incInform()

	post(client, agentURL, getRPCMethodsEnvelope(msgID))
	post(client, agentURL, transferCompleteEnvelope(msgID))

	atc, _ := post(client, agentURL, atcEnvelope(msgID))
	if strings.Contains(strings.ToLower(atc), "fault") {
		log.Printf("REGRESSION: agent answered AutonomousTransferComplete with a Fault:\n%s", atc)
	}

	for i := 0; i < 64; i++ {
		body, ok := post(client, agentURL, "") // empty POST = "your turn, ACS"
		if !ok || strings.TrimSpace(body) == "" {
			return // 204 -> nothing queued -> session complete
		}
		req := parseACSRequest(body)
		switch req.kind {
		case "gpv":
			post(client, agentURL, gpvResponseEnvelope(msgID, dev.get(req.paths), m))
		case "spv":
			dev.set(req.writes)
			post(client, agentURL, spvResponseEnvelope(msgID))
		case "gpn":
			prefix := ""
			if len(req.paths) > 0 {
				prefix = req.paths[0]
			}
			post(client, agentURL, gpnResponseEnvelope(msgID, dev, prefix))
		case "reboot":
			post(client, agentURL, rebootResponseEnvelope(msgID))
		default:
			return // unrecognised ACS message
		}
	}
}

// post sends body (empty = the "ACS turn" POST) and returns the response text.
// ok=false means the request failed (agent unreachable) — distinct from a 204.
func post(client *http.Client, url, body string) (string, bool) {
	resp, err := client.Post(url, `text/xml; charset="utf-8"`, strings.NewReader(body))
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), true
}

// acsRequest is the decoded ACS→CPE message the device must answer.
type acsRequest struct {
	kind   string // gpv | spv | gpn | reboot | ""
	paths  []string
	writes [][2]string
}

func parseACSRequest(body string) acsRequest {
	var env struct {
		Body struct {
			GetParameterValues *struct {
				Names []string `xml:"ParameterNames>string"`
			} `xml:"GetParameterValues"`
			SetParameterValues *struct {
				Params []struct {
					Name  string `xml:"Name"`
					Value string `xml:"Value"`
				} `xml:"ParameterList>ParameterValueStruct"`
			} `xml:"SetParameterValues"`
			GetParameterNames *struct {
				Path string `xml:"ParameterPath"`
			} `xml:"GetParameterNames"`
			Reboot *struct{} `xml:"Reboot"`
		} `xml:"Body"`
	}
	dec := xml.NewDecoder(strings.NewReader(body))
	dec.Strict = false
	if err := dec.Decode(&env); err != nil {
		return acsRequest{}
	}
	switch {
	case env.Body.GetParameterValues != nil:
		return acsRequest{kind: "gpv", paths: env.Body.GetParameterValues.Names}
	case env.Body.SetParameterValues != nil:
		writes := make([][2]string, 0, len(env.Body.SetParameterValues.Params))
		for _, p := range env.Body.SetParameterValues.Params {
			writes = append(writes, [2]string{p.Name, p.Value})
		}
		return acsRequest{kind: "spv", writes: writes}
	case env.Body.GetParameterNames != nil:
		return acsRequest{kind: "gpn", paths: []string{env.Body.GetParameterNames.Path}}
	case env.Body.Reboot != nil:
		return acsRequest{kind: "reboot"}
	}
	return acsRequest{}
}

// ── device → agent SOAP builders ────────────────────────────────────────────

const soapOpen = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:soap="http://schemas.xmlsoap.org/soap/encoding/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="urn:dslforum-org:cwmp-1-2">
<soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">` + "%s" + `</cwmp:ID></soapenv:Header>
<soapenv:Body>`

const soapClose = `</soapenv:Body></soapenv:Envelope>`

func wrap(msgID, inner string) string {
	return fmt.Sprintf(soapOpen, msgID) + inner + soapClose
}

func informEnvelope(msgID string, dev *device) string {
	informParams := []string{
		"Device.DeviceInfo.SoftwareVersion",
		"Device.LAN.IPAddress",
		"Device.WAN.IPAddress",
		"Device.Services.FAPService.1.FAPControl.LTE.RFTxStatus",
		"Device.Services.FAPService.1.FAPControl.LTE.OpState",
	}
	pairs := dev.get(informParams)
	var pl strings.Builder
	for _, pv := range pairs {
		fmt.Fprintf(&pl, `<ParameterValueStruct><Name>%s</Name><Value xsi:type="xsd:string">%s</Value></ParameterValueStruct>`, pv[0], pv[1])
	}
	inner := fmt.Sprintf(`<cwmp:Inform>`+
		`<DeviceID><Manufacturer>NybSys</Manufacturer><OUI>%s</OUI><ProductClass>%s</ProductClass><SerialNumber>%s</SerialNumber></DeviceID>`+
		`<Event soap:arrayType="cwmp:EventStruct[1]"><EventStruct><EventCode>2 PERIODIC</EventCode><CommandKey></CommandKey></EventStruct></Event>`+
		`<MaxEnvelopes>1</MaxEnvelopes><CurrentTime>%s</CurrentTime><RetryCount>0</RetryCount>`+
		`<ParameterList soap:arrayType="cwmp:ParameterValueStruct[%d]">%s</ParameterList>`+
		`</cwmp:Inform>`,
		deviceOUI, deviceProductClass, deviceSerial, time.Now().UTC().Format(time.RFC3339), len(pairs), pl.String())
	return wrap(msgID, inner)
}

// getRPCMethodsEnvelope is a device-initiated notification (CPE -> ACS); the
// agent's onGetRPCMethods (session.go) answers with its supported method list.
func getRPCMethodsEnvelope(msgID string) string {
	return wrap(msgID, `<cwmp:GetRPCMethods></cwmp:GetRPCMethods>`)
}

// transferCompleteEnvelope is the CPE's notification that an ACS-commanded
// transfer finished; the agent's onTransferComplete (session.go) answers with
// an empty TransferCompleteResponse. Distinct from AutonomousTransferComplete
// (atcEnvelope), which is for transfers the device initiated on its own.
func transferCompleteEnvelope(msgID string) string {
	now := time.Now().UTC().Format(time.RFC3339)
	inner := `<cwmp:TransferComplete>` +
		`<CommandKey></CommandKey>` +
		`<FaultStruct><FaultCode>0</FaultCode><FaultString></FaultString></FaultStruct>` +
		`<StartTime>` + now + `</StartTime>` +
		`<CompleteTime>` + now + `</CompleteTime>` +
		`</cwmp:TransferComplete>`
	return wrap(msgID, inner)
}

func atcEnvelope(msgID string) string {
	inner := `<cwmp:AutonomousTransferComplete>` +
		`<AnnounceURL></AnnounceURL><TransferURL>ftp://192.168.8.100/</TransferURL>` +
		`<IsDownload>0</IsDownload><FileType>4 Vendor Log File</FileType><FileSize>2048</FileSize>` +
		`<TargetFileName>` + deviceCWMPID + `_DeviceLog.tgz</TargetFileName>` +
		`<FaultStruct><FaultCode>0</FaultCode><FaultString></FaultString></FaultStruct>` +
		`<StartTime>` + time.Now().UTC().Format(time.RFC3339) + `</StartTime>` +
		`<CompleteTime>` + time.Now().UTC().Format(time.RFC3339) + `</CompleteTime>` +
		`</cwmp:AutonomousTransferComplete>`
	return wrap(msgID, inner)
}

// gpvResponseEnvelope answers a GetParameterValues with each path's manifest
// xsi:type (finding D: was a blanket xsd:string for every path regardless of
// its real type).
func gpvResponseEnvelope(msgID string, pairs [][2]string, m *manifest) string {
	var pl strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&pl, `<ParameterValueStruct><Name>%s</Name><Value xsi:type="%s">%s</Value></ParameterValueStruct>`, p[0], m.xsdType(p[0]), xmlEscape(p[1]))
	}
	inner := fmt.Sprintf(`<cwmp:GetParameterValuesResponse><ParameterList soap:arrayType="cwmp:ParameterValueStruct[%d]">%s</ParameterList></cwmp:GetParameterValuesResponse>`, len(pairs), pl.String())
	return wrap(msgID, inner)
}

func spvResponseEnvelope(msgID string) string {
	return wrap(msgID, `<cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse>`)
}

// gpnResponseEnvelope answers the first-contact writability walk, scoped to the
// requested ParameterPath (CWMP GetParameterNames semantics — the old stub
// ignored it and dumped the whole store). The managed snapshot paths are
// writable; identity/status paths are not. The agent walks "Device." (root,
// see session.go), so this legitimately returns the full 20,260-param tree —
// the agent's 16 MB request cap (server.go) is sized for exactly that.
func gpnResponseEnvelope(msgID string, dev *device, prefix string) string {
	writable := map[string]bool{
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower": true,
		"Device.ManagementServer.PeriodicInformInterval":                          true,
	}
	dev.mu.Lock()
	names := make([]string, 0, len(dev.params))
	for p := range dev.params {
		if prefix != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		names = append(names, p)
	}
	dev.mu.Unlock()
	var pl strings.Builder
	for _, p := range names {
		w := "0"
		if writable[p] || strings.Contains(p, ".CellConfig.") {
			w = "1"
		}
		fmt.Fprintf(&pl, `<ParameterInfoStruct><Name>%s</Name><Writable>%s</Writable></ParameterInfoStruct>`, p, w)
	}
	inner := fmt.Sprintf(`<cwmp:GetParameterNamesResponse><ParameterList soap:arrayType="cwmp:ParameterInfoStruct[%d]">%s</ParameterList></cwmp:GetParameterNamesResponse>`, len(names), pl.String())
	return wrap(msgID, inner)
}

func rebootResponseEnvelope(msgID string) string {
	return wrap(msgID, `<cwmp:RebootResponse></cwmp:RebootResponse>`)
}

// ── helpers ─────────────────────────────────────────────────────────────────

var msgCounter int64

func nextMsgID() string { return strconv.FormatInt(atomic.AddInt64(&msgCounter, 1), 10) }

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func envelope(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "timestamp": time.Now().UTC().Format(time.RFC3339), "data": data, "errors": []any{}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ftpUploadConfig is the mock device's FTP transport, mirroring the real
// NanoLink's curl -u <user>:<pass> ftp://<host>/ upload, plus the scenario
// that drives both the generated log content and (for the self-triggering
// scenarios) the upload path/credentials.
type ftpUploadConfig struct {
	host     string
	user     string
	pass     string
	interval time.Duration
	scenario loggen.Scenario
}

// runFTPUploadLoop waits for the device to Inform at least once (the agent's
// device-id resolution depends on cwmp_devices being populated first), then
// curl-uploads a real-shaped ring archive + bare Devicelog (ST-2) on the
// configured cadence, matching the real device's ~60s cycle.
func runFTPUploadLoop(dev *device, cfg ftpUploadConfig) {
	for dev.informs() == 0 {
		time.Sleep(250 * time.Millisecond)
	}
	corpus := sampleLines()
	upload := func() {
		genCfg := loggen.Config{
			OUI: deviceOUI, Serial: deviceSerial, ProductClass: deviceProductClass,
			PowerOnAt: time.Now().UTC(), Scenario: cfg.scenario,
		}
		archive, deviceLog, err := loggen.Generate(genCfg, corpus)
		if err != nil {
			log.Printf("log generation failed: %v", err)
			return
		}

		// The content-carrying upload ALWAYS targets the FTP root with the
		// configured creds, so the scenario's staged log line reliably reaches
		// the agent — this is what /health's typed-event gate asserts. If this
		// upload instead targeted the scenario's broken path/creds, the bytes
		// carrying the fault line would never arrive (the point of the fault),
		// making the typed-event assertion hollow for exactly the two
		// scenarios meant to prove it.
		if err := curlUpload(cfg.host, cfg.user, cfg.pass, archive, "/"+genCfg.ArchiveName()); err != nil {
			log.Printf("ftp archive upload failed: %v", err)
		}
		if err := curlUpload(cfg.host, cfg.user, cfg.pass, deviceLog, "/"+genCfg.DeviceLogName()); err != nil {
			log.Printf("ftp devicelog upload failed: %v", err)
		}

		// Self-triggering scenarios ALSO fire a separate probe upload against
		// the broken path/creds — proves the FTP hop itself faithfully
		// reproduces the real curl failure code (finding C), independent of
		// content delivery. Expected to fail; only logged for visibility.
		switch cfg.scenario {
		case loggen.ScenarioFTPPathReject:
			if err := curlUpload(cfg.host, cfg.user, cfg.pass, archive, "/uploads/"+genCfg.ArchiveName()); err != nil {
				log.Printf("ftp-path-reject probe (expected failure, proves curl(25)): %v", err)
			}
		case loggen.ScenarioFTPAuthFail:
			if err := curlUpload(cfg.host, cfg.user, cfg.pass+"-wrong", archive, "/"+genCfg.ArchiveName()); err != nil {
				log.Printf("ftp-auth-fail probe (expected failure, proves curl(67)): %v", err)
			}
		}
	}
	upload()
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for range ticker.C {
		upload()
	}
}

// curlUpload writes data to a scratch file and shells out to curl (installed
// in the testsuite image) rather than a Go FTP client — faithful to the real
// device, whose logs literally show
// "curl -vvv -T <file> ... -u nybsys:*** -g 'ftp://192.168.8.100/'".
func curlUpload(host, user, pass string, data []byte, remotePath string) error {
	tmp, err := os.CreateTemp("", "nanolink-upload-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	url := fmt.Sprintf("ftp://%s%s", host, remotePath)
	cmd := exec.Command("curl", "-sS", "-T", tmpPath, "-u", user+":"+pass, "--connect-timeout", "5", url)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
