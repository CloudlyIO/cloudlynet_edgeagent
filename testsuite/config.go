package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"cloudlynet_edgeagent/testsuite/loggen"
)

// nanolinkConfig is the single .conf driving the mock NanoLink: identity,
// FTP transport, and the named scenario. Env vars (FTP_HOST/FTP_USER/FTP_PASS,
// NANOLINK_SCENARIO — already wired in docker-compose.test.yml) override the file,
// matching this repo's existing env-first convention (see env()).
//
// An optional devices: list switches the process into FLEET MODE: N mock
// femtocells in one process, each with its own identity, param overlay, CWMP
// session loop, FTP upload loop, and connection-request port. With no devices:
// list, behavior is exactly the historical single-device path. See FLEET.md.
type nanolinkConfig struct {
	Identity struct {
		OUI          string `yaml:"oui"`
		ProductClass string `yaml:"product_class"`
		Serial       string `yaml:"serial"`
	} `yaml:"identity"`
	FTP struct {
		Host           string        `yaml:"host"`
		User           string        `yaml:"user"`
		Pass           string        `yaml:"pass"`
		UploadInterval time.Duration `yaml:"upload_interval"`
	} `yaml:"ftp"`
	Scenario string `yaml:"scenario"`
	// Devices is the optional fleet list (conf-file-only — no env equivalent).
	// Empty = single-device mode driven by Identity + NANOLINK_* env overrides.
	Devices []deviceConfig `yaml:"devices"`
}

// deviceConfig is one fleet entry. Serial is required; oui/product_class
// default from the identity block (itself defaulted from the built-ins);
// params is a TR-069 path -> value overlay applied to the manifest-seeded
// store AFTER identity seeding (per-device PhyCellID, CellIdentity, EARFCNDL,
// ReferenceSignalPower, SampleSet CurrentValues, ...). YAML note: values must
// be quoted strings ("449", not 449) — CWMP carries strings.
type deviceConfig struct {
	Serial       string            `yaml:"serial"`
	OUI          string            `yaml:"oui"`
	ProductClass string            `yaml:"product_class"`
	Params       map[string]string `yaml:"params"`
}

func defaultNanolinkConfig() nanolinkConfig {
	var cfg nanolinkConfig
	cfg.Identity.OUI = "8C1F64"
	cfg.Identity.ProductClass = "ENB-N03002-B3"
	// Synthetic serial: cwmp_id (OUI-ProductClass-Serial) is UNIQUE per tenant on
	// the real cloud, so a real device's serial collides in acsftp. Override
	// per-tester via NANOLINK_SERIAL; no effect in full mode.
	cfg.Identity.Serial = "2205609999"
	cfg.FTP.Host = "ftp"
	cfg.FTP.User = "nybsys"
	cfg.FTP.Pass = ""
	cfg.FTP.UploadInterval = 60 * time.Second
	cfg.Scenario = string(loggen.ScenarioHappy)
	return cfg
}

// loadNanolinkConfig reads path, falling back to defaults if the file is
// absent (the .conf is optional — every field also has a hardcoded default).
func loadNanolinkConfig(path string) (nanolinkConfig, error) {
	cfg := defaultNanolinkConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// withEnvOverrides applies the env-var knobs docker-compose.test.yml wires
// (identity: NANOLINK_OUI/NANOLINK_PRODUCT_CLASS/NANOLINK_SERIAL; plus
// FTP_HOST/FTP_USER/FTP_PASS/NANOLINK_SCENARIO), taking precedence over the
// .conf — consistent with this file's env-first convention.
func (c nanolinkConfig) withEnvOverrides() nanolinkConfig {
	// Identity overrides → distinct cwmp_ids for concurrent acsftp runs.
	if v := os.Getenv("NANOLINK_OUI"); v != "" {
		c.Identity.OUI = v
	}
	if v := os.Getenv("NANOLINK_PRODUCT_CLASS"); v != "" {
		c.Identity.ProductClass = v
	}
	if v := os.Getenv("NANOLINK_SERIAL"); v != "" {
		c.Identity.Serial = v
	}
	if v := os.Getenv("FTP_HOST"); v != "" {
		c.FTP.Host = v
	}
	if v := os.Getenv("FTP_USER"); v != "" {
		c.FTP.User = v
	}
	if v := os.Getenv("FTP_PASS"); v != "" {
		c.FTP.Pass = v
	}
	if v := os.Getenv("NANOLINK_SCENARIO"); v != "" {
		c.Scenario = v
	}
	return c
}

// canonicalID mirrors goagent/internal/cwmp.CanonicalID's percent-encoding
// (testsuite can't import goagent/internal — separate module boundary, same
// reason the manifest is duplicated). Must byte-match the agent's encoding or
// the mock device's canonical id never resolves against cwmp_devices.
func canonicalID(oui, productClass, serial string) string {
	enc := func(s string) string { return strings.ReplaceAll(s, "-", "%2D") }
	return enc(oui) + "-" + enc(productClass) + "-" + enc(serial)
}

// crPortBase is the historical single-device connection-request port; fleet
// device i listens on crPortBase+i (device 0 keeps :30005 — back-compat).
const crPortBase = 30005

func crPortFor(index int) int { return crPortBase + index }

// startStagger is the deterministic per-device start offset (index*startStagger)
// that spreads fleet CWMP/FTP loop starts so N devices don't thundering-herd the
// agent. Device 0 starts immediately — single-device timing is unchanged.
const startStagger = 700 * time.Millisecond

func startDelay(index int) time.Duration { return time.Duration(index) * startStagger }

// deviceSpec is one fully-resolved mock device: identity, canonical cwmp_id,
// connection-request port, and the param overlay applied after identity seeding.
type deviceSpec struct {
	Index        int
	OUI          string
	ProductClass string
	Serial       string
	CWMPID       string
	CRPort       int
	// CRURL, when non-empty, is overlaid onto the store's
	// Device.ManagementServer.ConnectionRequestURL and advertised in the Inform.
	// Empty in single-device mode: the manifest's captured value stays untouched
	// and the Inform keeps its historical parameter list (byte-for-byte).
	CRURL  string
	Params map[string]string
}

// deviceSpecs resolves the configured fleet. With no devices: list it returns
// exactly one spec built from the identity block — the historical single-device
// path. In fleet mode each entry needs a unique, non-empty serial; oui and
// product_class default from the identity block.
func (c nanolinkConfig) deviceSpecs() ([]deviceSpec, error) {
	if len(c.Devices) == 0 {
		return []deviceSpec{{
			OUI:          c.Identity.OUI,
			ProductClass: c.Identity.ProductClass,
			Serial:       c.Identity.Serial,
			CWMPID:       canonicalID(c.Identity.OUI, c.Identity.ProductClass, c.Identity.Serial),
			CRPort:       crPortFor(0),
		}}, nil
	}
	seen := make(map[string]struct{}, len(c.Devices))
	specs := make([]deviceSpec, 0, len(c.Devices))
	for i, d := range c.Devices {
		serial := strings.TrimSpace(d.Serial)
		if serial == "" {
			return nil, fmt.Errorf("devices[%d]: serial is required", i)
		}
		if _, dup := seen[serial]; dup {
			// cwmp_id is UNIQUE per tenant on the real cloud; two devices with one
			// serial would fold into a single cwmp_id and silently fight each other.
			return nil, fmt.Errorf("devices[%d]: duplicate serial %q", i, serial)
		}
		seen[serial] = struct{}{}
		oui := d.OUI
		if oui == "" {
			oui = c.Identity.OUI
		}
		productClass := d.ProductClass
		if productClass == "" {
			productClass = c.Identity.ProductClass
		}
		specs = append(specs, deviceSpec{
			Index:        i,
			OUI:          oui,
			ProductClass: productClass,
			Serial:       serial,
			CWMPID:       canonicalID(oui, productClass, serial),
			CRPort:       crPortFor(i),
			Params:       d.Params,
		})
	}
	return specs, nil
}
