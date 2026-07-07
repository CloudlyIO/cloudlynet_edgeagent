package cwmp

import (
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

// Server is the in-agent CWMP ACS. It binds the exact host:port the NanoLink
// already dials (Device.ManagementServer.URL), so replacing the former ACS needs no
// device-side change. Each source IP gets one long-lived Session.
type Server struct {
	Addr            string // e.g. "0.0.0.0:7547"
	store           Store
	cr              crSettings
	firstContactGPV []string // managed paths read once on first contact (optional)
	mu              sync.Mutex
	// sessions are keyed by device source IP. This assumes one CWMP device per
	// source IP — true for the NanoLink deployment (one edge box, one device).
	// Distinct devices behind a single NAT would share a session; multi-device
	// disambiguation (by DeviceID) is out of scope for this single-device wave.
	sessions map[string]*Session
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
	return &Server{Addr: addr, store: store, sessions: map[string]*Session{}}
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
	sess := srv.session(host)

	// Serialize per-device request handling: Handle/nextTask mutate MsgID/
	// DeviceID/inflight, and the response echoes MsgID. Capture the response
	// body + msgID under the lock, then write outside it.
	sess.mu.Lock()
	var respBody interface{}
	if len(strings.TrimSpace(string(body))) == 0 {
		// Empty POST = "your turn, ACS": hand the device its next queued task.
		respBody = sess.nextTask()
	} else {
		env, derr := DecodeEnvelope(body)
		if derr != nil {
			sess.mu.Unlock()
			log.Printf("[CWMP][%s] decode: %v", host, derr)
			http.Error(w, "bad soap", http.StatusBadRequest)
			return
		}
		respBody = sess.Handle(env)
	}
	msgID := sess.MsgID
	sess.mu.Unlock()

	writeResponse(w, sess.IP, msgID, respBody)
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

func (srv *Server) session(ip string) *Session {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if s, ok := srv.sessions[ip]; ok {
		return s
	}
	s := &Session{
		IP:              ip,
		store:           srv.store,
		taskQueue:       make(chan Task, 256),
		results:         make(chan TaskResult, 256),
		firstContactGPV: srv.firstContactGPV,
	}
	srv.sessions[ip] = s
	return s
}

// Store exposes the backing store so the worker/collector can read cached
// params, device inventory, and events without a second handle.
func (srv *Server) Store() Store { return srv.store }

// Enqueue queues a task for the device at deviceIP; it applies on that device's
// next session (device-initiated ~60s, or immediately if a connection request
// lands). Non-blocking: an offline device that lets the queue fill drops tasks
// rather than freezing the worker goroutine.
func (srv *Server) Enqueue(deviceIP string, t Task) { srv.session(deviceIP).enqueue(t) }

// Await blocks until a TaskResult for commandID arrives (empty commandID matches
// any) or the timeout elapses. ok=false on timeout.
func (srv *Server) Await(deviceIP, commandID string, timeout time.Duration) (TaskResult, bool) {
	sess := srv.session(deviceIP)
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
func (srv *Server) RequestAndAwait(deviceIP string, t Task, timeout time.Duration) (TaskResult, bool) {
	srv.Enqueue(deviceIP, t)
	go srv.triggerCR(deviceIP)
	return srv.Await(deviceIP, t.CommandID, timeout)
}

// Refresh queues a fire-and-forget GPV so the parameter cache is current on the
// next device session. Used by telemetry, which reads cached values and does
// not block on the device (config latency ~ inform cadence). No connection
// request — telemetry is not latency-critical.
func (srv *Server) Refresh(deviceID string, paths []string) {
	ip := srv.store.DeviceIP(deviceID)
	if ip == "" || len(paths) == 0 {
		return
	}
	srv.Enqueue(ip, Task{Type: TaskGPV, Paths: paths})
}

func (srv *Server) triggerCR(deviceIP string) {
	if err := TriggerConnectionRequest(deviceIP, srv.cr.user, srv.cr.pass, srv.cr.urlOverride); err != nil {
		log.Printf("[CWMP][%s] connection request failed (non-fatal): %v", deviceIP, err)
	}
}
