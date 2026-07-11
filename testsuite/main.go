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
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
}

const managedSnapshotParamCount = 24

const (
	deviceOUI          = "8C1F64"
	deviceProductClass = "ENB-N03002-B3"
	deviceSerial       = "2205600282"
	deviceCWMPID       = "8C1F64-ENB%2DN03002%2DB3-2205600282" // canonical (%2D-encoded) id
)

func main() {
	mode := strings.ToLower(strings.TrimSpace(env("TESTSUITE_MODE", "full")))
	st := &state{acks: map[string]string{}, snapshotParams: map[string]any{}}
	dev := newDevice()

	ftpDir := env("FTP_DIR", "/ftp")
	if err := os.MkdirAll(ftpDir, 0o755); err == nil {
		go func() {
			time.Sleep(3 * time.Second)
			if err := writeArchive(path.Join(ftpDir, deviceCWMPID+"_logs.tgz")); err != nil {
				log.Printf("fixture archive failed: %v", err)
			}
		}()
	}

	// The mock device dials the agent's ACS. It retries until the agent is up.
	agentURL := env("AGENT_CWMP_URL", "http://cloudlynet-edgeagent:7547/")
	dialNow := make(chan struct{}, 1)
	go runConnRequestListener(":30005", dialNow)
	go runDeviceLoop(agentURL, dev, dialNow)

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
			st.failures > 0 && st.snapshots > 0 && len(st.snapshotParams) == managedSnapshotParamCount && len(st.acks) >= 3
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              ok,
			"registered":      st.registered,
			"heartbeats":      st.heartbeats,
			"telemetry":       st.telemetry,
			"events":          st.events,
			"failures":        st.failures,
			"snapshots":       st.snapshots,
			"snapshot_params": len(st.snapshotParams),
			"acks":            st.acks,
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
			Events []any `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.telemetry++
		st.events += len(body.Events)
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

func newDevice() *device {
	return &device{params: map[string]string{
		// Identity + liveness/IP (cached on Inform, feeds inventory).
		"Device.DeviceInfo.SerialNumber":                         deviceSerial,
		"Device.DeviceInfo.ProductClass":                         deviceProductClass,
		"Device.DeviceInfo.SoftwareVersion":                      "1.0.0",
		"Device.LAN.IPAddress":                                   "192.168.8.248",
		"Device.WAN.IPAddress":                                   "10.0.0.10",
		"Device.Services.FAPService.1.FAPControl.LTE.RFTxStatus": "1",
		"Device.Services.FAPService.1.FAPControl.LTE.OpState":    "1",
		// The 24 managed snapshot parameters.
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.PhyCellID":                              "449",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNDL":                               "1850",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNUL":                               "19850",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.FreqBandIndicator":                      "3",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.DLBandwidth":                            "100",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ULBandwidth":                            "100",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower":                   "-10",
		"Device.Services.FAPService.1.Capabilities.MaxTxPower":                                      "21",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.PHY.PDSCH.Pa":                              "0",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.PHY.PDSCH.Pb":                              "0",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.DRXEnabled":                        "1",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.OnDurationTimer":                   "40",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.DRXInactivityTimer":                "1920",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.LongDRXCycle":                      "128",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.ShortDRXCycle":                     "128",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.X_8C1F64_PCH.DefaultPagingCycle":       "rf128",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.IdleMode.IntraFreq.QRxLevMinSIB1": "-62",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.IdleMode.IntraFreq.SIntraSearch":  "21",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.A2ThresholdRSRP":   "50",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.A1ThresholdRSRP":   "60",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.Hysteresis":        "2",
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.TimeToTrigger":     "40",
		"Device.ManagementServer.PeriodicInformInterval":                                            "300",
		"Device.X_8C1F64_DebugMgmt.Upload.AutonomousTransferCompletePolicy":                         "Always",
	}}
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

func runDeviceLoop(agentURL string, dev *device, dialNow <-chan struct{}) {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		runSession(client, agentURL, dev)
		select {
		case <-ticker.C:
		case <-dialNow:
		}
	}
}

// runSession performs one CWMP session against the agent: Inform -> ATC -> drain
// the ACS's queued tasks -> 204.
func runSession(client *http.Client, agentURL string, dev *device) {
	msgID := nextMsgID()
	if _, ok := post(client, agentURL, informEnvelope(msgID)); !ok {
		return // agent not up yet; retry next tick
	}
	dev.incInform()

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
			post(client, agentURL, gpvResponseEnvelope(msgID, dev.get(req.paths)))
		case "spv":
			dev.set(req.writes)
			post(client, agentURL, spvResponseEnvelope(msgID))
		case "gpn":
			post(client, agentURL, gpnResponseEnvelope(msgID, dev))
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
		return acsRequest{kind: "gpn"}
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

func informEnvelope(msgID string) string {
	informParams := []string{
		"Device.DeviceInfo.SoftwareVersion",
		"Device.LAN.IPAddress",
		"Device.WAN.IPAddress",
		"Device.Services.FAPService.1.FAPControl.LTE.RFTxStatus",
		"Device.Services.FAPService.1.FAPControl.LTE.OpState",
	}
	var pl strings.Builder
	seed := newDevice()
	for _, p := range informParams {
		fmt.Fprintf(&pl, `<ParameterValueStruct><Name>%s</Name><Value xsi:type="xsd:string">%s</Value></ParameterValueStruct>`, p, seed.params[p])
	}
	inner := fmt.Sprintf(`<cwmp:Inform>`+
		`<DeviceID><Manufacturer>NybSys</Manufacturer><OUI>%s</OUI><ProductClass>%s</ProductClass><SerialNumber>%s</SerialNumber></DeviceID>`+
		`<Event soap:arrayType="cwmp:EventStruct[1]"><EventStruct><EventCode>2 PERIODIC</EventCode><CommandKey></CommandKey></EventStruct></Event>`+
		`<MaxEnvelopes>1</MaxEnvelopes><CurrentTime>%s</CurrentTime><RetryCount>0</RetryCount>`+
		`<ParameterList soap:arrayType="cwmp:ParameterValueStruct[%d]">%s</ParameterList>`+
		`</cwmp:Inform>`,
		deviceOUI, deviceProductClass, deviceSerial, time.Now().UTC().Format(time.RFC3339), len(informParams), pl.String())
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

func gpvResponseEnvelope(msgID string, pairs [][2]string) string {
	var pl strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&pl, `<ParameterValueStruct><Name>%s</Name><Value xsi:type="xsd:string">%s</Value></ParameterValueStruct>`, p[0], xmlEscape(p[1]))
	}
	inner := fmt.Sprintf(`<cwmp:GetParameterValuesResponse><ParameterList soap:arrayType="cwmp:ParameterValueStruct[%d]">%s</ParameterList></cwmp:GetParameterValuesResponse>`, len(pairs), pl.String())
	return wrap(msgID, inner)
}

func spvResponseEnvelope(msgID string) string {
	return wrap(msgID, `<cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse>`)
}

// gpnResponseEnvelope answers the first-contact writability walk. The managed
// snapshot paths are writable; identity/status paths are not.
func gpnResponseEnvelope(msgID string, dev *device) string {
	writable := map[string]bool{
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower": true,
		"Device.ManagementServer.PeriodicInformInterval":                          true,
	}
	dev.mu.Lock()
	names := make([]string, 0, len(dev.params))
	for p := range dev.params {
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

func writeArchive(target string) error {
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	files := map[string]string{
		"TR69.log":       "RPC Unknown received from ACS\n",
		"FILE_TRANS.log": "File upload success, curl code=(0)\n",
		"FM.log":         "device reboot observed\n",
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return err
		}
	}
	return nil
}
