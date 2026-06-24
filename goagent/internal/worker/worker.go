package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"cloudlynet_edgeagent/goagent/internal/buffer"
	"cloudlynet_edgeagent/goagent/internal/cloud"
	"cloudlynet_edgeagent/goagent/internal/collector"
	"cloudlynet_edgeagent/goagent/internal/config"
	"cloudlynet_edgeagent/goagent/internal/genieacs"
)

const AgentVersion = "0.1.0"

type Worker struct {
	cfg   *config.Config
	cloud *cloud.Client
	nbi   *genieacs.Client
	buf   *buffer.Buffer
	col   *collector.Collector
	// baselined tracks NanoLinks whose ATC-policy baseline SPV has been applied.
	// Accessed only from the single Run loop goroutine, so no lock is needed.
	baselined  map[string]bool
	registered bool
}

func New(cfg *config.Config, cloudClient *cloud.Client, nbi *genieacs.Client, buf *buffer.Buffer, col *collector.Collector) *Worker {
	return &Worker{cfg: cfg, cloud: cloudClient, nbi: nbi, buf: buf, col: col, baselined: map[string]bool{}}
}

func (w *Worker) Run(ctx context.Context) error {
	log.Printf("cloudlynet edge agent %s starting: edge_id=%s base_url=%s genieacs=%s poll=%s",
		AgentVersion, w.cfg.Enrollment.EdgeID, w.cfg.Enrollment.BaseURL, w.cfg.GenieACSNBIURL, w.cfg.PollInterval)
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
		"genieacs_url":  w.cfg.GenieACSNBIURL,
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
	for _, d := range devices {
		if w.baselined[d.GenieACSID] {
			continue
		}
		if err := w.nbi.EnsureBaseline(ctx, d.GenieACSID); err != nil {
			log.Printf("baseline ensure failed for %s: %v", d.GenieACSID, err)
			continue
		}
		w.baselined[d.GenieACSID] = true
		log.Printf("baseline ATC policy ensured for %s", d.GenieACSID)
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
	req := cloud.TelemetryRequest{Metrics: metrics, Events: w.col.DrainEvents(), Alarms: alarms}
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
		params, err := w.col.Snapshot(ctx, d.GenieACSID)
		if err != nil {
			log.Printf("snapshot failed for %s: %v", d.GenieACSID, err)
			continue
		}
		if len(params) == 0 {
			// A GPV task can be accepted before GenieACS has refreshed its cache.
			// Do not let an empty response replace the last usable cloud snapshot.
			log.Printf("snapshot skipped for %s: GenieACS returned no managed parameters", d.GenieACSID)
			continue
		}
		if err := w.cloud.SendSnapshot(ctx, d.GenieACSID, cloud.SnapshotRequest{Params: params, Source: "agent"}); err != nil {
			log.Printf("snapshot post failed for %s: %v", d.GenieACSID, err)
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
			w.postCommandSnapshot(ctx, cmd.GenieACSID, ack.Result.Readback)
		}
	}
}

func (w *Worker) apply(ctx context.Context, cmd cloud.Command) cloud.AckRequest {
	switch cmd.Type {
	case "configure", "optimise", "heal", "rollback":
		taskID, err := w.nbi.SetParams(ctx, cmd.GenieACSID, cmd.Payload.Writes)
		if err != nil {
			return failed(err)
		}
		select {
		case <-ctx.Done():
			return failed(ctx.Err())
		case <-time.After(w.cfg.CommandVerifyDelay):
		}
		paths := writePaths(cmd.Payload.Writes)
		expected := expectedValues(cmd.Payload)
		readback, err := w.nbi.GetParamsMatching(ctx, cmd.GenieACSID, paths, expected, w.cfg.CommandVerifyTimeout)
		if err != nil {
			return failed(err)
		}
		mismatch := verifyExpected(expected, readback)
		status := "applied"
		detail := ""
		if len(mismatch) > 0 {
			status = "failed"
			detail = fmt.Sprintf("device read-back did not match the requested value within %s", w.cfg.CommandVerifyTimeout)
			log.Printf("command %s verification mismatch: %+v", cmd.ID, mismatch)
		}
		return cloud.AckRequest{Status: status, Result: cloud.AckResult{Readback: readback, Mismatch: mismatch, TaskID: taskID, Detail: detail}}
	case "query":
		// Let the GPV refresh task land before reading back (same settle budget as configure).
		select {
		case <-ctx.Done():
			return failed(ctx.Err())
		case <-time.After(w.cfg.CommandVerifyDelay):
		}
		readback, err := w.nbi.GetParams(ctx, cmd.GenieACSID, cmd.Payload.ReadPaths)
		if err != nil {
			return failed(err)
		}
		return cloud.AckRequest{Status: "applied", Result: cloud.AckResult{Readback: readback}}
	case "reboot":
		taskID, err := w.nbi.Reboot(ctx, cmd.GenieACSID)
		if err != nil {
			return failed(err)
		}
		return cloud.AckRequest{Status: "applied", Result: cloud.AckResult{TaskID: taskID}}
	default:
		return failed(fmt.Errorf("unknown command type %q", cmd.Type))
	}
}

func (w *Worker) postCommandSnapshot(ctx context.Context, genieacsID string, readback map[string]any) {
	if len(readback) == 0 {
		return
	}
	if err := w.cloud.SendSnapshot(ctx, genieacsID, cloud.SnapshotRequest{Params: readback, Source: "command_readback"}); err != nil {
		log.Printf("command snapshot failed: %v", err)
	}
}

func failed(err error) cloud.AckRequest {
	return cloud.AckRequest{Status: "failed", Result: cloud.AckResult{Detail: err.Error()}}
}

func writePaths(writes []cloud.Write) []string {
	out := make([]string, 0, len(writes))
	for _, w := range writes {
		out = append(out, w.Path)
	}
	return out
}

func verify(payload cloud.CommandPayload, readback map[string]any) []map[string]any {
	return verifyExpected(expectedValues(payload), readback)
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
		if !ok || fmt.Sprint(got) != fmt.Sprint(want) {
			mismatch = append(mismatch, map[string]any{"path": path, "expected": want, "actual": got, "missing": !ok})
		}
	}
	return mismatch
}
