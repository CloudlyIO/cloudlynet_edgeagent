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
}

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
		genCfg := loggen.Config{
			OUI: deviceOUI, Serial: deviceSerial, ProductClass: deviceProductClass,
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
		cycle++
	}
	upload()
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for range ticker.C {
		upload()
	}
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
