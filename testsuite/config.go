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
	cfg.Identity.Serial = "2205600282"
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

// withEnvOverrides applies the same env-var knobs docker-compose.test.yml already
// wires (FTP_HOST/FTP_USER/FTP_PASS/NANOLINK_SCENARIO), taking precedence over
// the .conf — consistent with this file's env-first convention elsewhere.
func (c nanolinkConfig) withEnvOverrides() nanolinkConfig {
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
