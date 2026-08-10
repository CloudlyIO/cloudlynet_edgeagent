package main

import (
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
