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
// enrich the device inventory (replaces the former ACS document dig).
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

// SnapshotPaths is the curated managed-parameter catalogue shown by the NanoLink Config
// tab. It is an alias for the GENERATED ManagedPaths (see managed_paths.go), which is
// derived from smo_sim's managed_params.py, the single write-validation gate. It used to
// be a hand-maintained copy. Status and telemetry paths do not belong in a snapshot.
var SnapshotPaths = ManagedPaths

// maxDeferTicks is scanFTP's FAST resolution window for an upload from a
// not-yet-known device (~30 ticks at the 2s poll interval == ~60s), matching
// the real device's own Inform/upload cadence so a cold-start race resolves on
// its own. Past the window the upload stays deferred — retried every
// resolveRetryTicks — never emitted under a made-up id (see scanFTP).
const maxDeferTicks = 30

// resolveRetryTicks is the slow retry cadence once the fast window is spent
// (~30 ticks == ~60s between device-store lookups per unresolved file).
const resolveRetryTicks = 30

// maxParseRetries bounds how many polls a failing parse is retried before the
// upload is given up on. A real curl→vsftpd STOR is non-atomic, so a 2s poll
// can catch a large .tgz mid-write (a truncated gzip); retrying a few polls
// lets the upload finish rather than dropping it permanently, while a genuinely
// corrupt file is eventually abandoned instead of re-parsed forever.
const maxParseRetries = 5

// maxLogBytes bounds the decompressed size read from a single-file gzip periodic
// log (Log_*.gz / ErrorLog_*.gz) — generous for the tiny real slices (KB) while
// capping a malformed/hostile gzip from ballooning memory.
const maxLogBytes = 8 << 20

// Collector reads device state from the in-agent CWMP ACS (params cached from
// Informs + GPV responses) and parses NanoLink FTP log archives into events.
type Collector struct {
	acs        *cwmp.Server
	rules      *rules.Engine
	ftpDir     string
	mu         sync.Mutex
	events     []cloud.EventItem
	seenPaths  map[string]struct{}
	deferred   map[string]int
	parseFails map[string]int
}

func New(acs *cwmp.Server, ruleEngine *rules.Engine, ftpDir string) *Collector {
	return &Collector{acs: acs, rules: ruleEngine, ftpDir: ftpDir, seenPaths: map[string]struct{}{}, deferred: map[string]int{}, parseFails: map[string]int{}}
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
		if e.IsDir() || !isLogUpload(e.Name()) {
			continue
		}
		path := filepath.Join(c.ftpDir, e.Name())
		present[path] = struct{}{}
		if _, ok := c.seenPaths[path]; ok {
			continue
		}

		if tick := c.deferred[path]; tick >= maxDeferTicks && (tick-maxDeferTicks)%resolveRetryTicks != 0 {
			// Past the fast window: retry resolution only every
			// resolveRetryTicks polls — no per-poll SQLite lookup for a file
			// that may never resolve.
			c.deferred[path]++
			continue
		}
		deviceID, known := c.resolveDeviceID(path)
		if !known {
			// Not yet onboarded (Inform hasn't landed): keep the file in place
			// and re-try, indefinitely. It is NEVER emitted under a made-up id:
			// a telemetry batch keyed to a cwmp_id the cloud doesn't know can be
			// rejected, and the strictly-ordered outbox would wedge behind it,
			// blocking all later telemetry. Bookkeeping stays bounded by the
			// directory contents, and the upload is ingested on the canonical
			// id if the device's Inform ever lands.
			c.deferred[path]++
			if c.deferred[path] == maxDeferTicks {
				log.Printf("ftp upload unresolved after %d polls (no matching Inform); leaving it in place, retrying every %d polls: %s", maxDeferTicks, resolveRetryTicks, path)
			}
			continue
		}
		events, err := c.parseUpload(path, deviceID)
		if err != nil {
			// Retry a bounded number of polls before giving up — a large .tgz
			// caught mid-STOR fails to gunzip but completes shortly. The file is
			// NOT marked seen until it parses, so a still-uploading archive is
			// re-attempted rather than dropped.
			c.parseFails[path]++
			if c.parseFails[path] < maxParseRetries {
				continue
			}
			log.Printf("ftp upload parse failed after %d attempts, giving up: %s: %v", maxParseRetries, path, err)
			c.markSeen(path)
			continue
		}
		c.markSeen(path)
		log.Printf("ftp ingest: %s -> %d event(s) [%s]", filepath.Base(path), len(events), deviceID)
		c.QueueEvents(events)
	}
	// Keep seenPaths/deferred bounded by the directory contents: forget entries
	// that have rotated away.
	for p := range c.seenPaths {
		if _, ok := present[p]; !ok {
			delete(c.seenPaths, p)
		}
	}
	for p := range c.deferred {
		if _, ok := present[p]; !ok {
			delete(c.deferred, p)
		}
	}
	for p := range c.parseFails {
		if _, ok := present[p]; !ok {
			delete(c.parseFails, p)
		}
	}
}

// markSeen records a path as fully processed and clears its deferral/retry
// bookkeeping.
func (c *Collector) markSeen(path string) {
	c.seenPaths[path] = struct{}{}
	delete(c.deferred, path)
	delete(c.parseFails, path)
}

// isLogUpload matches the NanoLink FTP upload shapes the agent ingests. At
// present that is ONLY the routine ~60s periodic feed:
//   - "Log_<date>.<time>+<tz>_<OUI>.<serial>.gz"
//   - "ErrorLog_<date>.<time>+<tz>_<OUI>.<serial>.gz"  (error-slice sibling)
//
// PARKED (#346 follow-up — ingestion revamp): the reboot/power-on ring dump
// ("<OUI>_<serial>_PowerOn_<ts>_continuouslogging.tgz") and the bare Devicelog
// alarm log are no longer emitted by the emulator, and their intake is deferred
// to the ingestion revamp (which will settle the full upload taxonomy: live feed
// vs on-demand ring backfill vs forensic artifacts). The `.tgz`/`_Devicelog`
// check below — together with the matching branches in parseUpload and the
// eventsFromArchive/eventsFromLog readers — is intentionally commented out so a
// real box's reboot dump is currently NOT ingested. Re-enable all of them
// together when the revamp lands. See docs task notes.md.
func isLogUpload(name string) bool {
	// if strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, "_Devicelog") {
	// 	return true
	// }
	return isPeriodicLog(name)
}

// isPeriodicLog reports whether name is a routine periodic-feed upload
// (Log_*.gz / ErrorLog_*.gz): a single-file gzip, distinct from the .tgz ring.
// Matched by BOTH the Log_/ErrorLog_ prefix AND the .gz suffix — never a bare
// ".gz" — so forensic dump artifacts that also end in .gz (e.g. "…_fsm.log.gz")
// are not swept in.
func isPeriodicLog(name string) bool {
	return strings.HasSuffix(name, ".gz") &&
		(strings.HasPrefix(name, "Log_") || strings.HasPrefix(name, "ErrorLog_"))
}

// parseUpload reads the upload with the already-resolved deviceID (the parser
// never re-resolves). Only the routine periodic feed (single-file gzip) is
// handled at present; the ".tgz" ring-dump and bare "Devicelog" branches are
// PARKED (see isLogUpload) — commented out here and re-enabled by the ingestion
// revamp alongside the eventsFromArchive/eventsFromLog readers.
func (c *Collector) parseUpload(path, deviceID string) ([]cloud.EventItem, error) {
	// switch {
	// case strings.HasSuffix(path, ".tgz"):
	// 	return c.eventsFromArchive(path, deviceID)
	// case isPeriodicLog(filepath.Base(path)):
	// 	return c.eventsFromGzipLog(path, deviceID)
	// default:
	// 	return c.eventsFromLog(path, deviceID)
	// }
	return c.eventsFromGzipLog(path, deviceID)
}

// resolveDeviceID resolves a real NanoLink upload's canonical device id via
// the cwmp_devices store the CWMP Inform populates. The upload filename
// carries only OUI+serial (no ProductClass), so the canonical id
// (OUI-ProductClass-Serial) cannot be reconstructed from the filename alone —
// OUI+serial uniquely identify the single onboarded device.
func (c *Collector) resolveDeviceID(path string) (canonicalID string, known bool) {
	oui, serial := ouiSerialFromName(path)
	if oui == "" || serial == "" {
		return "", false
	}
	for _, d := range c.acs.Store().ListDevices() {
		// Delimiter-bound: CanonicalID always joins OUI-ProductClass-Serial with
		// "-" (percent-encoding any literal "-" inside a component first), so
		// "-" only ever appears as the field separator. A bare HasPrefix(d.DeviceID,
		// oui) would also match a stored record whose OUI is a superstring of
		// this one (e.g. a corrupted/duplicate "8C1F644-..." row matching
		// "8C1F64") purely as a string prefix — mis-keying to a phantom device,
		// exactly what this resolver exists to prevent.
		if d.SerialNumber == serial && strings.HasPrefix(d.DeviceID, oui+"-") {
			return d.DeviceID, true
		}
	}
	return "", false
}

// ouiSerialFromName extracts OUI+serial from a real upload filename, handling
// both real shapes:
//   - reboot dump / alarm log "<OUI>_<serial>_PowerOn_…": the leading two
//     _-tokens ("8C1F64_2205609999_PowerOn_…_continuouslogging.tgz").
//   - periodic feed "Log_<date>.<time>+<tz>_<OUI>.<serial>.gz": the OUI.serial
//     pair is the LAST _-token, dot-joined ("…_8C1F64.2205609999.gz"). A
//     leading-_-split would wrongly yield oui="Log" here — hence the shape split.
func ouiSerialFromName(path string) (oui, serial string) {
	base := filepath.Base(path)
	if isPeriodicLog(base) {
		tail := strings.TrimSuffix(base, ".gz")
		if i := strings.LastIndexByte(tail, '_'); i >= 0 {
			tail = tail[i+1:] // "8C1F64.2205609999"
		}
		o, s, ok := strings.Cut(tail, ".")
		if !ok {
			return "", ""
		}
		return o, s
	}
	base = strings.TrimSuffix(base, ".tgz")
	parts := strings.SplitN(base, "_", 3)
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// occurrenceBucket returns the upload's occurrence discriminator for dedup
// keys: the date carried in the periodic filename ("Log_<date>.<time>…"), else
// the file's mtime day (UTC). Re-parsing the SAME file (agent restart) and the
// Log∩ErrorLog overlap of one cycle yield the same bucket, while a
// byte-identical fault line recurring in a later day's upload gets a new one —
// see rules.dedup.
func occurrenceBucket(path string) string {
	base := filepath.Base(path)
	if isPeriodicLog(base) {
		rest := strings.TrimPrefix(strings.TrimPrefix(base, "ErrorLog_"), "Log_")
		if date, _, ok := strings.Cut(rest, "."); ok && len(date) == 8 {
			return date
		}
	}
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime().UTC().Format("20060102")
	}
	return ""
}

// eventsFromArchive reads the gzip-tar reboot/power-on ring dump (numbered
// entries 1…10/index/max). PARKED (#346 follow-up): its parseUpload dispatch is
// commented out pending the ingestion revamp; the reader is kept + unit-tested
// (TestEventsFromArchiveReaderClassifies) so re-enabling it later stays safe.
func (c *Collector) eventsFromArchive(path, deviceID string) ([]cloud.EventItem, error) {
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
	bucket := occurrenceBucket(path)
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
		lines := strings.Split(string(b), "\n")
		out = append(out, c.rules.Apply(lines, deviceID, bucket)...)
	}
}

// eventsFromGzipLog handles the routine periodic feed (Log_*.gz / ErrorLog_*.gz):
// a single gzip-compressed log file — NOT a tar — carrying the same
// "<seq> <ts> [MODULE] <msg>" line format as the ring entries. A truncated read
// (a file caught mid-STOR) returns an error so scanFTP's bounded retry re-attempts
// it rather than dropping it after one gunzip failure.
func (c *Collector) eventsFromGzipLog(path, deviceID string) ([]cloud.EventItem, error) {
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
	b, err := io.ReadAll(io.LimitReader(gz, maxLogBytes))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	return c.rules.Apply(lines, deviceID, occurrenceBucket(path)), nil
}

// eventsFromLog handles a bare (non-archive) upload — the real Devicelog,
// which carries the same "<seq> <ts> [MODULE] <msg>" line format as the ring
// archive entries but is uploaded uncompressed with no .tgz extension.
// PARKED (#346 follow-up): its parseUpload dispatch is commented out pending the
// ingestion revamp; the reader is kept + unit-tested (TestEventsFromLogReaderClassifies).
func (c *Collector) eventsFromLog(path, deviceID string) ([]cloud.EventItem, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	return c.rules.Apply(lines, deviceID, occurrenceBucket(path)), nil
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
