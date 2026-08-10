// Command testsuite drives a local functional test of the edge agent. It plays
// every role around the agent-under-test: the mock CloudlyNet cloud on :9000
// (cloud.go), a mock NanoLink CWMP device that dials the agent's :7547
// (device.go + soap.go), and the FTP log-upload path (ftp.go). A GET /health
// gate reports pass/fail. See testsuite/README.md.
//
// The critical behaviour under test: the agent answers AutonomousTransferComplete
// with an empty response (never a Fault), so the CWMP session survives to the
// ACS's read/write turn — the entire reason the CWMP epic exists.
package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"cloudlynet_edgeagent/testsuite/loggen"
)

func main() {
	mode := strings.ToLower(strings.TrimSpace(env("TESTSUITE_MODE", "full")))

	cfg, err := loadNanolinkConfig(env("NANOLINK_CONF", "conf/nanolink.conf"))
	if err != nil {
		log.Fatalf("nanolink config load failed: %v", err)
	}
	cfg = cfg.withEnvOverrides()
	deviceOUI, deviceProductClass, deviceSerial = cfg.Identity.OUI, cfg.Identity.ProductClass, cfg.Identity.Serial
	deviceCWMPID = canonicalID(deviceOUI, deviceProductClass, deviceSerial)

	m, err := loadManifest()
	if err != nil {
		log.Fatalf("nanolink manifest load failed: %v", err)
	}

	st := &state{
		acks:           map[string]string{},
		snapshotParams: map[string]any{},
		typedEvents:    map[string]struct{}{},
		seenDedup:      map[string]struct{}{},
		expectedEvent:  scenarioEventType[loggen.Scenario(cfg.Scenario)],
	}
	dev := newDevice(m)

	ftpDir := env("FTP_DIR", "/ftp")
	_ = os.MkdirAll(ftpDir, 0o755)
	ftpCfg := ftpUploadConfig{
		host:     cfg.FTP.Host,
		user:     cfg.FTP.User,
		pass:     cfg.FTP.Pass,
		interval: cfg.FTP.UploadInterval,
		scenario: loggen.Scenario(cfg.Scenario),
	}

	// The mock device dials the agent's ACS. It retries until the agent is up.
	agentURL := env("AGENT_CWMP_URL", "http://cloudlynet-edgeagent:7547/")
	dialNow := make(chan struct{}, 1)
	go runConnRequestListener(":30005", dialNow)
	go runDeviceLoop(agentURL, dev, dialNow, m)

	// The device Informs (onboards) before its first log upload — the agent's
	// device-id resolution depends on cwmp_devices being populated first.
	go runFTPUploadLoop(dev, ftpCfg)

	if mode == "acsftp" || mode == "acs" {
		log.Printf("mock cwmp-device+ftp health listening on :9000 (platform mock disabled); dialing agent at %s", agentURL)
		log.Fatal(http.ListenAndServe(":9000", acsHealthMux(dev, ftpDir, mode, agentURL)))
	}
	log.Printf("mock cloud listening on :9000; mock device dialing agent at %s", agentURL)
	log.Fatal(http.ListenAndServe(":9000", cloudMux(st)))
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
