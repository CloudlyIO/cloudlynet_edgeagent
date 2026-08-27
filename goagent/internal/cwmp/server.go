package cwmp

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxRequestBytes caps an inbound CWMP request body (memory-DoS guard). A
// full-tree GetParameterNames/Values response is a few MB; 16 MB is ample.
const maxRequestBytes = 16 << 20

// sessionCookie is the HTTP cookie the ACS sets once a session's device
// identity is known (from the Inform). TR-069 requires the CPE to return
// cookies on every subsequent request of the session, which lets the ACS route
// mid-session messages (GPV/SPV responses, empty "your turn" POSTs — none of
// which carry a DeviceID) back to the RIGHT device even when several devices
// share one source IP (NAT, or the testsuite's N-devices-in-one-container
// fleet). Devices that ignore cookies fall back to source-IP routing, which is
// exact for the one-device-per-IP production deployment.
const sessionCookie = "CWMPSID"

// connectionRequestURLPath is where a CPE advertises its Connection Request
// listener; cached from the Inform/GPVs, it is the per-device CR dial target.
const connectionRequestURLPath = "Device.ManagementServer.ConnectionRequestURL"

// Server is the in-agent CWMP ACS. It binds the exact host:port the NanoLink
// already dials (Device.ManagementServer.URL), so replacing the former ACS needs no
// device-side change.
//
// Sessions are keyed by canonical DeviceID (from the Inform), so N devices are
// fully isolated even behind one source IP: each device has its own task queue,
// results channel, and cached-param attribution. Mid-session requests carry no
// DeviceID, so they are routed by the sessionCookie first and by source IP
// (last device that Informed from that IP) as a fallback.
type Server struct {
	Addr            string // e.g. "0.0.0.0:7547"
	store           Store
	cr              crSettings
	firstContactGPV []string // managed paths read once on first contact (optional)
	mu              sync.Mutex
	// sessions is keyed by canonical DeviceID; requests seen before any Inform
	// from their source get a provisional "ip:<host>" entry (which can only ever
	// be handed 204s — tasks are queued by DeviceID).
	sessions map[string]*Session
	// byIP maps a source IP to the session key of the last device that Informed
	// from it — the cookie-less routing fallback. With one device per IP
	// (production) this is exact; cookie routing disambiguates shared IPs.
	byIP map[string]string
}

// crSettings are the connection-request credentials used to trigger an
// on-demand session (see connrequest.go). Optional — blank falls back to the
// device's periodic inform cadence.
type crSettings struct {
	user        string
	pass        string
	urlOverride string
}

func NewServer(addr string, store Store) *Server {
	return &Server{Addr: addr, store: store, sessions: map[string]*Session{}, byIP: map[string]string{}}
}

// UseConnRequest sets the connection-request credentials.
func (srv *Server) UseConnRequest(user, pass, urlOverride string) {
	srv.cr = crSettings{user: user, pass: pass, urlOverride: urlOverride}
}

// UseFirstContactSnapshot makes the ACS read the given managed paths once on a
// device's first contact, so the config snapshot is populated immediately
// instead of waiting for the periodic snapshot cycle. Empty disables it.
func (srv *Server) UseFirstContactSnapshot(paths []string) {
	srv.firstContactGPV = paths
}

// Run blocks serving the ACS. A bind failure is returned so the caller can
// treat it as fatal (a dead ACS listener means no config can flow).
func (srv *Server) Run() error {
	log.Printf("[CWMP] ACS listening on %s", srv.Addr)
	s := &http.Server{
		Addr:         srv.Addr,
		Handler:      srv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	return s.ListenAndServe()
}

func (srv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	// Bound the request body: a legitimate GPN/GPV response for the full tree is
	// a few MB, so 16 MB is ample while capping a rogue LAN peer's memory-DoS.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}

	// Decode BEFORE session selection: an Inform carries the DeviceID that keys
	// the session, so it must never be attributed by source IP alone.
	var env *Envelope
	if len(strings.TrimSpace(string(body))) > 0 {
		env, err = DecodeEnvelope(body)
		if err != nil {
			log.Printf("[CWMP][%s] decode: %v", host, err)
			http.Error(w, "bad soap", http.StatusBadRequest)
			return
		}
	}
	sess := srv.routeSession(host, r, env)

	// Serialize per-device request handling: Handle/nextTask mutate MsgID/
	// DeviceID/inflight, and the response echoes MsgID. Capture the response
	// body + msgID under the lock, then write outside it.
	sess.mu.Lock()
	if host != "" {
		sess.IP = host
	}
	var respBody interface{}
	if env == nil {
		// Empty POST = "your turn, ACS": hand the device its next queued task.
		respBody = sess.nextTask()
	} else {
		respBody = sess.Handle(env)
	}
	msgID := sess.MsgID
	deviceID := sess.DeviceID
	ip := sess.IP
	sess.mu.Unlock()

	if deviceID != "" {
		// Session affinity for the rest of this CWMP session (TR-069 §3.4.2: the
		// CPE MUST return cookies within the session).
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: deviceID, Path: "/"})
	}
	writeResponse(w, ip, msgID, respBody)
}

func writeResponse(w http.ResponseWriter, ip, msgID string, respBody interface{}) {
	if respBody == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	out, err := EncodeResponse(msgID, respBody)
	if err != nil {
		log.Printf("[CWMP][%s] encode: %v", ip, err)
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// routeSession picks the session for an inbound request. Precedence:
//  1. an Inform's own DeviceID (authoritative — also refreshes the IP fallback),
//  2. the session cookie (mid-session affinity, exact under shared IPs),
//  3. the last device that Informed from this source IP,
//  4. a provisional per-IP session (first contact before any Inform).
func (srv *Server) routeSession(host string, r *http.Request, env *Envelope) *Session {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if env != nil && env.Body.Inform != nil {
		id := CanonicalID(env.Body.Inform.DeviceID.OUI,
			env.Body.Inform.DeviceID.ProductClass, env.Body.Inform.DeviceID.SerialNumber)
		srv.byIP[host] = id
		return srv.deviceSessionLocked(id)
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if s, ok := srv.sessions[c.Value]; ok {
			return s
		}
	}
	if key, ok := srv.byIP[host]; ok {
		if s, ok := srv.sessions[key]; ok {
			return s
		}
	}
	key := "ip:" + host
	srv.byIP[host] = key
	if s, ok := srv.sessions[key]; ok {
		return s
	}
	s := srv.newSessionLocked()
	s.IP = host
	srv.sessions[key] = s
	return s
}

// deviceSessionLocked returns (creating if needed) the session keyed by a
// canonical DeviceID. Callers hold srv.mu.
func (srv *Server) deviceSessionLocked(deviceID string) *Session {
	if s, ok := srv.sessions[deviceID]; ok {
		return s
	}
	s := srv.newSessionLocked()
	s.DeviceID = deviceID
	s.IP = srv.store.DeviceIP(deviceID)
	srv.sessions[deviceID] = s
	return s
}

func (srv *Server) newSessionLocked() *Session {
	return &Session{
		store:           srv.store,
		taskQueue:       make(chan Task, 256),
		results:         make(chan TaskResult, 256),
		firstContactGPV: srv.firstContactGPV,
	}
}

func (srv *Server) session(deviceID string) *Session {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.deviceSessionLocked(deviceID)
}

// Store exposes the backing store so the worker/collector can read cached
// params, device inventory, and events without a second handle.
func (srv *Server) Store() Store { return srv.store }

// Enqueue queues a task for the device with the given canonical id; it applies
// on that device's next session (device-initiated ~60s, or immediately if a
// connection request lands). Keying by DeviceID — not IP — is what keeps a
// queued SPV/GPV from being executed by a different device sharing the source
// IP. Non-blocking: an offline device that lets the queue fill drops tasks
// rather than freezing the worker goroutine.
func (srv *Server) Enqueue(deviceID string, t Task) { srv.session(deviceID).enqueue(t) }

// Await blocks until a TaskResult for commandID arrives on the device's own
// session (empty commandID matches any) or the timeout elapses. ok=false on
// timeout.
func (srv *Server) Await(deviceID, commandID string, timeout time.Duration) (TaskResult, bool) {
	sess := srv.session(deviceID)
	deadline := time.After(timeout)
	for {
		select {
		case res := <-sess.results:
			if commandID == "" || res.CommandID == commandID {
				return res, true
			}
		case <-deadline:
			return TaskResult{}, false
		}
	}
}

// RequestAndAwait queues a task, fires a best-effort connection request (async,
// so a slow/unreachable CR never eats the caller's timeout budget), and waits
// for the result. Used for command apply and config snapshots.
func (srv *Server) RequestAndAwait(deviceID string, t Task, timeout time.Duration) (TaskResult, bool) {
	srv.Enqueue(deviceID, t)
	go srv.triggerCR(deviceID)
	return srv.Await(deviceID, t.CommandID, timeout)
}

// Refresh queues a fire-and-forget GPV so the parameter cache is current on the
// next device session. Used by telemetry, which reads cached values and does
// not block on the device (config latency ~ inform cadence). No connection
// request — telemetry is not latency-critical.
func (srv *Server) Refresh(deviceID string, paths []string) {
	if len(paths) == 0 || srv.store.DeviceIP(deviceID) == "" {
		return
	}
	srv.Enqueue(deviceID, Task{Type: TaskGPV, Paths: paths})
}

// crURL resolves the connection-request dial target for a device:
// the configured override (LAN CR escape hatch) wins, then the device's own
// advertised Device.ManagementServer.ConnectionRequestURL (per-device — the
// only correct choice when several devices share one IP with distinct CR
// ports), then the historical <deviceIP>:30005 default.
func (srv *Server) crURL(deviceID string) string {
	if srv.cr.urlOverride != "" {
		return srv.cr.urlOverride
	}
	if adv := srv.store.GetParams(deviceID, []string{connectionRequestURLPath})[connectionRequestURLPath]; adv != "" {
		return adv
	}
	ip := srv.store.DeviceIP(deviceID)
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:30005/", ip)
}

func (srv *Server) triggerCR(deviceID string) {
	url := srv.crURL(deviceID)
	if url == "" {
		return // device never onboarded; nothing to poke
	}
	if err := TriggerConnectionRequest(url, srv.cr.user, srv.cr.pass); err != nil {
		log.Printf("[CWMP][%s] connection request failed (non-fatal): %v", deviceID, err)
	}
}
