package cwmp

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Session is a per-device CWMP state machine. A CWMP session is a sequence of
// HTTP POSTs: Inform → InformResponse → (the CPE's own RPCs, incl. ATC) →
// empty POST → (the ACS's turn: our queued GPV/SPV/GPN/Reboot) → 204. The ATC
// fault the former ACS returned happened during the CPE's RPC turn, killing the
// session before the ACS could read/write — onATC answering correctly is the
// entire fix.
type Session struct {
	// mu serializes request handling for one device. CWMP is a synchronous
	// request/response protocol (the CPE never pipelines), so a compliant device
	// is already serial; the lock guards against overlapping requests from the
	// same source IP (e.g. a connection-request session racing a periodic Inform)
	// mutating MsgID/DeviceID/inflight concurrently. taskQueue/results are
	// channels and are safe for the worker's Enqueue/Await without this lock.
	mu       sync.Mutex
	IP       string
	DeviceID string // canonical id, set on Inform
	MsgID    string // echoed cwmp:ID of the current request

	store     Store
	taskQueue chan Task
	results   chan TaskResult
	// waiters routes a result to the caller awaiting that specific CommandID, so
	// two concurrent Awaits on one session cannot consume each other's results.
	wmu     sync.Mutex
	waiters map[string]chan TaskResult
	inflight  *Task
	// firstContactGPV, if set, is read once on first contact (alongside the
	// writability walk) to populate the managed-config cache immediately.
	firstContactGPV []string
}

// Handle consumes one decoded CWMP message and returns the response body to
// marshal (nil → HTTP 204, which ends the session cleanly).
func (s *Session) Handle(env *Envelope) interface{} {
	s.MsgID = env.Header.ID.Value
	b := env.Body
	switch {
	case b.Inform != nil:
		return s.onInform(b.Inform)
	case b.GetRPCMethods != nil:
		return s.onGetRPCMethods()
	case b.AutonomousTransferComplete != nil:
		return s.onATC(b.AutonomousTransferComplete)
	case b.TransferComplete != nil:
		return &TransferCompleteResponse{}
	case b.GetParameterValuesResponse != nil:
		return s.onGPV(b.GetParameterValuesResponse)
	case b.SetParameterValuesResponse != nil:
		return s.onSPV(b.SetParameterValuesResponse)
	case b.GetParameterNamesResponse != nil:
		return s.onGPN(b.GetParameterNamesResponse)
	case b.RebootResponse != nil:
		log.Printf("[CWMP][%s] <- Reboot ack", s.DeviceID)
		return s.finishInflight(TaskResult{Type: TaskReboot})
	case b.Fault != nil:
		return s.onFault(b.Fault)
	default:
		// Empty POST or an unmodelled message: the device is ready for our turn.
		return s.nextTask()
	}
}

func (s *Session) onInform(inf *Inform) interface{} {
	s.DeviceID = CanonicalID(inf.DeviceID.OUI, inf.DeviceID.ProductClass, inf.DeviceID.SerialNumber)
	s.store.UpsertDevice(DeviceRecord{
		DeviceID:     s.DeviceID,
		IP:           s.IP,
		Manufacturer: inf.DeviceID.Manufacturer,
		ProductClass: inf.DeviceID.ProductClass,
		SerialNumber: inf.DeviceID.SerialNumber,
		SWVersion:    swVersion(inf.ParameterList),
		LastInformAt: time.Now().UTC(),
	})
	for _, pv := range inf.ParameterList {
		s.store.CacheParam(s.DeviceID, pv.Name, pv.Value.Text)
	}
	log.Printf("[CWMP][%s] Inform events=%v", s.DeviceID, eventCodes(inf.Event))
	if !s.store.HasWritabilityMap(s.DeviceID) {
		// First contact: learn the authoritative writable bits for the whole tree,
		// and (if configured) pull the managed-config catalogue now so the first
		// config snapshot is populated without waiting for the periodic cycle.
		s.enqueue(Task{Type: TaskGPN, Paths: []string{"Device."}})
		if len(s.firstContactGPV) > 0 {
			s.enqueue(Task{Type: TaskGPV, Paths: s.firstContactGPV})
		}
	}
	return &InformResponse{MaxEnvelopes: 1}
}

func (s *Session) onGetRPCMethods() interface{} {
	return &GetRPCMethodsResponse{MethodList: []string{
		"GetRPCMethods", "Inform", "TransferComplete", "AutonomousTransferComplete",
	}}
}

// onATC is the handler the former ACS (and Magma's enodebd) omit. It MUST return the empty
// AutonomousTransferCompleteResponse (never a Fault) so the session survives to
// the ACS's read/write turn. It also records the transfer as a device event for
// the telemetry batch (smo-sim ingests it — sibling issue).
func (s *Session) onATC(atc *AutonomousTransferComplete) interface{} {
	log.Printf("[CWMP][%s] ATC file=%s size=%d fault=%d",
		s.DeviceID, atc.TargetFileName, atc.FileSize, atc.FaultStruct.FaultCode)
	severity := "info"
	if atc.FaultStruct.FaultCode != 0 {
		severity = "major"
	}
	s.store.EmitEvent(s.DeviceID, "TR69", "autonomous_transfer_complete", severity,
		fmt.Sprintf("ATC %s (%dB) fault=%d", atc.TargetFileName, atc.FileSize, atc.FaultStruct.FaultCode))
	return &AutonomousTransferCompleteResponse{}
}

func (s *Session) onGPV(r *GetParameterValuesResponse) interface{} {
	params := make(map[string]string, len(r.ParameterList))
	for _, pv := range r.ParameterList {
		params[pv.Name] = pv.Value.Text
		s.store.CacheParam(s.DeviceID, pv.Name, pv.Value.Text)
	}
	log.Printf("[CWMP][%s] <- GPV %d param(s)", s.DeviceID, len(r.ParameterList))
	return s.finishInflight(TaskResult{Type: TaskGPV, Params: params})
}

func (s *Session) onSPV(r *SetParameterValuesResponse) interface{} {
	label := "applied"
	if r.Status == 1 {
		label = "applied-after-reboot"
	}
	log.Printf("[CWMP][%s] <- SPV status=%d (%s)", s.DeviceID, r.Status, label)
	return s.finishInflight(TaskResult{Type: TaskSPV, Status: r.Status})
}

func (s *Session) onGPN(r *GetParameterNamesResponse) interface{} {
	s.store.SaveWritability(s.DeviceID, r.ParameterList)
	log.Printf("[CWMP][%s] <- GPN %d name(s)", s.DeviceID, len(r.ParameterList))
	return s.finishInflight(TaskResult{Type: TaskGPN, Names: r.ParameterList})
}

func (s *Session) onFault(f *Fault) interface{} {
	log.Printf("[CWMP][%s] CPE Fault %s: %s", s.DeviceID,
		f.Detail.CWMPFault.FaultCode, f.Detail.CWMPFault.FaultString)
	if s.inflight != nil {
		return s.finishInflight(TaskResult{Type: s.inflight.Type, Err: f.Detail.CWMPFault.FaultString})
	}
	return nil
}

// enqueue queues a task without blocking. A full queue means the device is not
// draining it (offline), so we drop rather than block the caller — which may be
// the single worker goroutine (via Enqueue) or an ServeHTTP handler holding the
// session lock (via onInform). Dropped fire-and-forget reads retry next cycle;
// a dropped command surfaces as an Await timeout rather than a frozen worker.
func (s *Session) enqueue(t Task) {
	select {
	case s.taskQueue <- t:
	default:
		log.Printf("[CWMP][%s] task queue full; dropping %s task", s.DeviceID, t.Type)
	}
}

// finishInflight publishes the current task's result and hands the device its
// next queued task. Fire-and-forget tasks (Refresh / first-contact reads) carry
// no CommandID and have no waiter — their params are already cached by onGPV, so
// we skip publishing a result that nobody drains (which would fill the channel).
func (s *Session) finishInflight(res TaskResult) interface{} {
	if s.inflight != nil {
		if s.inflight.CommandID != "" {
			res.CommandID = s.inflight.CommandID
			s.deliver(res)
		}
		s.inflight = nil
	}
	return s.nextTask()
}

// deliver hands a result to the goroutine waiting for THAT command, falling back
// to the shared channel when nobody registered for it.
//
// One device session serves several concurrent callers — the worker applying a
// command and the collector taking a config snapshot both Await on it. When every
// result went to one shared channel, whichever Await happened to receive a result
// it did not recognise DISCARDED it, and the rightful waiter timed out: an applied
// write was reported as "device session timeout" and rolled back. Routing by
// CommandID makes concurrent waiters independent.
func (s *Session) deliver(res TaskResult) {
	s.wmu.Lock()
	ch, ok := s.waiters[res.CommandID]
	s.wmu.Unlock()
	if ok {
		select {
		case ch <- res:
			return
		default:
		}
	}
	select {
	case s.results <- res:
	default:
	}
}

// waitFor registers a private channel for one CommandID. The returned release
// func must be called by the waiter; a result arriving after release falls back
// to the shared channel rather than blocking the session goroutine.
func (s *Session) waitFor(commandID string) (<-chan TaskResult, func()) {
	ch := make(chan TaskResult, 1)
	s.wmu.Lock()
	if s.waiters == nil {
		s.waiters = map[string]chan TaskResult{}
	}
	s.waiters[commandID] = ch
	s.wmu.Unlock()
	return ch, func() {
		s.wmu.Lock()
		delete(s.waiters, commandID)
		s.wmu.Unlock()
	}
}

// nextTask pops one queued task and returns the ACS→CPE request for it, or nil
// (→ 204, session ends) when nothing is queued.
func (s *Session) nextTask() interface{} {
	select {
	case t := <-s.taskQueue:
		s.inflight = &t
		switch t.Type {
		case TaskGPV:
			hint := ""
			if strings.HasSuffix(t.CommandID, ":rb") {
				hint = " (read-back)"
			}
			log.Printf("[CWMP][%s] -> GPV %v%s", s.DeviceID, t.Paths, hint)
			return &GetParameterValues{ParameterNames: t.Paths}
		case TaskSPV:
			log.Printf("[CWMP][%s] -> SPV %s key=%s", s.DeviceID, formatWrites(t.Writes), t.CmdKey)
			return &SetParameterValues{ParameterList: t.Writes, ParameterKey: t.CmdKey}
		case TaskGPN:
			path := "Device."
			if len(t.Paths) > 0 {
				path = t.Paths[0]
			}
			log.Printf("[CWMP][%s] -> GPN %s", s.DeviceID, path)
			return &GetParameterNames{ParameterPath: path, NextLevel: false}
		case TaskReboot:
			log.Printf("[CWMP][%s] -> Reboot key=%s", s.DeviceID, t.CmdKey)
			return &Reboot{CommandKey: t.CmdKey}
		}
	default:
	}
	return nil
}

// formatWrites renders SPV writes as "name=value, …" for the TR-069 log,
// redacting secret parameters: TR-069 carries credentials in the clear
// (Device.ManagementServer.Password, ConnectionRequestPassword, Wi-Fi
// KeyPassphrase, …), and journald/shipped container logs must never hold a
// cloud-pushed rotation in plaintext.
func formatWrites(writes []ParameterValueStruct) string {
	parts := make([]string, len(writes))
	for i, w := range writes {
		v := w.Value.Text
		if isSecretParam(w.Name) {
			v = "<redacted>"
		}
		parts[i] = w.Name + "=" + v
	}
	return strings.Join(parts, ", ")
}

// isSecretParam reports whether a TR-069 parameter path carries a credential.
// Name-based, matching the vendor's own isTr69Password flagging: any path whose
// last segments mention a password/passphrase/key-passphrase/shared secret.
func isSecretParam(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "password") ||
		strings.Contains(n, "passphrase") ||
		strings.Contains(n, "presharedkey") ||
		strings.Contains(n, "wepkey") ||
		strings.Contains(n, "secret")
}

func eventCodes(ev []EventStruct) []string {
	out := make([]string, len(ev))
	for i, e := range ev {
		out[i] = e.EventCode
	}
	return out
}

// swVersion pulls the software version out of an Inform parameter list, if the
// device includes it (it feeds the inventory heartbeat).
func swVersion(params []ParameterValueStruct) string {
	for _, pv := range params {
		if pv.Name == "Device.DeviceInfo.SoftwareVersion" {
			return pv.Value.Text
		}
	}
	return ""
}
