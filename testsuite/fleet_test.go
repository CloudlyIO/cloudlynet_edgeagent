// Tests for fleet mode (N mock femtocells in one process — SCOPE B1) and for
// the back-compat contract: with no devices: list, everything must resolve to
// the historical single-device behavior.
package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	phyCellIDPath = "Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.PhyCellID"
	// Synthetic serials per the 22056xxxxx mock convention — never real ones.
	fleetSerial1 = "2205610001"
	fleetSerial2 = "2205610002"
)

func writeConf(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nanolink.conf")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	return p
}

const fleetConf = `
identity:
  oui: "8C1F64"
  product_class: "ENB-N03002-B3"
  serial: "2205609999"
devices:
  - serial: "` + fleetSerial1 + `"
    params:
      ` + phyCellIDPath + `: "101"
      ` + sinrPath + `: "9.5"
  - serial: "` + fleetSerial2 + `"
    oui: "AABBCC"
    product_class: "ENB-TEST"
`

func TestSingleDeviceConfResolvesToTheHistoricalDefaults(t *testing.T) {
	// The back-compat default path: no devices: list -> exactly one spec, built
	// from the identity block, CR on the historical :30005, no CR advertisement.
	cfg, err := loadNanolinkConfig(writeConf(t, "identity:\n  serial: \"2205609999\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	specs, err := cfg.deviceSpecs()
	if err != nil {
		t.Fatalf("deviceSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	s := specs[0]
	if s.OUI != "8C1F64" || s.ProductClass != "ENB-N03002-B3" || s.Serial != "2205609999" {
		t.Fatalf("identity = %s/%s/%s, want built-in defaults", s.OUI, s.ProductClass, s.Serial)
	}
	if s.CWMPID != "8C1F64-ENB%2DN03002%2DB3-2205609999" {
		t.Fatalf("cwmp_id = %q", s.CWMPID)
	}
	if s.CRPort != 30005 || s.CRURL != "" || s.Index != 0 || len(s.Params) != 0 {
		t.Fatalf("spec = %+v, want CRPort 30005, no CRURL, index 0, no params", s)
	}
}

func TestFleetConfParsesEntriesWithIdentityBlockDefaults(t *testing.T) {
	cfg, err := loadNanolinkConfig(writeConf(t, fleetConf))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	specs, err := cfg.deviceSpecs()
	if err != nil {
		t.Fatalf("deviceSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2", len(specs))
	}
	// Entry 0 inherits oui/product_class from the identity block and carries its overlay.
	if specs[0].OUI != "8C1F64" || specs[0].ProductClass != "ENB-N03002-B3" || specs[0].Serial != fleetSerial1 {
		t.Fatalf("specs[0] identity = %+v", specs[0])
	}
	if specs[0].Params[phyCellIDPath] != "101" || specs[0].Params[sinrPath] != "9.5" {
		t.Fatalf("specs[0] params = %v", specs[0].Params)
	}
	// Entry 1 overrides both.
	if specs[1].OUI != "AABBCC" || specs[1].ProductClass != "ENB-TEST" || specs[1].Serial != fleetSerial2 {
		t.Fatalf("specs[1] identity = %+v", specs[1])
	}
}

func TestFleetCanonicalIDsArePerDeviceAndPercentEncoded(t *testing.T) {
	cfg, _ := loadNanolinkConfig(writeConf(t, fleetConf))
	specs, err := cfg.deviceSpecs()
	if err != nil {
		t.Fatalf("deviceSpecs: %v", err)
	}
	if specs[0].CWMPID != "8C1F64-ENB%2DN03002%2DB3-"+fleetSerial1 {
		t.Fatalf("specs[0].CWMPID = %q", specs[0].CWMPID)
	}
	if specs[1].CWMPID != "AABBCC-ENB%2DTEST-"+fleetSerial2 {
		t.Fatalf("specs[1].CWMPID = %q", specs[1].CWMPID)
	}
	if specs[0].CWMPID == specs[1].CWMPID {
		t.Fatal("cwmp_ids must be distinct per device")
	}
}

func TestFleetCRPortsAreBasePlusIndex(t *testing.T) {
	if crPortFor(0) != 30005 {
		t.Fatalf("crPortFor(0) = %d, want the historical 30005", crPortFor(0))
	}
	if crPortFor(5) != 30010 {
		t.Fatalf("crPortFor(5) = %d, want 30010", crPortFor(5))
	}
	cfg, _ := loadNanolinkConfig(writeConf(t, fleetConf))
	specs, _ := cfg.deviceSpecs()
	if specs[0].CRPort != 30005 || specs[1].CRPort != 30006 {
		t.Fatalf("CR ports = %d/%d, want 30005/30006", specs[0].CRPort, specs[1].CRPort)
	}
}

func TestFleetStartStaggerIsDeterministic(t *testing.T) {
	if startDelay(0) != 0 {
		t.Fatalf("startDelay(0) = %v, want 0 (single-device timing unchanged)", startDelay(0))
	}
	if startDelay(3) != 2100*time.Millisecond {
		t.Fatalf("startDelay(3) = %v, want 2.1s", startDelay(3))
	}
}

func TestDuplicateFleetSerialIsARejectedConf(t *testing.T) {
	cfg, err := loadNanolinkConfig(writeConf(t,
		"devices:\n  - serial: \""+fleetSerial1+"\"\n  - serial: \""+fleetSerial1+"\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := cfg.deviceSpecs(); err == nil || !strings.Contains(err.Error(), "duplicate serial") {
		t.Fatalf("deviceSpecs err = %v, want duplicate-serial error", err)
	}
}

func TestFleetEntryWithoutASerialIsARejectedConf(t *testing.T) {
	cfg, err := loadNanolinkConfig(writeConf(t, "devices:\n  - oui: \"8C1F64\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := cfg.deviceSpecs(); err == nil || !strings.Contains(err.Error(), "serial is required") {
		t.Fatalf("deviceSpecs err = %v, want serial-required error", err)
	}
}

func TestFleetIsConfFileOnlyForSerials(t *testing.T) {
	// NANOLINK_SERIAL keeps steering the single-device identity block, but fleet
	// entries carry explicit serials the env can never touch.
	t.Setenv("NANOLINK_SERIAL", "9999999999")
	cfg, _ := loadNanolinkConfig(writeConf(t, fleetConf))
	cfg = cfg.withEnvOverrides()
	specs, err := cfg.deviceSpecs()
	if err != nil {
		t.Fatalf("deviceSpecs: %v", err)
	}
	if specs[0].Serial != fleetSerial1 || specs[1].Serial != fleetSerial2 {
		t.Fatalf("fleet serials = %s/%s, env must not override them", specs[0].Serial, specs[1].Serial)
	}
}

func TestPerDeviceStoreSeedingAppliesTheParamsOverlayAfterIdentity(t *testing.T) {
	m, err := loadManifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	spec := deviceSpec{
		Index: 1, OUI: "8C1F64", ProductClass: "ENB-N03002-B3", Serial: fleetSerial1,
		CWMPID: canonicalID("8C1F64", "ENB-N03002-B3", fleetSerial1),
		CRPort: 30006, CRURL: "http://testsuite:30006/",
		Params: map[string]string{
			phyCellIDPath: "101",
			sinrPath:      "9.5", // must beat the T3 healthy pin (12.5)
		},
	}
	dev := newDeviceFromSpec(m, spec)
	for path, want := range map[string]string{
		"Device.DeviceInfo.SerialNumber": fleetSerial1, // identity seeding
		phyCellIDPath:                    "101",        // overlay over the manifest value (449)
		sinrPath:                         "9.5",        // overlay wins over the T3 pin
		connectionRequestURLPath:         "http://testsuite:30006/",
	} {
		got := dev.get([]string{path})
		if len(got) != 1 || got[0][1] != want {
			t.Errorf("store[%s] = %v, want %q", path, got, want)
		}
	}
	if dev.cwmpID != spec.CWMPID || dev.crPort != 30006 {
		t.Fatalf("device identity = %q/:%d, want spec's", dev.cwmpID, dev.crPort)
	}
}

func TestDefaultDeviceKeepsTheManifestConnectionRequestURL(t *testing.T) {
	// Single-device back-compat: no CR overlay — the manifest's captured value
	// stays, and the Inform keeps the historical 5-param list.
	m, err := loadManifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	dev := newDevice(m)
	got := dev.get([]string{connectionRequestURLPath})
	if len(got) != 1 || got[0][1] != m.byPath[connectionRequestURLPath].Value {
		t.Fatalf("store CR URL = %v, want the manifest's captured %q", got, m.byPath[connectionRequestURLPath].Value)
	}
	if env := informEnvelope("1", dev); strings.Contains(env, "ConnectionRequestURL") {
		t.Fatal("single-device Inform must not grow a ConnectionRequestURL param")
	}
}

func TestFleetDeviceAdvertisesItsOwnCRURLInTheInform(t *testing.T) {
	m, err := loadManifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	dev := newDeviceFromSpec(m, deviceSpec{
		Index: 2, OUI: "8C1F64", ProductClass: "ENB-N03002-B3", Serial: fleetSerial2,
		CWMPID: canonicalID("8C1F64", "ENB-N03002-B3", fleetSerial2),
		CRPort: 30007, CRURL: "http://testsuite:30007/",
	})
	env := informEnvelope("1", dev)
	if !strings.Contains(env, connectionRequestURLPath) || !strings.Contains(env, "http://testsuite:30007/") {
		t.Fatalf("fleet Inform must advertise the device's own CR URL, got:\n%s", env)
	}
	if !strings.Contains(env, "<SerialNumber>"+fleetSerial2+"</SerialNumber>") {
		t.Fatal("fleet Inform must carry the device's own serial")
	}
}

// fleetTestState builds a 2-device fleet state + devices the way main() does.
func fleetTestState(t *testing.T) (*state, []*device) {
	t.Helper()
	m, err := loadManifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	cfg, _ := loadNanolinkConfig(writeConf(t, fleetConf))
	specs, err := cfg.deviceSpecs()
	if err != nil {
		t.Fatalf("deviceSpecs: %v", err)
	}
	st := stateForSpecs(&state{
		acks:           map[string]string{},
		snapshotParams: map[string]any{},
		typedEvents:    map[string]struct{}{},
		seenDedup:      map[string]struct{}{},
	}, specs, true)
	devices := make([]*device, len(specs))
	for i, s := range specs {
		devices[i] = newDeviceFromSpec(m, s)
	}
	return st, devices
}

// fleetMuxHarness wires a 2-device fleet mux plus request/health helpers shared
// by the fleet gate tests.
type fleetMuxHarness struct {
	t       *testing.T
	mux     http.Handler
	devices []*device
	st      *state
}

func newFleetMuxHarness(t *testing.T) *fleetMuxHarness {
	t.Helper()
	st, devices := fleetTestState(t)
	st.failures = 1 // skip the forced first-telemetry 503
	return &fleetMuxHarness{t: t, mux: cloudMux(st, devices...), devices: devices, st: st}
}

func (h *fleetMuxHarness) do(method, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
	return rec
}

func (h *fleetMuxHarness) fleet() map[string]any {
	f, ok := decode(h.t, h.do(http.MethodGet, "/health", ""))["fleet"].(map[string]any)
	if !ok {
		h.t.Fatal("health has no fleet section")
	}
	return f
}

// ownPCI reads a device's live PhyCellID — what a correctly-attributed snapshot
// for that device must carry.
func (h *fleetMuxHarness) ownPCI(i int) string {
	got := h.devices[i].get([]string{phyCellIDPath})
	if len(got) != 1 {
		h.t.Fatalf("device %d has no PhyCellID", i)
	}
	return got[0][1]
}

func (h *fleetMuxHarness) snapshotURL(id string) string {
	return "/v1/agent/devices/" + strings.ReplaceAll(id, "%", "%25") + "/config-snapshot"
}

func TestFleetHealthTracksPerDeviceOnboarding(t *testing.T) {
	h := newFleetMuxHarness(t)
	id1, id2 := h.devices[0].cwmpID, h.devices[1].cwmpID

	// Nothing onboarded yet: fleet reports all expected devices, none complete.
	f := h.fleet()
	if f["expected"] != float64(2) || f["onboarded"] != float64(0) || f["ok"] != false {
		t.Fatalf("initial fleet = %v", f)
	}
	if per := f["per_device"].(map[string]any); len(per) != 2 {
		t.Fatalf("per_device = %v, want both expected ids listed", per)
	}

	// Device 1 fully onboards: heartbeat inventory + telemetry metrics + a
	// snapshot carrying ITS OWN RF identity (the attribution-checked gate).
	h.do(http.MethodPost, "/v1/agent/heartbeat", `{"devices":[{"cwmp_id":"`+id1+`"}]}`)
	h.do(http.MethodPost, "/v1/agent/telemetry",
		`{"metrics":[{"cwmp_id":"`+id1+`"}],"events":[{"cwmp_id":"`+id1+`","event_type":"ftp_upload_ok","dedup_key":"k1"}]}`)
	h.do(http.MethodPost, h.snapshotURL(id1), `{"params":{"`+phyCellIDPath+`":"`+h.ownPCI(0)+`"}}`)

	f = h.fleet()
	if f["onboarded"] != float64(1) || f["ok"] != false {
		t.Fatalf("after device 1: fleet = %v, want onboarded 1, ok still false", f)
	}
	d1 := f["per_device"].(map[string]any)[id1].(map[string]any)
	if d1["registered"] != true || d1["telemetry"] != float64(1) || d1["events"] != float64(1) ||
		d1["snapshots"] != float64(1) || d1["snapshot_verified"] != float64(1) ||
		d1["snapshot_mismatches"] != float64(0) || d1["complete"] != true {
		t.Fatalf("device 1 stats = %v", d1)
	}

	// Device 2 completes too -> fleet ok flips (top-level ok still gated by the
	// unchanged single-device conditions: registered/acks/24-param snapshot/...).
	h.do(http.MethodPost, "/v1/agent/heartbeat", `{"devices":[{"cwmp_id":"`+id1+`"},{"cwmp_id":"`+id2+`"}]}`)
	h.do(http.MethodPost, "/v1/agent/telemetry", `{"metrics":[{"cwmp_id":"`+id2+`"}]}`)
	h.do(http.MethodPost, h.snapshotURL(id2), `{"params":{"`+phyCellIDPath+`":"`+h.ownPCI(1)+`"}}`)

	full := decode(t, h.do(http.MethodGet, "/health", ""))
	f = full["fleet"].(map[string]any)
	if f["onboarded"] != float64(2) || f["ok"] != true {
		t.Fatalf("after both: fleet = %v, want onboarded 2 ok true", f)
	}
	if f["actuation"] != "pending" {
		t.Fatalf("actuation = %v, want pending before any configure ack", f["actuation"])
	}
	if full["ok"] != false {
		t.Fatal("top-level ok must still honor the unchanged single-device gate")
	}
}

// TestFleetHealthDetectsMisattributedSnapshots is the anti-scrambling gate: a
// snapshot POSTed under device 2's id but carrying device 1's RF identity (the
// exact artifact of per-IP shared CWMP sessions) must fail that device's
// complete flag — presence of N snapshots alone is NOT green health.
func TestFleetHealthDetectsMisattributedSnapshots(t *testing.T) {
	h := newFleetMuxHarness(t)
	id1, id2 := h.devices[0].cwmpID, h.devices[1].cwmpID
	if h.ownPCI(0) == h.ownPCI(1) {
		t.Fatal("test conf must give the two devices distinct PhyCellIDs")
	}

	h.do(http.MethodPost, "/v1/agent/heartbeat", `{"devices":[{"cwmp_id":"`+id1+`"},{"cwmp_id":"`+id2+`"}]}`)
	h.do(http.MethodPost, "/v1/agent/telemetry", `{"metrics":[{"cwmp_id":"`+id1+`"},{"cwmp_id":"`+id2+`"}]}`)
	h.do(http.MethodPost, h.snapshotURL(id1), `{"params":{"`+phyCellIDPath+`":"`+h.ownPCI(0)+`"}}`)
	// Device 2's snapshot answered by device 1: carries device 1's PCI.
	h.do(http.MethodPost, h.snapshotURL(id2), `{"params":{"`+phyCellIDPath+`":"`+h.ownPCI(0)+`"}}`)

	f := h.fleet()
	if f["ok"] != false {
		t.Fatalf("fleet = %v, want ok false on a misattributed snapshot", f)
	}
	d2 := f["per_device"].(map[string]any)[id2].(map[string]any)
	if d2["snapshots"] != float64(1) || d2["snapshot_mismatches"] != float64(1) || d2["complete"] != false {
		t.Fatalf("device 2 stats = %v, want the mismatch counted and complete false", d2)
	}
}

// TestFleetHealthDetectsMisattributedTelemetry: a metric sample filed under
// device 2 carrying device 1's in-use PCI is scrambled telemetry and must
// break device 2's complete flag.
func TestFleetHealthDetectsMisattributedTelemetry(t *testing.T) {
	h := newFleetMuxHarness(t)
	id2 := h.devices[1].cwmpID
	// Device 1's conf overlays PhyCellID 101, mirrored into its in-use path;
	// device 2's in-use PCI is the manifest's value — different by construction.
	h.do(http.MethodPost, "/v1/agent/telemetry",
		`{"metrics":[{"cwmp_id":"`+id2+`","metrics":{"pci_inuse":"101"}}]}`)
	f := h.fleet()
	d2 := f["per_device"].(map[string]any)[id2].(map[string]any)
	if d2["telemetry_mismatches"] != float64(1) {
		t.Fatalf("device 2 stats = %v, want the pci_inuse mismatch counted", d2)
	}
}

// TestFleetHealthActuationAttribution locks the configure-ack probe: an
// "applied" ack is only believed when the write is present on the addressed
// device's own store; otherwise the fleet gate reports a misroute and fails.
func TestFleetHealthActuationAttribution(t *testing.T) {
	// Misrouted: dev0 pinned to a non-target value, ack claims applied anyway.
	h := newFleetMuxHarness(t)
	h.devices[0].set([][2]string{{refSignalPowerPath, "-10"}})
	h.do(http.MethodPost, "/v1/agent/commands/"+configureCommandID+"/ack", `{"status":"applied"}`)
	f := h.fleet()
	if f["actuation"] != "misrouted" || f["ok"] != false {
		t.Fatalf("fleet = %v, want actuation misrouted and ok false", f)
	}

	// Verified: the write actually landed on dev0 before the ack.
	h = newFleetMuxHarness(t)
	h.devices[0].set([][2]string{{refSignalPowerPath, configuredRSPower}})
	h.do(http.MethodPost, "/v1/agent/commands/"+configureCommandID+"/ack", `{"status":"applied"}`)
	if f := h.fleet(); f["actuation"] != "verified" {
		t.Fatalf("fleet = %v, want actuation verified", f)
	}
}

// TestFleetOverlayMirrorsInUseRFIdentity: overlaying the configured PhyCellID/
// EARFCNDL also sets the *InUse reporting paths (what T2 telemetry reads),
// unless the overlay pinned them explicitly.
func TestFleetOverlayMirrorsInUseRFIdentity(t *testing.T) {
	_, devices := fleetTestState(t)
	got := devices[0].get([]string{phyCellIDInUsePath})
	if len(got) != 1 || got[0][1] != "101" {
		t.Fatalf("in-use PCI = %v, want the overlaid 101 mirrored", got)
	}
	// Device 2 has no overlay: its in-use paths stay the manifest's values.
	m, _ := loadManifest()
	got = devices[1].get([]string{phyCellIDInUsePath})
	if len(got) != 1 || got[0][1] != m.byPath[phyCellIDInUsePath].Value {
		t.Fatalf("device 2 in-use PCI = %v, want the manifest's untouched value", got)
	}
}

func TestSingleDeviceHealthHasNoFleetSection(t *testing.T) {
	// Byte-for-byte back-compat: the historical health JSON gains no keys.
	dev := testDevice(t)
	mux := cloudMux(&state{acks: map[string]string{}, snapshotParams: map[string]any{}}, dev)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if _, hasFleet := decode(t, rec)["fleet"]; hasFleet {
		t.Fatal("single-device /health must not carry a fleet section")
	}
}

func TestFleetDeviceParamsSelectorRoutesBySerial(t *testing.T) {
	_, devices := fleetTestState(t)
	handler := deviceParamsMux(devices)
	sinr0 := devices[0].get([]string{sinrPath})[0][1]

	// No selector in fleet mode is a 400, never a silent write to some default
	// device — a demo script that forgot ?device= must fail loudly.
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/device/params",
		strings.NewReader(`{"`+sinrPath+`": "-3.0"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("selector-less write status = %d, want 400", rec.Code)
	}
	if got := devices[0].get([]string{sinrPath}); got[0][1] != sinr0 {
		t.Fatalf("device 0 sinr changed to %v, a rejected request must write nothing", got)
	}

	// ?device=<serial> targets that device only.
	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/device/params?device="+fleetSerial2,
		strings.NewReader(`{"`+sinrPath+`": "-7.0"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("selector write status = %d", rec.Code)
	}
	if got := devices[1].get([]string{sinrPath}); got[0][1] != "-7.0" {
		t.Fatalf("device 1 sinr = %v, want -7.0", got)
	}
	if got := devices[0].get([]string{sinrPath}); got[0][1] != sinr0 {
		t.Fatalf("device 0 sinr changed to %v, selector must not leak across devices", got)
	}

	// ?device=<cwmp_id> works too (the canonical id the platform tracks; its
	// literal %2D must be query-escaped by the caller).
	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet,
		"/device/params?device="+url.QueryEscape(devices[1].cwmpID)+"&path="+sinrPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cwmp_id selector status = %d", rec.Code)
	}

	// Unknown selector is a 404, not a silent default-device write.
	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/device/params?device=nope",
		strings.NewReader(`{"`+sinrPath+`": "0"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown device status = %d, want 404", rec.Code)
	}
}
