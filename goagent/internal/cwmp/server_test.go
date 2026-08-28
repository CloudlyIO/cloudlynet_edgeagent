package cwmp

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testCanonicalID = "8C1F64-ENB%2DN03002%2DB3-2205609999"
	testRSPPath     = "Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower"
)

func postSOAP(t *testing.T, url, body string) (int, string) {
	t.Helper()
	return postSOAPWith(t, http.DefaultClient, url, body)
}

// postSOAPWith posts with a caller-supplied client — a cookie-jar client models
// a TR-069-compliant CPE that returns the ACS's session cookie.
func postSOAPWith(t *testing.T, client *http.Client, url, body string) (int, string) {
	t.Helper()
	resp, err := client.Post(url, `text/xml; charset="utf-8"`, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func cookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar}
}

// TestServerSessionReplayNoFault replays a full NanoLink session over HTTP:
// Inform → ATC → empty-POST(ACS turn) → GPN response → 204. The critical
// assertion is that ATC is answered 200 with an AutonomousTransferCompleteResponse
// and never a Fault, so the session survives to the ACS's read/write turn.
// Posts carry NO cookie — the source-IP fallback must keep a cookie-less
// single device fully working (the production one-device-per-IP deployment).
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

	if _, ok := store.devices[testCanonicalID]; !ok {
		t.Error("device not registered from Inform")
	}
	if !store.writability[testCanonicalID] {
		t.Error("writability map not saved from the GPN walk")
	}
}

// TestServerEnqueueAwait drives the worker-facing path: a task queued by
// canonical device id is applied on that device's next turn and its result
// surfaces through Await.
func TestServerEnqueueAwait(t *testing.T) {
	store := newFakeStore()
	// Preload writability so Inform does not enqueue a first-contact GPN.
	store.writability[testCanonicalID] = true
	srv := NewServer("0.0.0.0:7547", store)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	postSOAP(t, ts.URL, informXML) // opens the session

	srv.Enqueue(testCanonicalID, Task{
		Type: TaskGPV, CommandID: "q1",
		Paths: []string{testRSPPath},
	})

	// Device's next turn picks up the GPV request...
	status, body := postSOAP(t, ts.URL, "")
	if status != http.StatusOK || !strings.Contains(body, "cwmp:GetParameterValues") {
		t.Fatalf("queued GPV not handed out: status=%d body=%s", status, body)
	}
	// ...and answers it.
	postSOAP(t, ts.URL, gpvRespXML)

	res, ok := srv.Await(testCanonicalID, "q1", 2*time.Second)
	if !ok {
		t.Fatal("Await timed out waiting for the GPV result")
	}
	if res.Params[testRSPPath] != "-8" {
		t.Errorf("awaited result = %+v", res)
	}
}

// TestServerSetsSessionCookieOnInform locks the affinity contract: once the
// Inform names the device, every response carries the CWMPSID cookie a
// TR-069-compliant CPE echoes for the rest of the session.
func TestServerSetsSessionCookieOnInform(t *testing.T) {
	srv := NewServer("0.0.0.0:7547", newFakeStore())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Post(ts.URL, `text/xml; charset="utf-8"`, strings.NewReader(informXML))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var got string
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			got = c.Value
		}
	}
	if got != testCanonicalID {
		t.Fatalf("session cookie = %q, want the canonical id %q", got, testCanonicalID)
	}
}

// TestServerTwoDevicesOneIPIsolatedSessions is the fleet-mode crux: two devices
// behind ONE source IP (the testsuite's N-devices-in-one-container, or a NAT)
// must not share a session. Tasks queued per cwmp_id must be executed and
// answered by that device only, and GPV answers must be cached under the
// answering device's id — even with the two devices' requests interleaved.
func TestServerTwoDevicesOneIPIsolatedSessions(t *testing.T) {
	idA := testCanonicalID
	idB := "8C1F64-ENB%2DN03002%2DB3-2205610002"
	informB := strings.Replace(informXML, "2205609999", "2205610002", 1)
	gpvRespB := strings.Replace(gpvRespXML, ">-8<", ">-4<", 1)

	store := newFakeStore()
	store.writability[idA] = true
	store.writability[idB] = true
	srv := NewServer("0.0.0.0:7547", store)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	devA, devB := cookieClient(t), cookieClient(t)
	postSOAPWith(t, devA, ts.URL, informXML)
	postSOAPWith(t, devB, ts.URL, informB) // B informs LAST: the IP fallback now points at B

	srv.Enqueue(idA, Task{Type: TaskGPV, CommandID: "qa", Paths: []string{testRSPPath}})
	srv.Enqueue(idB, Task{Type: TaskGPV, CommandID: "qb", Paths: []string{testRSPPath}})

	// A's empty POST (cookie = idA) must be handed A's task even though B was
	// the last device to Inform from this IP — cookie routing beats the fallback.
	status, body := postSOAPWith(t, devA, ts.URL, "")
	if status != http.StatusOK || !strings.Contains(body, "cwmp:GetParameterValues") {
		t.Fatalf("device A not handed its task: status=%d body=%s", status, body)
	}
	postSOAPWith(t, devA, ts.URL, gpvRespXML)

	status, body = postSOAPWith(t, devB, ts.URL, "")
	if status != http.StatusOK || !strings.Contains(body, "cwmp:GetParameterValues") {
		t.Fatalf("device B not handed its task: status=%d body=%s", status, body)
	}
	postSOAPWith(t, devB, ts.URL, gpvRespB)

	resA, ok := srv.Await(idA, "qa", 2*time.Second)
	if !ok || resA.Params[testRSPPath] != "-8" {
		t.Fatalf("device A result = %+v ok=%v, want its own -8", resA, ok)
	}
	resB, ok := srv.Await(idB, "qb", 2*time.Second)
	if !ok || resB.Params[testRSPPath] != "-4" {
		t.Fatalf("device B result = %+v ok=%v, want its own -4", resB, ok)
	}

	// Cache attribution: each answer landed under the ANSWERING device's id.
	if v := store.params[idA][testRSPPath]; v != "-8" {
		t.Errorf("device A cached %q, want -8", v)
	}
	if v := store.params[idB][testRSPPath]; v != "-4" {
		t.Errorf("device B cached %q, want -4", v)
	}
}

// TestServerCRUsesAdvertisedURL locks the per-device connection-request target:
// override wins, then the device's own advertised ConnectionRequestURL, then
// the historical <deviceIP>:30005 fallback.
func TestServerCRUsesAdvertisedURL(t *testing.T) {
	store := newFakeStore()
	srv := NewServer("0.0.0.0:7547", store)

	store.devices[testCanonicalID] = DeviceRecord{DeviceID: testCanonicalID, IP: "192.168.10.2"}
	if got := srv.crURL(testCanonicalID); got != "http://192.168.10.2:30005/" {
		t.Fatalf("no advertised URL: crURL = %q, want the :30005 fallback", got)
	}

	store.CacheParam(testCanonicalID, connectionRequestURLPath, "http://mockfleet:30011/")
	if got := srv.crURL(testCanonicalID); got != "http://mockfleet:30011/" {
		t.Fatalf("crURL = %q, want the device's advertised URL", got)
	}

	srv.UseConnRequest("u", "p", "http://lan-override:30005/")
	if got := srv.crURL(testCanonicalID); got != "http://lan-override:30005/" {
		t.Fatalf("crURL = %q, want the configured override to win", got)
	}
}

// A device session serves several callers at once: the worker applying a command and the
// collector taking a config snapshot both Await on it. Before results were routed by
// CommandID, whichever Await received a result it did not recognise discarded it, and the
// rightful waiter timed out — an applied write was reported as "device session timeout" and
// then rolled back. This is that race, pinned.
func TestConcurrentAwaitsDoNotConsumeEachOthersResults(t *testing.T) {
	srv := NewServer("0.0.0.0:7547", newFakeStore())
	const dev = "8C1F64-ENB%2DN03002%2DB3-2205610013"

	type outcome struct {
		res TaskResult
		ok  bool
	}
	// Repeated: with the old shared-channel drain, which waiter consumed (and threw away) the
	// other's result was a coin flip, so one round reproduced the loss only half the time.
	for round := 0; round < 6; round++ {
		spv := make(chan outcome, 1)
		snap := make(chan outcome, 1)

		go func() {
			r, ok := srv.Await(dev, "cmd-1", 3*time.Second)
			spv <- outcome{r, ok}
		}()
		go func() {
			r, ok := srv.Await(dev, "snapshot:"+dev, 3*time.Second)
			snap <- outcome{r, ok}
		}()
		time.Sleep(50 * time.Millisecond) // let both register

		sess := srv.session(dev)
		// Deliver in the opposite order to the waits, which is what made the old code drop one.
		sess.deliver(TaskResult{CommandID: "snapshot:" + dev, Params: map[string]string{"p": "1"}})
		sess.deliver(TaskResult{CommandID: "cmd-1", Params: map[string]string{"p": "2"}})

		got := <-spv
		if !got.ok || got.res.CommandID != "cmd-1" || got.res.Params["p"] != "2" {
			t.Fatalf("round %d: the command waiter lost its result: %+v", round, got)
		}
		gotSnap := <-snap
		if !gotSnap.ok || gotSnap.res.CommandID != "snapshot:"+dev {
			t.Fatalf("round %d: the snapshot waiter lost its result: %+v", round, gotSnap)
		}
	}
}
