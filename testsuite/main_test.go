// Tests for the /device/params fault-injection endpoint (EPIC-5.S2).
//
// The endpoint is what EPIC-5's e2e and rollback demo use to degrade the mock device's SINR and
// then read back what the loop actually wrote, so it carries the whole "verified apply, automatic
// rollback" proof. Its failure modes are boring and therefore easy to ship broken: a wrong method
// silently succeeding, a malformed body writing nothing while reporting ok, or the handler being
// registered on only one of the two muxes.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sinrPath = "Device.PeriodicStatistics.SampleSet.1.Parameter.412.X_8C1F64_CurrentValue"

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	return body
}

// testDevice builds a manifest-seeded mock device the way main() does; the manifest is
// embedded, so loading it in a unit test is cheap and deterministic.
func testDevice(t *testing.T) *device {
	t.Helper()
	m, err := loadManifest()
	if err != nil {
		t.Fatalf("manifest load failed: %v", err)
	}
	return newDevice(m)
}

func TestNewDeviceSeedsTheT3PMCounters(t *testing.T) {
	// Without these the agent's T3 collector reads nothing, telemetry carries no sinr_avg_db,
	// and the loop's KPI watch sees a missing KPI - which it treats as healthy, not as a breach.
	dev := testDevice(t)
	for _, path := range []string{
		sinrPath,
		"Device.PeriodicStatistics.SampleSet.1.Parameter.316.X_8C1F64_CurrentValue",
		"Device.PeriodicStatistics.SampleSet.1.Parameter.168.X_8C1F64_CurrentValue",
		"Device.PeriodicStatistics.SampleSet.1.Parameter.170.X_8C1F64_CurrentValue",
		"Device.DeviceInfo.UpTime",
		"Device.Services.FAPService.1.FAPControl.LTE.AdminState",
	} {
		if got := dev.get([]string{path}); len(got) != 1 {
			t.Errorf("newDevice() is missing %s", path)
		}
	}
}

func TestTheSeededSINRIsHealthy(t *testing.T) {
	// 12.5 must clear the min_sinr_db=0 guardrail, or the demo rolls back before it applies.
	dev := testDevice(t)
	got := dev.get([]string{sinrPath})
	if len(got) != 1 || got[0][1] != "12.5" {
		t.Fatalf("seeded sinr = %v, want 12.5", got)
	}
}

func TestPostDeviceParamsWritesAndReports(t *testing.T) {
	dev := testDevice(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/device/params",
		strings.NewReader(`{"`+sinrPath+`": "-5.0"}`))

	deviceParamsHandler(dev)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decode(t, rec)
	if body["ok"] != true || body["written"] != float64(1) {
		t.Fatalf("body = %v, want ok=true written=1", body)
	}
	if got := dev.get([]string{sinrPath}); got[0][1] != "-5.0" {
		t.Fatalf("device sinr = %q, want -5.0", got[0][1])
	}
}

func TestGetDeviceParamsReadsBack(t *testing.T) {
	dev := testDevice(t)
	dev.set([][2]string{{sinrPath, "-5.0"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/device/params?path="+sinrPath, nil)

	deviceParamsHandler(dev)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	params, _ := decode(t, rec)["params"].(map[string]any)
	if params[sinrPath] != "-5.0" {
		t.Fatalf("read back %v, want -5.0", params[sinrPath])
	}
}

func TestGetDeviceParamsWithoutAPathIsARequestError(t *testing.T) {
	rec := httptest.NewRecorder()
	deviceParamsHandler(testDevice(t))(rec, httptest.NewRequest(http.MethodGet, "/device/params", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAMalformedBodyIsRejectedRatherThanSilentlyWritingNothing(t *testing.T) {
	dev := testDevice(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/device/params", strings.NewReader(`{"a": 1}`))

	deviceParamsHandler(dev)(rec, req)

	// A JSON number is a decode error, not a coercion: reporting ok here would make a demo that
	// injected nothing look like one that injected successfully.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if decode(t, rec)["ok"] != false {
		t.Fatal("a rejected body must not report ok")
	}
}

func TestAnUnsupportedMethodIsRefused(t *testing.T) {
	rec := httptest.NewRecorder()
	deviceParamsHandler(testDevice(t))(rec, httptest.NewRequest(http.MethodDelete, "/device/params", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestBothMuxesServeDeviceParams(t *testing.T) {
	// Registered on one mux only, either the demo works and the agent suite cannot degrade a
	// device, or the reverse - and both failures look like "the endpoint is missing" at 404.
	dev := testDevice(t)
	muxes := map[string]http.Handler{
		"cloudMux":     cloudMux(&state{acks: map[string]string{}, snapshotParams: map[string]any{}}, dev),
		"acsHealthMux": acsHealthMux(dev, t.TempDir(), "acsftp", "http://agent:7547/"),
	}
	for name, mux := range muxes {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/device/params",
			strings.NewReader(`{"`+sinrPath+`": "1.0"}`))
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, rec.Code)
		}
	}
}
