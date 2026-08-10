package cwmp

import (
	"strings"
	"testing"
)

// fakeStore is an in-memory Store for exercising the session state machine.
type fakeStore struct {
	devices     map[string]DeviceRecord
	params      map[string]map[string]string
	writability map[string]bool
	events      []StoredEvent
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		devices:     map[string]DeviceRecord{},
		params:      map[string]map[string]string{},
		writability: map[string]bool{},
	}
}

func (f *fakeStore) UpsertDevice(d DeviceRecord) { f.devices[d.DeviceID] = d }
func (f *fakeStore) CacheParam(id, path, value string) {
	if f.params[id] == nil {
		f.params[id] = map[string]string{}
	}
	f.params[id][path] = value
}
func (f *fakeStore) SaveWritability(id string, _ []ParameterInfoStruct) { f.writability[id] = true }
func (f *fakeStore) HasWritabilityMap(id string) bool                   { return f.writability[id] }
func (f *fakeStore) GetParams(id string, paths []string) map[string]string {
	out := map[string]string{}
	for _, p := range paths {
		if v, ok := f.params[id][p]; ok {
			out[p] = v
		}
	}
	return out
}
func (f *fakeStore) ListDevices() []DeviceRecord {
	var out []DeviceRecord
	for _, d := range f.devices {
		out = append(out, d)
	}
	return out
}
func (f *fakeStore) DeviceIP(id string) string { return f.devices[id].IP }
func (f *fakeStore) EmitEvent(id, module, et, sev, msg string) {
	f.events = append(f.events, StoredEvent{DeviceID: id, Module: module, EventType: et, Severity: sev, Message: msg})
}
func (f *fakeStore) DrainEvents() []StoredEvent { out := f.events; f.events = nil; return out }

func newTestSession(store Store) *Session {
	return &Session{IP: "192.168.8.100", store: store, taskQueue: make(chan Task, 16), results: make(chan TaskResult, 16)}
}

func TestOnInformFirstContactEnqueuesGPN(t *testing.T) {
	store := newFakeStore()
	s := newTestSession(store)
	env, err := DecodeEnvelope([]byte(informXML))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp := s.Handle(env)
	if _, ok := resp.(*InformResponse); !ok {
		t.Fatalf("Inform response = %T, want *InformResponse", resp)
	}
	wantID := "8C1F64-ENB%2DN03002%2DB3-2205609999"
	if s.DeviceID != wantID {
		t.Errorf("DeviceID = %q, want %q", s.DeviceID, wantID)
	}
	if _, ok := store.devices[wantID]; !ok {
		t.Error("device not upserted")
	}
	if store.params[wantID]["Device.DeviceInfo.SoftwareVersion"] != "V1.7.1" {
		t.Error("Inform params not cached")
	}
	if store.devices[wantID].SWVersion != "V1.7.1" {
		t.Errorf("SWVersion = %q, want V1.7.1", store.devices[wantID].SWVersion)
	}
	select {
	case task := <-s.taskQueue:
		if task.Type != TaskGPN {
			t.Errorf("queued task = %s, want gpn", task.Type)
		}
	default:
		t.Error("first contact should enqueue a GetParameterNames writability walk")
	}
}

func TestOnInformNoGPNWhenWritabilityKnown(t *testing.T) {
	store := newFakeStore()
	store.writability["8C1F64-ENB%2DN03002%2DB3-2205609999"] = true
	s := newTestSession(store)
	env, _ := DecodeEnvelope([]byte(informXML))
	s.Handle(env)
	select {
	case <-s.taskQueue:
		t.Error("should not re-walk writability when already loaded")
	default:
	}
}

// TestOnATCReturnsResponseNeverFault is the single most important test in the
// package — it is the entire reason the epic exists.
func TestOnATCReturnsResponseNeverFault(t *testing.T) {
	store := newFakeStore()
	s := newTestSession(store)
	env, err := DecodeEnvelope([]byte(atcXML))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp := s.Handle(env)
	if _, ok := resp.(*AutonomousTransferCompleteResponse); !ok {
		t.Fatalf("ATC response = %T, want *AutonomousTransferCompleteResponse", resp)
	}
	if _, isFault := resp.(*Fault); isFault {
		t.Fatal("ATC must NEVER be answered with a Fault")
	}
	if len(store.events) != 1 {
		t.Fatalf("expected 1 emitted event, got %d", len(store.events))
	}
	ev := store.events[0]
	if ev.EventType != "autonomous_transfer_complete" || ev.Severity != "info" {
		t.Errorf("event = %+v, want type=autonomous_transfer_complete severity=info", ev)
	}
}

func TestOnATCFaultSeverityMajor(t *testing.T) {
	store := newFakeStore()
	s := newTestSession(store)
	s.DeviceID = "dev-1"
	resp := s.onATC(&AutonomousTransferComplete{TargetFileName: "x", FileSize: 10, FaultStruct: FaultStruct{FaultCode: 9002}})
	if _, ok := resp.(*AutonomousTransferCompleteResponse); !ok {
		t.Fatalf("resp = %T", resp)
	}
	if store.events[0].Severity != "major" {
		t.Errorf("severity = %q, want major", store.events[0].Severity)
	}
}

func TestGPVResponseCachesAndReportsResult(t *testing.T) {
	store := newFakeStore()
	s := newTestSession(store)
	s.DeviceID = "dev-1"
	s.taskQueue <- Task{Type: TaskGPV, Paths: []string{"Device.X.RSP"}, CommandID: "cmd-1"}
	if _, ok := s.nextTask().(*GetParameterValues); !ok {
		t.Fatal("nextTask should hand out a GetParameterValues")
	}
	env, _ := DecodeEnvelope([]byte(gpvRespXML))
	s.MsgID = env.Header.ID.Value
	next := s.onGPV(env.Body.GetParameterValuesResponse)
	if next != nil {
		t.Errorf("no further task queued, want nil (204), got %T", next)
	}
	select {
	case res := <-s.results:
		if res.CommandID != "cmd-1" || res.Params["Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower"] != "-8" {
			t.Errorf("result = %+v", res)
		}
	default:
		t.Error("GPV response should publish a TaskResult")
	}
	if store.params["dev-1"]["Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower"] != "-8" {
		t.Error("GPV values should be cached")
	}
}

func TestSPVResponseReportsStatus(t *testing.T) {
	store := newFakeStore()
	s := newTestSession(store)
	s.taskQueue <- Task{Type: TaskSPV, CommandID: "cmd-2"}
	s.nextTask()
	s.onSPV(&SetParameterValuesResponse{Status: 0})
	select {
	case res := <-s.results:
		if res.CommandID != "cmd-2" || res.Type != TaskSPV {
			t.Errorf("result = %+v", res)
		}
	default:
		t.Error("SPV response should publish a TaskResult")
	}
}

func TestGPNResponseSavesWritability(t *testing.T) {
	store := newFakeStore()
	s := newTestSession(store)
	s.DeviceID = "dev-1"
	s.taskQueue <- Task{Type: TaskGPN, Paths: []string{"Device."}, CommandID: "gpn-1"}
	s.nextTask()
	env, _ := DecodeEnvelope([]byte(gpnRespXML))
	s.onGPN(env.Body.GetParameterNamesResponse)
	if !store.writability["dev-1"] {
		t.Error("GPN response should persist the writability map")
	}
}

func TestNextTaskEmptyQueueReturnsNil(t *testing.T) {
	s := newTestSession(newFakeStore())
	if got := s.nextTask(); got != nil {
		t.Errorf("empty queue nextTask = %T, want nil (204)", got)
	}
}

func TestFaultFinishesInflightWithError(t *testing.T) {
	store := newFakeStore()
	s := newTestSession(store)
	s.taskQueue <- Task{Type: TaskSPV, CommandID: "cmd-3"}
	s.nextTask()
	env, _ := DecodeEnvelope([]byte(faultXML))
	s.Handle(env)
	select {
	case res := <-s.results:
		if res.CommandID != "cmd-3" || res.Err == "" {
			t.Errorf("faulted result = %+v, want non-empty Err", res)
		}
	default:
		t.Error("a CPE Fault on an inflight task should publish a failed TaskResult")
	}
}

// TestFormatWritesRedactsSecrets: TR-069 carries credentials in the clear
// (ManagementServer passwords, Wi-Fi passphrases); the SPV trace log must
// never persist a cloud-pushed rotation in plaintext.
func TestFormatWritesRedactsSecrets(t *testing.T) {
	got := formatWrites([]ParameterValueStruct{
		{Name: "Device.ManagementServer.ConnectionRequestPassword", Value: ValueNode{Text: "s3cret"}},
		{Name: "Device.ManagementServer.PeriodicInformInterval", Value: ValueNode{Text: "300"}},
	})
	if strings.Contains(got, "s3cret") {
		t.Fatalf("secret value leaked into the SPV trace: %q", got)
	}
	if !strings.Contains(got, "ConnectionRequestPassword=<redacted>") {
		t.Errorf("secret param not redacted: %q", got)
	}
	if !strings.Contains(got, "PeriodicInformInterval=300") {
		t.Errorf("non-secret value must stay readable for the TR-069 trace: %q", got)
	}
}
