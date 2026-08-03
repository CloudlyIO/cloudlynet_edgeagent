// Mock NanoLink CWMP device: dials the agent's in-agent ACS at :7547 on a fast
// loop (Inform -> GetRPCMethods -> TransferComplete -> [ATC when a transfer
// completed] -> drain the queued GPV/SPV/GPN/Reboot), answering each ACS task
// from its manifest-seeded param store. The SOAP envelopes it sends/returns live
// in soap.go. Device identity is config-driven and shared package-wide (set once
// in main()).
package main

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Device identity is config-driven (nanolinkConfig.Identity) — these vars are
// set once in main() before any goroutine starts, then read everywhere the
// old hardcoded consts used to be.
var (
	deviceOUI          string
	deviceProductClass string
	deviceSerial       string
	deviceCWMPID       string // canonical (%2D-encoded) id, derived from identity
)

type device struct {
	mu          sync.Mutex
	params      map[string]string
	informsSent int64
	pendingXfer int64
	lastXfer    atomic.Value // most recent xfer, announced by the next ATC
}

// xfer is the file a completed autonomous transfer announces over ATC.
type xfer struct {
	name string
	size int
}

// newDevice seeds the mock device's entire param store from the NanoLink
// manifest (20,260 params), so a GetParameterValues for any managed path
// returns a realistic value + xsi:type (see manifest.go's xsdType). Only
// fields the manifest doesn't carry (test-harness IPs) are overridden; identity
// stays in lockstep with the active config (deviceOUI/ProductClass/Serial),
// never the manifest's own snapshot values.
func newDevice(m *manifest) *device {
	params := m.seedParams()
	params["Device.LAN.IPAddress"] = "192.168.8.248"
	params["Device.WAN.IPAddress"] = "10.0.0.10"
	params["Device.DeviceInfo.SerialNumber"] = deviceSerial
	params["Device.DeviceInfo.ProductClass"] = deviceProductClass

	// PM counters the agent's T3 collector reads (goagent/internal/collector/metrics.go
	// tier3Metrics), pinned over whatever the manifest snapshot carries. Without known
	// values the mock device answers T3 GPVs with the manifest's captured numbers (or
	// nothing), telemetry carries no deterministic sinr_avg_db or rrc_success_pct, and
	// the closed loop's KPI watch has nothing to judge - which looks exactly like a
	// healthy window (a missing KPI is not a breach). Values are the HEALTHY baseline;
	// EPIC-5's e2e and demo degrade 412 through POST /device/params to trigger the
	// rollback.
	for k, v := range map[string]string{
		"Device.Services.FAPService.1.FAPControl.LTE.AdminState":                    "1",
		"Device.PeriodicStatistics.SampleSet.1.Parameter.316.X_8C1F64_CurrentValue": "42.0", // prb_dl_pct
		"Device.PeriodicStatistics.SampleSet.1.Parameter.315.X_8C1F64_CurrentValue": "18.0", // prb_ul_pct
		"Device.PeriodicStatistics.SampleSet.1.Parameter.412.X_8C1F64_CurrentValue": "12.5", // sinr_avg_db
		"Device.PeriodicStatistics.SampleSet.1.Parameter.10.X_8C1F64_CurrentValue":  "6",    // rrc_conn_mean
		"Device.PeriodicStatistics.SampleSet.1.Parameter.118.X_8C1F64_CurrentValue": "35.2", // thp_dl
		"Device.PeriodicStatistics.SampleSet.1.Parameter.119.X_8C1F64_CurrentValue": "9.8",  // thp_ul
		// rrc_success_pct is derived: 118/120 = 98.3%, comfortably above the 95% guardrail.
		"Device.PeriodicStatistics.SampleSet.1.Parameter.168.X_8C1F64_CurrentValue": "120", // RRC.AttConnEstab
		"Device.PeriodicStatistics.SampleSet.1.Parameter.170.X_8C1F64_CurrentValue": "118", // RRC.SuccConnEstab
		"Device.DeviceInfo.UpTime":                 "86400",
		"Device.DeviceInfo.MemoryStatus.Free":      "180000",
		"Device.DeviceInfo.MemoryStatus.Total":     "256000",
		"Device.DeviceInfo.ProcessStatus.CPUUsage": "17",
	} {
		params[k] = v
	}
	return &device{params: params}
}

// deviceParamsHandler serves /device/params - test and demo fault injection plus read-back.
//
//	POST body {"<full TR-069 path>": "<value>"}  -> {"ok": true, "written": N}
//	GET  ?path=<full TR-069 path>&path=...       -> {"ok": true, "params": {...}}
//
// One handler for both verbs, not two: http.ServeMux routes on PATH, so registering a second
// handler for the same path panics at startup rather than adding a method.
//
// Registered on BOTH muxes because the two modes serve different audiences and both need it: the
// full-mode platform mock is what the agent's own integration run uses, and acsftp mode is what
// EPIC-5's e2e and rollback demo use against the real gateway. A handler on only one of them means
// either the demo works and the agent suite cannot degrade a device, or the reverse.
//
// Values are strings because that is what CWMP carries and what `device.params` stores; sending
// 12.5 as a JSON number is a decode error rather than a silent coercion, which is the honest
// outcome. The GET exists so the e2e can prove an apply actually reached the device rather than
// only that a command row claims it did.
func deviceParamsHandler(dev *device) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var kv map[string]string
			if err := json.NewDecoder(r.Body).Decode(&kv); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writes := make([][2]string, 0, len(kv))
			for k, v := range kv {
				writes = append(writes, [2]string{k, v})
			}
			dev.set(writes)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "written": len(writes)})
		case http.MethodGet:
			paths := r.URL.Query()["path"]
			if len(paths) == 0 {
				writeJSON(w, http.StatusBadRequest,
					map[string]any{"ok": false, "error": "at least one ?path= is required"})
				return
			}
			values := map[string]string{}
			for _, pair := range dev.get(paths) {
				values[pair[0]] = pair[1]
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "params": values})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
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

// markTransfer records a completed autonomous file transfer (a log upload) and
// the file it uploaded. runSession emits one ATC per recorded transfer, so ATC
// cadence tracks real uploads (~60s) instead of firing on every CWMP session —
// matching a real device, which only ATCs when it finishes uploading a file, and
// announces that exact file.
func (d *device) markTransfer(name string, size int) {
	d.lastXfer.Store(xfer{name: name, size: size})
	atomic.AddInt64(&d.pendingXfer, 1)
}

// takeTransfer consumes one pending transfer, returning the file to announce and
// whether an ATC is due.
func (d *device) takeTransfer() (xfer, bool) {
	for {
		n := atomic.LoadInt64(&d.pendingXfer)
		if n <= 0 {
			return xfer{}, false
		}
		if atomic.CompareAndSwapInt64(&d.pendingXfer, n, n-1) {
			x, _ := d.lastXfer.Load().(xfer)
			return x, true
		}
	}
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

// maxSessionTasks bounds the ACS task-drain loop per session — a safety cap so a
// misbehaving ACS that never returns 204 can't spin the device loop forever.
const maxSessionTasks = 64

// runSession performs one CWMP session against the agent: Inform -> GetRPCMethods
// -> TransferComplete -> (ATC only when a transfer completed) -> drain the ACS's
// queued tasks -> 204. GetRPCMethods and TransferComplete are device-initiated
// notifications the agent's CWMP session already answers (session.go); sending
// them exercises that surface without disturbing the task-queue drain loop below.
func runSession(client *http.Client, agentURL string, dev *device, m *manifest) {
	msgID := nextMsgID()
	if _, ok := post(client, agentURL, informEnvelope(msgID, dev)); !ok {
		return // agent not up yet; retry next tick
	}
	dev.incInform()

	post(client, agentURL, getRPCMethodsEnvelope(msgID))
	post(client, agentURL, transferCompleteEnvelope(msgID))

	// ATC only when a log upload actually completed (see device.markTransfer),
	// so we don't announce a transfer every 500ms session like the old loop did.
	if x, ok := dev.takeTransfer(); ok {
		atc, _ := post(client, agentURL, atcEnvelope(msgID, x.name, x.size))
		if strings.Contains(strings.ToLower(atc), "fault") {
			log.Printf("REGRESSION: agent answered AutonomousTransferComplete with a Fault:\n%s", atc)
		}
	}

	for i := 0; i < maxSessionTasks; i++ {
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

var msgCounter int64

func nextMsgID() string { return strconv.FormatInt(atomic.AddInt64(&msgCounter, 1), 10) }
