// FTP upload path: the mock device curl-uploads real-shaped log archives to the
// vsftpd container (exactly like the real NanoLink), which writes them into the
// shared volume the agent's WatchFTP polls. Also holds the redacted real-log
// corpus loggen replays into every generated archive.
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
// curl-uploads a real-shaped ring archive + bare Devicelog on the configured
// cadence, matching the real device's ~60s cycle.
func runFTPUploadLoop(dev *device, cfg ftpUploadConfig) {
	for dev.informs() == 0 {
		time.Sleep(250 * time.Millisecond)
	}
	corpus := sampleLines()
	upload := func() {
		genCfg := loggen.Config{
			OUI: deviceOUI, Serial: deviceSerial, ProductClass: deviceProductClass,
			PowerOnAt: time.Now().UTC(), Scenario: cfg.scenario,
		}
		archive, deviceLog, err := loggen.Generate(genCfg, corpus)
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
		if err := curlUpload(cfg.host, cfg.user, cfg.pass, archive, "/"+genCfg.ArchiveName()); err != nil {
			log.Printf("ftp archive upload failed: %v", err)
		} else {
			// A real transfer completed -> the device is now due to announce it
			// over CWMP (ATC). runSession emits exactly one ATC per mark, so ATCs
			// track uploads (~60s) rather than every 500ms session.
			dev.markTransfer()
		}
		if err := curlUpload(cfg.host, cfg.user, cfg.pass, deviceLog, "/"+genCfg.DeviceLogName()); err != nil {
			log.Printf("ftp devicelog upload failed: %v", err)
		}

		// Self-triggering scenarios ALSO fire a separate probe upload against
		// the broken path/creds — proves the FTP hop itself faithfully
		// reproduces the real curl failure code, independent of content
		// delivery. Expected to fail; only logged for visibility.
		switch cfg.scenario {
		case loggen.ScenarioFTPPathReject:
			if err := curlUpload(cfg.host, cfg.user, cfg.pass, archive, "/uploads/"+genCfg.ArchiveName()); err != nil {
				log.Printf("ftp-path-reject probe (expected failure, proves curl(25)): %v", err)
			}
		case loggen.ScenarioFTPAuthFail:
			if err := curlUpload(cfg.host, cfg.user, cfg.pass+"-wrong", archive, "/"+genCfg.ArchiveName()); err != nil {
				log.Printf("ftp-auth-fail probe (expected failure, proves curl(67)): %v", err)
			}
		}
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
