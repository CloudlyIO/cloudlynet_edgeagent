package cwmp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func postSOAP(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, `text/xml; charset="utf-8"`, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestServerSessionReplayNoFault replays a full NanoLink session over HTTP:
// Inform → ATC → empty-POST(ACS turn) → GPN response → 204. The critical
// assertion is that ATC is answered 200 with an AutonomousTransferCompleteResponse
// and never a Fault, so the session survives to the ACS's read/write turn.
func TestServerSessionReplayNoFault(t *testing.T) {
	store := newFakeStore()
	srv := NewServer("0.0.0.0:7547", store)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. Inform → InformResponse.
	status, body := postSOAP(t, ts.URL, informXML)
	if status != http.StatusOK || !strings.Contains(body, "cwmp:InformResponse") {
		t.Fatalf("Inform: status=%d body=%s", status, body)
	}

	// 2. ATC → empty AutonomousTransferCompleteResponse, never a Fault. THE FIX.
	status, body = postSOAP(t, ts.URL, atcXML)
	if status != http.StatusOK {
		t.Fatalf("ATC status = %d, want 200 (the former ACS returned a fault here)", status)
	}
	if !strings.Contains(body, "cwmp:AutonomousTransferCompleteResponse") {
		t.Fatalf("ATC response missing:\n%s", body)
	}
	if strings.Contains(strings.ToLower(body), "fault") {
		t.Fatalf("ATC must never be answered with a Fault:\n%s", body)
	}

	// 3. Empty POST = ACS turn → the first-contact GetParameterNames walk.
	status, body = postSOAP(t, ts.URL, "")
	if status != http.StatusOK || !strings.Contains(body, "cwmp:GetParameterNames") {
		t.Fatalf("expected first-contact GPN: status=%d body=%s", status, body)
	}

	// 4. Device answers GPN → nothing left queued → 204, session completes.
	status, _ = postSOAP(t, ts.URL, gpnRespXML)
	if status != http.StatusNoContent {
		t.Fatalf("session end status = %d, want 204", status)
	}

	if _, ok := store.devices["8C1F64-ENB%2DN03002%2DB3-2205609999"]; !ok {
		t.Error("device not registered from Inform")
	}
	if !store.writability["8C1F64-ENB%2DN03002%2DB3-2205609999"] {
		t.Error("writability map not saved from the GPN walk")
	}
}

// TestServerEnqueueAwait drives the worker-facing path: a queued task is applied
// on the device's next turn and its result surfaces through Await.
func TestServerEnqueueAwait(t *testing.T) {
	store := newFakeStore()
	// Preload writability so Inform does not enqueue a first-contact GPN.
	store.writability["8C1F64-ENB%2DN03002%2DB3-2205609999"] = true
	srv := NewServer("0.0.0.0:7547", store)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	postSOAP(t, ts.URL, informXML) // opens the session (source host 127.0.0.1)

	srv.Enqueue("127.0.0.1", Task{
		Type: TaskGPV, CommandID: "q1",
		Paths: []string{"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower"},
	})

	// Device's next turn picks up the GPV request...
	status, body := postSOAP(t, ts.URL, "")
	if status != http.StatusOK || !strings.Contains(body, "cwmp:GetParameterValues") {
		t.Fatalf("queued GPV not handed out: status=%d body=%s", status, body)
	}
	// ...and answers it.
	postSOAP(t, ts.URL, gpvRespXML)

	res, ok := srv.Await("127.0.0.1", "q1", 2*time.Second)
	if !ok {
		t.Fatal("Await timed out waiting for the GPV result")
	}
	if res.Params["Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower"] != "-8" {
		t.Errorf("awaited result = %+v", res)
	}
}
