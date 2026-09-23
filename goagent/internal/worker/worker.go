package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"cloudlynet_edgeagent/goagent/internal/buffer"
	"cloudlynet_edgeagent/goagent/internal/cloud"
	"cloudlynet_edgeagent/goagent/internal/collector"
	"cloudlynet_edgeagent/goagent/internal/config"
	"cloudlynet_edgeagent/goagent/internal/cwmp"
)

const AgentVersion = "0.1.0"

type Worker struct {
	cfg      *config.Config
	cloud    *cloud.Client
	acs      *cwmp.Server
	manifest *cwmp.Manifest
	buf      *buffer.Buffer
	col      *collector.Collector

	registered bool
}

func New(cfg *config.Config, cloudClient *cloud.Client, acs *cwmp.Server, manifest *cwmp.Manifest, buf *buffer.Buffer, col *collector.Collector) *Worker {
	return &Worker{cfg: cfg, cloud: cloudClient, acs: acs, manifest: manifest, buf: buf, col: col}
}

func (w *Worker) Run(ctx context.Context) error {
	log.Printf("cloudlynet edge agent %s starting: edge_id=%s base_url=%s cwmp_listen=%s poll=%s",
		AgentVersion, w.cfg.Enrollment.EdgeID, w.cfg.Enrollment.BaseURL, w.cfg.CWMP.Listen, w.cfg.PollInterval)
	go w.col.WatchFTP(ctx)
	w.register(ctx)
	w.heartbeat(ctx)
	w.pushTier(ctx, 1)
	w.pushSnapshots(ctx)

	pollT := time.NewTicker(w.cfg.PollInterval)
	hbT := time.NewTicker(w.cfg.HeartbeatInterval)
	t1 := time.NewTicker(w.cfg.TelemetryT1Interval)
	t2 := time.NewTicker(w.cfg.TelemetryT2Interval)
	t3 := time.NewTicker(w.cfg.TelemetryT3Interval)
	snapT := time.NewTicker(w.cfg.SnapshotInterval)
	defer pollT.Stop()
	defer hbT.Stop()
	defer t1.Stop()
	defer t2.Stop()
	defer t3.Stop()
	defer snapT.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollT.C:
			w.handleCommands(ctx)
			w.flushOutbox(ctx)
		case <-hbT.C:
			w.heartbeat(ctx)
		case <-t1.C:
			w.pushTier(ctx, 1)
		case <-t2.C:
			w.pushTier(ctx, 2)
		case <-t3.C:
			w.pushTier(ctx, 3)
		case <-snapT.C:
			w.pushSnapshots(ctx)
		}
	}
}

func (w *Worker) register(ctx context.Context) {
	meta := map[string]any{
		"edge_id":       w.cfg.Enrollment.EdgeID,
		"tenant_id":     w.cfg.Enrollment.TenantID,
		"cwmp_listen":   w.cfg.CWMP.Listen,
		"ftp_watch_dir": w.cfg.FTPWatchDir,
		"buffer_db":     w.cfg.BufferDB,
		"agent_runtime": "docker-or-systemd",
	}
	if err := w.cloud.Register(ctx, cloud.RegisterRequest{AgentVersion: AgentVersion, Meta: meta}); err != nil {
		log.Printf("register failed: %v", err)
		return
	}
	w.registered = true
	log.Printf("registered with cloud: edge_id=%s", w.cfg.Enrollment.EdgeID)
}

func (w *Worker) heartbeat(ctx context.Context) {
	if !w.registered {
		w.register(ctx)
	}
	devices, err := w.col.Inventory(ctx)
	if err != nil {
		log.Printf("inventory failed: %v", err)
		return
	}
	if err := w.cloud.Heartbeat(ctx, cloud.HeartbeatRequest{Devices: devices}); err != nil {
		log.Printf("heartbeat failed: %v", err)
	}
}

func (w *Worker) pushTier(ctx context.Context, tier int) {
	metrics, alarms, err := w.col.CollectTier(ctx, tier)
	if err != nil {
		log.Printf("collect tier %d failed: %v", tier, err)
		return
	}
	// FTP-log events + CWMP events (incl. ATC) share the telemetry batch.
	events := append(w.col.DrainEvents(), w.drainCWMPEvents()...)
	req := cloud.TelemetryRequest{Metrics: metrics, Events: events, Alarms: alarms}
	if len(req.Metrics) == 0 && len(req.Events) == 0 && len(req.Alarms) == 0 {
		return
	}
	body, _ := json.Marshal(req)
	if err := w.buf.Enqueue(ctx, "telemetry", body); err != nil {
		log.Printf("telemetry enqueue failed: %v", err)
		return
	}
	w.flushOutbox(ctx)
}

// drainCWMPEvents pulls buffered CWMP events (e.g. autonomous_transfer_complete)
// out of the store and maps them to the northbound event DTO.
func (w *Worker) drainCWMPEvents() []cloud.EventItem {
	stored := w.acs.Store().DrainEvents()
	if len(stored) == 0 {
		return nil
	}
	out := make([]cloud.EventItem, 0, len(stored))
	for _, e := range stored {
		ts := e.TS.Format(time.RFC3339)
		out = append(out, cloud.EventItem{
			CWMPID:    e.DeviceID,
			Timestamp: ts,
			Module:    e.Module,
			EventType: e.EventType,
			Severity:  e.Severity,
			Message:   e.Message,
			DedupKey:  eventDedupKey(e.DeviceID, e.EventType, e.Message, ts),
		})
	}
	return out
}

func (w *Worker) flushOutbox(ctx context.Context) {
	err := w.buf.Drain(ctx, func(kind string, body []byte) error {
		if kind != "telemetry" {
			return nil
		}
		return w.cloud.SendTelemetryRaw(ctx, body)
	})
	if err != nil {
		log.Printf("outbox drain paused: %v", err)
	}
}

func (w *Worker) pushSnapshots(ctx context.Context) {
	devices, err := w.col.Inventory(ctx)
	if err != nil {
		log.Printf("snapshot inventory failed: %v", err)
		return
	}
	for _, d := range devices {
		params, err := w.col.Snapshot(ctx, d.CWMPID)
		if err != nil {
			log.Printf("snapshot failed for %s: %v", d.CWMPID, err)
			continue
		}
		if len(params) == 0 {
			// A read can be queued before the device dials in. Do not let an
			// empty response replace the last usable cloud snapshot.
			log.Printf("snapshot skipped for %s: no managed parameters yet", d.CWMPID)
			continue
		}
		if err := w.cloud.SendSnapshot(ctx, d.CWMPID, cloud.SnapshotRequest{Params: params, Source: "agent"}); err != nil {
			log.Printf("snapshot post failed for %s: %v", d.CWMPID, err)
		}
	}
}

func (w *Worker) handleCommands(ctx context.Context) {
	poll, err := w.cloud.Poll(ctx)
	if err != nil {
		log.Printf("poll failed: %v", err)
		return
	}
	for _, cmd := range poll.Commands {
		if done, status, result, err := w.buf.AlreadyApplied(ctx, cmd.ID); err == nil && done {
			var ackResult cloud.AckResult
			_ = json.Unmarshal(result, &ackResult)
			_ = w.cloud.Ack(ctx, cmd.ID, cloud.AckRequest{Status: status, Result: ackResult})
			continue
		}
		ack := w.apply(ctx, cmd)
		log.Printf("command %s type=%s -> %s", cmd.ID, cmd.Type, ack.Status)
		result, _ := json.Marshal(ack.Result)
		if err := w.buf.MarkApplied(ctx, cmd.ID, ack.Status, result); err != nil {
			log.Printf("mark applied failed for %s: %v", cmd.ID, err)
		}
		if err := w.cloud.Ack(ctx, cmd.ID, ack); err != nil {
			log.Printf("ack failed for %s: %v", cmd.ID, err)
		}
		if ack.Status == "applied" && len(cmd.Payload.Writes) > 0 {
			w.postCommandSnapshot(ctx, cmd.CWMPID, ack.Result.Readback)
		}
	}
}

func (w *Worker) apply(ctx context.Context, cmd cloud.Command) cloud.AckRequest {
	// DeviceIP is populated by the device's Inform — its absence means the device
	// never onboarded. Task routing itself is by canonical cwmp_id (NOT by IP:
	// several devices can share one source IP, and an IP-routed SPV could be
	// executed by the wrong femtocell).
	if w.acs.Store().DeviceIP(cmd.CWMPID) == "" {
		return failed(fmt.Errorf("no CWMP session for device %s", cmd.CWMPID))
	}
	switch cmd.Type {
	case "configure", "optimise", "heal", "rollback":
		writes := toWrites(cmd.Payload.Writes, w.manifest)
		res, ok := w.acs.RequestAndAwait(cmd.CWMPID, cwmp.Task{Type: cwmp.TaskSPV, Writes: writes, CmdKey: cmd.ID, CommandID: cmd.ID}, w.cfg.CommandVerifyTimeout)
		if !ok {
			return failed(fmt.Errorf("device session timeout applying %s", cmd.ID))
		}
		if res.Err != "" {
			return failed(fmt.Errorf("device rejected write: %s", res.Err))
		}
		if res.Status == 1 {
			// Status 1 = applied but takes effect after a reboot; an immediate
			// read-back would still show the old value, so ack without verifying.
			return cloud.AckRequest{Status: "applied", Result: cloud.AckResult{TaskID: cmd.ID, Detail: "applied; takes effect after device reboot"}}
		}
		// Let the device apply the write before reading it back.
		select {
		case <-ctx.Done():
			return failed(ctx.Err())
		case <-time.After(w.cfg.CommandVerifyDelay):
		}
		expected := expectedValues(cmd.Payload)
		rb, ok := w.acs.RequestAndAwait(cmd.CWMPID, cwmp.Task{Type: cwmp.TaskGPV, Paths: keysOf(expected), CommandID: cmd.ID + ":rb"}, w.cfg.CommandVerifyTimeout)
		if !ok {
			return failed(fmt.Errorf("read-back timeout for %s", cmd.ID))
		}
		readback := collector.ToAnyMap(rb.Params)
		mismatch := verifyExpected(expected, readback)
		status, detail := "applied", ""
		if len(mismatch) > 0 {
			status = "failed"
			detail = fmt.Sprintf("device read-back did not match the requested value within %s", w.cfg.CommandVerifyTimeout)
			log.Printf("command %s verification mismatch: %+v", cmd.ID, mismatch)
		}
		return cloud.AckRequest{Status: status, Result: cloud.AckResult{Readback: readback, Mismatch: mismatch, TaskID: cmd.ID, Detail: detail}}
	case "query":
		res, ok := w.acs.RequestAndAwait(cmd.CWMPID, cwmp.Task{Type: cwmp.TaskGPV, Paths: cmd.Payload.ReadPaths, CommandID: cmd.ID}, w.cfg.CommandVerifyTimeout)
		if !ok {
			return failed(fmt.Errorf("query timeout for %s", cmd.ID))
		}
		return cloud.AckRequest{Status: "applied", Result: cloud.AckResult{Readback: collector.ToAnyMap(res.Params)}}
	case "reboot":
		if _, ok := w.acs.RequestAndAwait(cmd.CWMPID, cwmp.Task{Type: cwmp.TaskReboot, CmdKey: cmd.ID, CommandID: cmd.ID}, w.cfg.CommandVerifyTimeout); !ok {
			return failed(fmt.Errorf("no reboot ack for %s", cmd.ID))
		}
		return cloud.AckRequest{Status: "applied"}
	default:
		return failed(fmt.Errorf("unknown command type %q", cmd.Type))
	}
}

func (w *Worker) postCommandSnapshot(ctx context.Context, cwmpID string, readback map[string]any) {
	if len(readback) == 0 {
		return
	}
	if err := w.cloud.SendSnapshot(ctx, cwmpID, cloud.SnapshotRequest{Params: readback, Source: "command_readback"}); err != nil {
		log.Printf("command snapshot failed: %v", err)
	}
}

func failed(err error) cloud.AckRequest {
	return cloud.AckRequest{Status: "failed", Result: cloud.AckResult{Detail: err.Error()}}
}

// toWrites builds CWMP SetParameterValues entries, attaching the xsi:type from
// the command (falling back to the embedded manifest, then xsd:string).
func toWrites(writes []cloud.Write, manifest *cwmp.Manifest) []cwmp.ParameterValueStruct {
	out := make([]cwmp.ParameterValueStruct, 0, len(writes))
	for _, wr := range writes {
		xsd := wr.XSDType
		if xsd == "" && manifest != nil {
			xsd = manifest.XSD(wr.Path)
		}
		if xsd == "" {
			xsd = "xsd:string"
		}
		out = append(out, cwmp.ParameterValueStruct{
			Name:  wr.Path,
			Value: cwmp.ValueNode{Type: xsd, Text: formatValue(wr.Value)},
		})
	}
	return out
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func expectedValues(payload cloud.CommandPayload) map[string]any {
	expected := payload.Verify
	if len(expected) > 0 {
		return expected
	}
	expected = map[string]any{}
	for _, w := range payload.Writes {
		expected[w.Path] = w.Value
	}
	return expected
}

func verifyExpected(expected map[string]any, readback map[string]any) []map[string]any {
	var mismatch []map[string]any
	for path, want := range expected {
		got, ok := readback[path]
		if !ok || !valuesMatch(want, got) {
			mismatch = append(mismatch, map[string]any{"path": path, "expected": want, "actual": got, "missing": !ok})
		}
	}
	return mismatch
}

// valuesMatch compares an expected value against a device read-back, tolerating
// the CPE's canonical form for booleans (JSON true/false vs TR-069 "1"/"0").
func valuesMatch(want, got any) bool {
	return normalizeValue(formatValue(want)) == normalizeValue(formatValue(got))
}

// formatValue renders a command value for the wire and for comparison. Command
// JSON numbers decode to float64; fmt's default format switches to exponent
// form at magnitude >= 1e6 ("1e+06"), which is not a valid xsd numeric lexical
// form and would both mis-write the device and false-fail read-back. Format
// integer-valued floats plainly instead.
func formatValue(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func normalizeValue(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true":
		return "1"
	case "false":
		return "0"
	default:
		return s
	}
}

func eventDedupKey(deviceID, eventType, message, ts string) string {
	h := sha256.Sum256([]byte(deviceID + "|" + eventType + "|" + message + "|" + ts))
	return hex.EncodeToString(h[:])
}
