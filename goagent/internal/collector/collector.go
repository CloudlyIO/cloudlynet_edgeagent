package collector

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloudlynet_edgeagent/goagent/internal/cloud"
	"cloudlynet_edgeagent/goagent/internal/cwmp"
	"cloudlynet_edgeagent/goagent/internal/rules"
)

// autonomousTransferCompletePolicy is the managed path formerly exported by the
// (removed) NBI client package. It stays in the snapshot catalogue as a readable
// managed parameter; the agent no longer writes it (the ATC handler obsoletes
// the old policy=None stopgap).
const autonomousTransferCompletePolicy = "Device.X_8C1F64_DebugMgmt.Upload.AutonomousTransferCompletePolicy"

// Inventory liveness/identity paths read from the CWMP parameter cache to
// enrich the device inventory (replaces the old the former ACS document dig).
const (
	pathRFTxStatus = "Device.Services.FAPService.1.FAPControl.LTE.RFTxStatus"
	pathOpState    = "Device.Services.FAPService.1.FAPControl.LTE.OpState"
	pathLANIP      = "Device.LAN.IPAddress"
	pathWANIP      = "Device.WAN.IPAddress"
)

var inventoryStatusPaths = []string{pathRFTxStatus, pathOpState, pathLANIP, pathWANIP}

// snapshotAwait bounds how long a config snapshot waits for a fresh device read
// before falling back to the last cached values.
const snapshotAwait = 10 * time.Second

// SnapshotPaths is the complete curated managed-parameter catalogue shown by the
// NanoLink Config tab. Keep this list aligned with SMO Sim's MANAGED_PARAMS and
// the frontend managed-params.ts catalogue; status/telemetry paths do not belong
// in a configuration snapshot.
var SnapshotPaths = []string{
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.PhyCellID",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNDL",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNUL",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.FreqBandIndicator",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.DLBandwidth",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ULBandwidth",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower",
	"Device.Services.FAPService.1.Capabilities.MaxTxPower",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.PHY.PDSCH.Pa",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.PHY.PDSCH.Pb",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.DRXEnabled",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.OnDurationTimer",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.DRXInactivityTimer",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.LongDRXCycle",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.ShortDRXCycle",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.X_8C1F64_PCH.DefaultPagingCycle",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.IdleMode.IntraFreq.QRxLevMinSIB1",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.IdleMode.IntraFreq.SIntraSearch",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.A2ThresholdRSRP",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.A1ThresholdRSRP",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.Hysteresis",
	"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.TimeToTrigger",
	"Device.ManagementServer.PeriodicInformInterval",
	autonomousTransferCompletePolicy,
}

// Collector reads device state from the in-agent CWMP ACS (params cached from
// Informs + GPV responses) and parses NanoLink FTP log archives into events.
type Collector struct {
	acs     *cwmp.Server
	rules   *rules.Engine
	ftpDir  string
	mu      sync.Mutex
	events  []cloud.EventItem
	seenTGZ map[string]struct{}
}

func New(acs *cwmp.Server, ruleEngine *rules.Engine, ftpDir string) *Collector {
	return &Collector{acs: acs, rules: ruleEngine, ftpDir: ftpDir, seenTGZ: map[string]struct{}{}}
}

// Inventory builds the device inventory from the CWMP store, enriching liveness
// and IP fields from the parameter cache.
func (c *Collector) Inventory(ctx context.Context) ([]cloud.InventoryItem, error) {
	records := c.acs.Store().ListDevices()
	out := make([]cloud.InventoryItem, 0, len(records))
	for _, d := range records {
		item := cloud.InventoryItem{
			CWMPID:       d.DeviceID,
			SerialNumber: d.SerialNumber,
			ProductClass: d.ProductClass,
			SWVersion:    d.SWVersion,
		}
		if !d.LastInformAt.IsZero() {
			item.LastInformAt = d.LastInformAt.Format(time.RFC3339)
		}
		st := c.acs.Store().GetParams(d.DeviceID, inventoryStatusPaths)
		item.AdminLANIP = st[pathLANIP]
		item.WANIP = st[pathWANIP]
		if v, ok := st[pathRFTxStatus]; ok {
			b := truthy(v)
			item.RFTxStatus = &b
		}
		if v, ok := st[pathOpState]; ok {
			b := truthy(v)
			item.OpState = &b
		}
		out = append(out, item)
	}
	return out, nil
}

// CollectTier reads a tier's canonical metric keys (and, for T3, FaultMgmt alarms) for every
// NanoLink from the cached parameter store, and queues a fresh GPV read so the cache is current
// next cycle. Devices with no readable metric for the tier are skipped so empty samples are not
// pushed. Alarms are only collected for T3.
func (c *Collector) CollectTier(ctx context.Context, tier int) ([]cloud.MetricSample, []cloud.AlarmItem, error) {
	devices, err := c.Inventory(ctx)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	readPaths := tierReadPaths(tier)
	if tier == 3 {
		readPaths = append(readPaths, alarmReadPaths()...)
	}
	samples := make([]cloud.MetricSample, 0, len(devices))
	var alarms []cloud.AlarmItem
	for _, d := range devices {
		raw := ToAnyMap(c.acs.Store().GetParams(d.CWMPID, readPaths))
		// Queue a fresh device read for the next collection cycle.
		c.acs.Refresh(d.CWMPID, readPaths)
		metrics := buildMetrics(tier, raw)
		// Fall back to inventory-derived liveness for T1 when the cache omits them.
		if tier == 1 {
			if _, ok := metrics["rf_tx_status"]; !ok && d.RFTxStatus != nil {
				metrics["rf_tx_status"] = *d.RFTxStatus
			}
			if _, ok := metrics["op_state"]; !ok && d.OpState != nil {
				metrics["op_state"] = *d.OpState
			}
		}
		if len(metrics) > 0 {
			samples = append(samples, cloud.MetricSample{CWMPID: d.CWMPID, Timestamp: now, Tier: tier, Metrics: metrics})
		}
		if tier == 3 {
			alarms = append(alarms, buildAlarms(d.CWMPID, now, raw)...)
		}
	}
	return samples, alarms, nil
}

// Snapshot reads the managed configuration catalogue for a device, preferring a
// fresh device read and falling back to the last cached values. The worker skips
// publishing an all-empty result.
func (c *Collector) Snapshot(ctx context.Context, cwmpID string) (map[string]any, error) {
	ip := c.acs.Store().DeviceIP(cwmpID)
	if ip == "" {
		return map[string]any{}, nil
	}
	res, ok := c.acs.RequestAndAwait(ip, cwmp.Task{Type: cwmp.TaskGPV, Paths: SnapshotPaths, CommandID: "snapshot:" + cwmpID}, snapshotAwait)
	if ok && len(res.Params) > 0 {
		return ToAnyMap(res.Params), nil
	}
	return ToAnyMap(c.acs.Store().GetParams(cwmpID, SnapshotPaths)), nil
}

func (c *Collector) QueueEvents(events []cloud.EventItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, events...)
}

func (c *Collector) DrainEvents() []cloud.EventItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.events
	c.events = nil
	return out
}

func (c *Collector) WatchFTP(ctx context.Context) {
	if c.ftpDir == "" {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.scanFTP()
		}
	}
}

func (c *Collector) scanFTP() {
	entries, err := os.ReadDir(c.ftpDir)
	if err != nil {
		return
	}
	present := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tgz") {
			continue
		}
		path := filepath.Join(c.ftpDir, e.Name())
		present[path] = struct{}{}
		if _, ok := c.seenTGZ[path]; ok {
			continue
		}
		c.seenTGZ[path] = struct{}{}
		events, err := c.eventsFromArchive(path)
		if err != nil {
			log.Printf("ftp archive parse failed: %v", err)
			continue
		}
		c.QueueEvents(events)
	}
	// Keep seenTGZ bounded by the directory contents: forget archives that have rotated away.
	for p := range c.seenTGZ {
		if _, ok := present[p]; !ok {
			delete(c.seenTGZ, p)
		}
	}
}

func (c *Collector) eventsFromArchive(path string) ([]cloud.EventItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var out []cloud.EventItem
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.FileInfo().IsDir() {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 2<<20))
		if err != nil {
			return nil, err
		}
		module := moduleFromName(h.Name)
		lines := strings.Split(string(b), "\n")
		out = append(out, c.rules.Apply(module, lines, deviceFromName(path))...)
	}
}

func moduleFromName(name string) string {
	up := strings.ToUpper(name)
	switch {
	case strings.Contains(up, "FILE_TRANS"):
		return "FILE_TRANS"
	case strings.Contains(up, "TR69"):
		return "TR69"
	case strings.Contains(up, "FM"):
		return "FM"
	default:
		return "UNKNOWN"
	}
}

func deviceFromName(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".tgz")
	if i := strings.Index(base, "_"); i > 0 {
		return base[:i]
	}
	return base
}

// ToAnyMap widens a CWMP string param map to the map[string]any the cloud DTOs
// and metric builders use. Exported so the worker reuses it for command read-backs.
func ToAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// truthy interprets a cached TR-069 string value as a boolean liveness flag.
func truthy(v string) bool {
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "up")
}
