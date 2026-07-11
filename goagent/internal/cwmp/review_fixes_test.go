package cwmp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEncodeSPVEmitsLiteralXsiType locks the fix for the typed-write interop
// issue: the Value element must carry a literal xsi:type (the envelope declares
// the xsi prefix), not Go's generated namespace-URL attribute form.
func TestEncodeSPVEmitsLiteralXsiType(t *testing.T) {
	spv := &SetParameterValues{
		ParameterList: []ParameterValueStruct{
			{Name: "Device.X", Value: ValueNode{Type: "xsd:unsignedInt", Text: "23"}},
		},
		ParameterKey: "k",
	}
	out, err := EncodeResponse("1", spv)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(out), `xsi:type="xsd:unsignedInt"`) {
		t.Errorf("expected literal xsi:type on the Value element:\n%s", out)
	}
}

// TestOnInformFirstContactSnapshotGPV locks the FullSnapshotOnFirstContact
// wiring: when configured, first contact queues the writability walk AND a
// managed-config GPV.
func TestOnInformFirstContactSnapshotGPV(t *testing.T) {
	store := newFakeStore()
	s := newTestSession(store)
	s.firstContactGPV = []string{"Device.A", "Device.B"}
	env, err := DecodeEnvelope([]byte(informXML))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	s.Handle(env)
	got := drainKinds(s)
	if len(got) != 2 || got[0] != TaskGPN || got[1] != TaskGPV {
		t.Errorf("first-contact tasks = %v, want [gpn gpv]", got)
	}
}

func drainKinds(s *Session) []TaskType {
	var kinds []TaskType
	for {
		select {
		case task := <-s.taskQueue:
			kinds = append(kinds, task.Type)
		default:
			return kinds
		}
	}
}

// TestSessionEnqueueDropsWhenFull locks the non-blocking enqueue: a full queue
// (offline device) must drop rather than block the caller.
func TestSessionEnqueueDropsWhenFull(t *testing.T) {
	s := &Session{taskQueue: make(chan Task, 1), results: make(chan TaskResult, 1)}
	s.enqueue(Task{Type: TaskGPV}) // fills the single slot
	done := make(chan struct{})
	go func() {
		s.enqueue(Task{Type: TaskGPV}) // must not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked on a full queue")
	}
	if len(s.taskQueue) != 1 {
		t.Errorf("queue len = %d, want 1 (second task dropped)", len(s.taskQueue))
	}
}

// TestFireAndForgetResultNotPublished locks the fix that keeps un-awaited
// Refresh results from filling the results channel: a task with no CommandID
// caches its params but publishes no result.
func TestFireAndForgetResultNotPublished(t *testing.T) {
	s := newTestSession(newFakeStore())
	s.taskQueue <- Task{Type: TaskGPV, Paths: []string{"Device.A"}} // no CommandID
	s.nextTask()
	env, _ := DecodeEnvelope([]byte(gpvRespXML))
	s.onGPV(env.Body.GetParameterValuesResponse)
	select {
	case res := <-s.results:
		t.Errorf("fire-and-forget result should not be published, got %+v", res)
	default:
	}
}

// TestServerConcurrentSameIPNoRace exercises the per-session lock: many
// overlapping requests from one source IP must not race on the shared Session
// (MsgID/DeviceID/inflight). Run with -race to catch a regression.
func TestServerConcurrentSameIPNoRace(t *testing.T) {
	srv := NewServer("0.0.0.0:7547", newFakeStore())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	bodies := []string{informXML, atcXML, "", gpvRespXML, ""}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(b string) {
			defer wg.Done()
			resp, err := http.Post(ts.URL, `text/xml; charset="utf-8"`, strings.NewReader(b))
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(bodies[i%len(bodies)])
	}
	wg.Wait()
}
