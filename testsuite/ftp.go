// FTP upload path: the mock device curl-uploads real-shaped log archives to the
// vsftpd container (exactly like the real NanoLink), which writes them into the
// shared volume the agent's WatchFTP polls. Also holds the redacted real-log
// corpus loggen replays into every generated slice.
package main

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"cloudlynet_edgeagent/testsuite/loggen"
)

//go:embed fixtures/real_sample.log
var realSampleFixture string

// sampleLines returns the redacted real-log corpus lines loggen replays
// alongside its synthetic per-module filler.
func sampleLines() []string {
	var out []string
	for _, l := range strings.Split(realSampleFixture, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// ftpUploadConfig is the mock device's FTP transport, mirroring the real
// NanoLink's curl -u <user>:<pass> ftp://<host>/ upload, plus the scenario
// that drives both the generated log content and (for the self-triggering
// scenarios) the upload path/credentials.
type ftpUploadConfig struct {
	host     string
	user     string
	pass     string
	interval time.Duration
	scenario loggen.Scenario
	// retention is how long the device keeps its own uploads on the FTP server
	// before deleting them (<= 0 keeps everything). See pruneOwnUploads.
	retention time.Duration
}

// defaultFTPRetention bounds how long a mock device leaves its own uploads on
// the FTP server. A real NanoLink's FTP target is rotated by its operator; the
// mock has no operator, and an unpruned watch dir grew to ~354k files in ~78
// days, which every agent restart then had to list (and, before the agent
// persisted its ingest progress, re-parse). Six hours is far longer than the
// agent needs (it ingests within one 2s poll) and keeps a day's debugging
// window per device at 60s cadence small (~360 Log files).
const defaultFTPRetention = 6 * time.Hour

// pruneEveryCycles spaces retention sweeps: one NLST per device per sweep is
// cheap, but there is no need to list the directory every 60s upload. The
// first cycle always sweeps, so leftovers from before a restart go promptly.
const pruneEveryCycles = 10

// deleteBatch bounds the DELE commands passed to one curl invocation, keeping
// argv small when a long-unpruned directory holds thousands of expired files.
const deleteBatch = 200

// uploadStampLayout is loggen's filename timestamp ("<date>.<time><tz>").
const uploadStampLayout = "20060102.1504-0700"

// runFTPUploadLoop waits for the device to Inform at least once (the agent's
// device-id resolution depends on cwmp_devices being populated first), then
// curl-uploads a real-shaped periodic Log_*.gz every cycle (the ~60s VendorLog
// feed) and, only during an incident window (see loggen.IsIncidentCycle), the
// correlated ErrorLog_*.gz — matching the real device's cadence.
func runFTPUploadLoop(dev *device, cfg ftpUploadConfig) {
	for dev.informs() == 0 {
		time.Sleep(250 * time.Millisecond)
	}
	corpus := sampleLines()
	cycle := 0
	upload := func() {
		// Identity comes from the OWNING device (per-device in fleet mode), so
		// loggen.rewriteIdentity stamps each device's own OUI/serial/product-class
		// into its generated log content and filenames.
		genCfg := loggen.Config{
			OUI: dev.oui, Serial: dev.serial, ProductClass: dev.productClass,
			At: time.Now().UTC(), Scenario: cfg.scenario, Cycle: cycle,
		}
		logGz, errorLogGz, err := loggen.Generate(genCfg, corpus)
		if err != nil {
			log.Printf("log generation failed: %v", err)
			return
		}

		// The content-carrying upload ALWAYS targets the FTP root with the
		// configured creds, so the scenario's staged log line reliably reaches
		// the agent — this is what /health's typed-event gate asserts. If this
		// upload instead targeted the scenario's broken path/creds, the bytes
		// carrying the fault line would never arrive (the point of the fault),
		// making the typed-event assertion hollow for exactly the two
		// scenarios meant to prove it.
		if err := curlUpload(cfg.host, cfg.user, cfg.pass, logGz, "/"+genCfg.LogName()); err != nil {
			log.Printf("ftp Log upload failed: %v", err)
		} else {
			log.Printf("ftp Log upload ok: %s (%d bytes)", genCfg.LogName(), len(logGz))
			// A real transfer completed -> the device is now due to announce it over
			// CWMP (ATC), naming the file it just uploaded. runSession emits exactly
			// one ATC per mark, so ATCs track uploads (~60s) not every 500ms session.
			dev.markTransfer(genCfg.LogName(), len(logGz))
		}
		// The ErrorLog is emitted ONLY during an incident window (errorLogGz != nil):
		// its lines are the burst of error/alarm lines that also appear in this
		// cycle's Log, so the cloud's content-dedup collapses the overlap — the real
		// device uploads an ErrorLog around an incident, not on a fixed timer.
		if errorLogGz != nil {
			if err := curlUpload(cfg.host, cfg.user, cfg.pass, errorLogGz, "/"+genCfg.ErrorLogName()); err != nil {
				log.Printf("ftp ErrorLog upload failed: %v", err)
			} else {
				log.Printf("ftp ErrorLog upload ok (incident): %s (%d bytes)", genCfg.ErrorLogName(), len(errorLogGz))
			}
		}

		// Self-triggering scenarios ALSO fire a separate probe upload against
		// the broken path/creds — proves the FTP hop itself faithfully
		// reproduces the real curl failure code, independent of content
		// delivery. Expected to fail; only logged for visibility.
		switch cfg.scenario {
		case loggen.ScenarioFTPPathReject:
			if err := curlUpload(cfg.host, cfg.user, cfg.pass, logGz, "/uploads/"+genCfg.LogName()); err != nil {
				log.Printf("ftp-path-reject probe (expected failure, proves curl(25)): %v", err)
			}
		case loggen.ScenarioFTPAuthFail:
			if err := curlUpload(cfg.host, cfg.user, cfg.pass+"-wrong", logGz, "/"+genCfg.LogName()); err != nil {
				log.Printf("ftp-auth-fail probe (expected failure, proves curl(67)): %v", err)
			}
		}
		if cfg.retention > 0 && cycle%pruneEveryCycles == 0 {
			pruneOwnUploads(dev, cfg, time.Now())
		}
		cycle++
	}
	upload()
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for range ticker.C {
		upload()
	}
}

// pruneOwnUploads deletes this device's uploads older than cfg.retention from
// the FTP server, over FTP (NLST, then DELE) so the retention path goes
// through the same curl -> vsftpd hop as the uploads. Each device prunes only
// files carrying its own OUI.serial, so fleet devices never delete each
// other's uploads. Failures are logged and retried on the next sweep.
func pruneOwnUploads(dev *device, cfg ftpUploadConfig, now time.Time) {
	names, err := curlList(cfg.host, cfg.user, cfg.pass)
	if err != nil {
		log.Printf("ftp retention list failed (device %s): %v", dev.serial, err)
		return
	}
	expired := expiredUploads(names, dev.oui, dev.serial, now, cfg.retention)
	if len(expired) == 0 {
		return
	}
	if err := curlDelete(cfg.host, cfg.user, cfg.pass, expired); err != nil {
		log.Printf("ftp retention delete failed (device %s): %v", dev.serial, err)
		return
	}
	log.Printf("ftp retention: deleted %d upload(s) older than %s (device %s)", len(expired), cfg.retention, dev.serial)
}

// uploadTime parses the timestamp out of one of this device's own upload
// names ("Log_|ErrorLog_<date>.<time><tz>_<OUI>.<serial>.gz"). ok is false for
// any other name, including another device's uploads and anything whose stamp
// does not parse; such files are never this device's to delete.
func uploadTime(name, oui, serial string) (time.Time, bool) {
	var rest string
	switch {
	case strings.HasPrefix(name, "Log_"):
		rest = strings.TrimPrefix(name, "Log_")
	case strings.HasPrefix(name, "ErrorLog_"):
		rest = strings.TrimPrefix(name, "ErrorLog_")
	default:
		return time.Time{}, false
	}
	tail := "_" + oui + "." + serial + ".gz"
	if !strings.HasSuffix(rest, tail) {
		return time.Time{}, false
	}
	ts, err := time.Parse(uploadStampLayout, strings.TrimSuffix(rest, tail))
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// expiredUploads selects this device's uploads stamped before now-keep. A
// keep <= 0 selects nothing (keep everything).
func expiredUploads(names []string, oui, serial string, now time.Time, keep time.Duration) []string {
	if keep <= 0 {
		return nil
	}
	cutoff := now.Add(-keep)
	var out []string
	for _, n := range names {
		if ts, ok := uploadTime(n, oui, serial); ok && ts.Before(cutoff) {
			out = append(out, n)
		}
	}
	return out
}

// curlList returns the FTP root's entry names (curl --list-only == NLST).
func curlList(host, user, pass string) ([]string, error) {
	url := fmt.Sprintf("ftp://%s/", host)
	cmd := exec.Command("curl", "-sS", "--list-only", "-u", user+":"+pass, "--connect-timeout", "5", url)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var names []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// curlDelete removes names from the FTP root with DELE quote commands. -I
// (no body) keeps curl from opening a data connection or listing the
// directory after the quotes; the "*" prefix lets the remaining DELEs run
// when one fails (e.g. a file already gone), so one stale name cannot block
// a whole batch.
func curlDelete(host, user, pass string, names []string) error {
	url := fmt.Sprintf("ftp://%s/", host)
	for start := 0; start < len(names); start += deleteBatch {
		end := min(start+deleteBatch, len(names))
		args := []string{"-sS", "-I", "-u", user + ":" + pass, "--connect-timeout", "5"}
		for _, n := range names[start:end] {
			args = append(args, "-Q", "*DELE "+n)
		}
		args = append(args, url)
		if out, err := exec.Command("curl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// curlUpload writes data to a scratch file and shells out to curl (installed
// in the testsuite image) rather than a Go FTP client — faithful to the real
// device, whose logs literally show
// "curl -vvv -T <file> ... -u nybsys:*** -g 'ftp://192.168.8.100/'".
func curlUpload(host, user, pass string, data []byte, remotePath string) error {
	tmp, err := os.CreateTemp("", "nanolink-upload-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	url := fmt.Sprintf("ftp://%s%s", host, remotePath)
	cmd := exec.Command("curl", "-sS", "-T", tmpPath, "-u", user+":"+pass, "--connect-timeout", "5", url)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
