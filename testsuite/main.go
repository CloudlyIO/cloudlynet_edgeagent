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
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"cloudlynet_edgeagent/testsuite/loggen"
)

func main() {
	mode := strings.ToLower(strings.TrimSpace(env("TESTSUITE_MODE", "full")))

	cfg, err := loadNanolinkConfig(env("NANOLINK_CONF", "conf/nanolink.conf"))
	if err != nil {
		log.Fatalf("nanolink config load failed: %v", err)
	}
	cfg = cfg.withEnvOverrides()
	specs, err := cfg.deviceSpecs()
	if err != nil {
		log.Fatalf("nanolink config invalid: %v", err)
	}
	// Fleet mode is conf-file-only (a devices: list); the NANOLINK_* identity
	// envs keep steering the single-device path (and the identity-block defaults
	// fleet entries inherit oui/product_class from). See FLEET.md.
	fleetMode := len(cfg.Devices) > 0
	if fleetMode {
		// Each device advertises its own connection-request URL, like a real CPE
		// fleet behind one IP would advertise distinct ports.
		crHost := env("NANOLINK_CR_HOST", defaultCRHost())
		for i := range specs {
			specs[i].CRURL = fmt.Sprintf("http://%s:%d/", crHost, specs[i].CRPort)
		}
	}

	m, err := loadManifest()
	if err != nil {
		log.Fatalf("nanolink manifest load failed: %v", err)
	}

	st := stateForSpecs(&state{
		acks:           map[string]string{},
		snapshotParams: map[string]any{},
		typedEvents:    map[string]struct{}{},
		seenDedup:      map[string]struct{}{},
		expectedEvent:  scenarioEventType[loggen.Scenario(cfg.Scenario)],
	}, specs, fleetMode)

	devices := make([]*device, len(specs))
	for i, spec := range specs {
		devices[i] = newDeviceFromSpec(m, spec)
	}

	ftpDir := env("FTP_DIR", "/ftp")
	_ = os.MkdirAll(ftpDir, 0o755)
	ftpCfg := ftpUploadConfig{
		host:     cfg.FTP.Host,
		user:     cfg.FTP.User,
		pass:     cfg.FTP.Pass,
		interval: cfg.FTP.UploadInterval,
		scenario: loggen.Scenario(cfg.Scenario),
	}

	// The mock devices dial the agent's ACS, retrying until the agent is up.
	// Fleet starts are staggered deterministically (index*700ms) so N devices
	// don't thundering-herd the agent; device 0 starts immediately, exactly like
	// the historical single-device path.
	agentURL := env("AGENT_CWMP_URL", "http://cloudlynet-edgeagent:7547/")
	for _, dev := range devices {
		dev := dev
		go func() {
			time.Sleep(startDelay(dev.index))
			dialNow := make(chan struct{}, 1)
			go runConnRequestListener(fmt.Sprintf(":%d", dev.crPort), dialNow)
			go runDeviceLoop(agentURL, dev, dialNow, m)

			// The device Informs (onboards) before its first log upload — the agent's
			// device-id resolution depends on cwmp_devices being populated first.
			go runFTPUploadLoop(dev, ftpCfg)
		}()
	}

	if mode == "acsftp" || mode == "acs" {
		log.Printf("mock cwmp-device+ftp health listening on :9000 (platform mock disabled); dialing agent at %s", agentURL)
		log.Fatal(http.ListenAndServe(":9000", acsHealthMuxFleet(devices, ftpDir, mode, agentURL)))
	}
	log.Printf("mock cloud listening on :9000; mock device dialing agent at %s", agentURL)
	log.Fatal(http.ListenAndServe(":9000", cloudMux(st, devices...)))
}

// defaultCRHost is the host fleet devices advertise in their per-device
// ConnectionRequestURL: the container hostname (resolvable on the compose
// network), falling back to localhost outside one.
func defaultCRHost() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
