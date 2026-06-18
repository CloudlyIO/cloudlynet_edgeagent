package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Enrollment struct {
	Version  int    `json:"v"`
	TenantID string `json:"tenant_id"`
	EdgeID   string `json:"edge_id"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

type Config struct {
	EnrollmentToken     string        `yaml:"enrollment_token"`
	PollInterval        time.Duration `yaml:"poll_interval"`
	HeartbeatInterval   time.Duration `yaml:"heartbeat_interval"`
	TelemetryT1Interval time.Duration `yaml:"telemetry_t1_interval"`
	TelemetryT2Interval time.Duration `yaml:"telemetry_t2_interval"`
	TelemetryT3Interval time.Duration `yaml:"telemetry_t3_interval"`
	SnapshotInterval    time.Duration `yaml:"snapshot_interval"`
	CommandVerifyDelay  time.Duration `yaml:"command_verify_delay"`
	GenieACSNBIURL      string        `yaml:"genieacs_nbi_url"`
	FTPWatchDir         string        `yaml:"ftp_watch_dir"`
	RulesFile           string        `yaml:"rules_file"`
	BufferDB            string        `yaml:"buffer_db"`
	BufferMaxBytes      int64         `yaml:"buffer_max_bytes"`
	Enrollment          Enrollment    `yaml:"-"`
}

type rawConfig struct {
	EnrollmentToken     string `yaml:"enrollment_token"`
	PollInterval        string `yaml:"poll_interval"`
	HeartbeatInterval   string `yaml:"heartbeat_interval"`
	TelemetryT1Interval string `yaml:"telemetry_t1_interval"`
	TelemetryT2Interval string `yaml:"telemetry_t2_interval"`
	TelemetryT3Interval string `yaml:"telemetry_t3_interval"`
	SnapshotInterval    string `yaml:"snapshot_interval"`
	CommandVerifyDelay  string `yaml:"command_verify_delay"`
	GenieACSNBIURL      string `yaml:"genieacs_nbi_url"`
	FTPWatchDir         string `yaml:"ftp_watch_dir"`
	RulesFile           string `yaml:"rules_file"`
	BufferDB            string `yaml:"buffer_db"`
	BufferMaxBytes      int64  `yaml:"buffer_max_bytes"`
}

func Load(path string) (*Config, error) {
	cfg := defaultConfig()
	if b, err := os.ReadFile(path); err == nil {
		var raw rawConfig
		if err := yaml.Unmarshal(b, &raw); err != nil {
			return nil, err
		}
		applyRaw(cfg, raw)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	applyEnv(cfg)
	enrollment, err := DecodeEnrollmentToken(cfg.EnrollmentToken)
	if err != nil {
		return nil, err
	}
	cfg.Enrollment = enrollment
	return cfg, nil
}

func defaultConfig() *Config {
	return &Config{
		PollInterval:        10 * time.Second,
		HeartbeatInterval:   30 * time.Second,
		TelemetryT1Interval: 30 * time.Second,
		TelemetryT2Interval: time.Minute,
		TelemetryT3Interval: 5 * time.Minute,
		SnapshotInterval:    5 * time.Minute,
		CommandVerifyDelay:  2 * time.Second,
		GenieACSNBIURL:      "http://127.0.0.1:7557",
		FTPWatchDir:         "/srv/nybsys-ftp/nybsysftp/uploads",
		RulesFile:           "/etc/cloudlynet-agent/rules.yaml",
		BufferDB:            "/var/lib/cloudlynet-agent/buffer.sqlite",
		BufferMaxBytes:      104857600,
	}
}

func applyRaw(cfg *Config, raw rawConfig) {
	if raw.EnrollmentToken != "" {
		cfg.EnrollmentToken = raw.EnrollmentToken
	}
	if raw.GenieACSNBIURL != "" {
		cfg.GenieACSNBIURL = raw.GenieACSNBIURL
	}
	if raw.FTPWatchDir != "" {
		cfg.FTPWatchDir = raw.FTPWatchDir
	}
	if raw.RulesFile != "" {
		cfg.RulesFile = raw.RulesFile
	}
	if raw.BufferDB != "" {
		cfg.BufferDB = raw.BufferDB
	}
	if raw.BufferMaxBytes > 0 {
		cfg.BufferMaxBytes = raw.BufferMaxBytes
	}
	setDuration := func(v string, dst *time.Duration) {
		if v == "" {
			return
		}
		if d, err := time.ParseDuration(v); err == nil {
			*dst = d
		}
	}
	setDuration(raw.PollInterval, &cfg.PollInterval)
	setDuration(raw.HeartbeatInterval, &cfg.HeartbeatInterval)
	setDuration(raw.TelemetryT1Interval, &cfg.TelemetryT1Interval)
	setDuration(raw.TelemetryT2Interval, &cfg.TelemetryT2Interval)
	setDuration(raw.TelemetryT3Interval, &cfg.TelemetryT3Interval)
	setDuration(raw.SnapshotInterval, &cfg.SnapshotInterval)
	setDuration(raw.CommandVerifyDelay, &cfg.CommandVerifyDelay)
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("CLOUDLYNET_ENROLLMENT_TOKEN"); strings.TrimSpace(v) != "" {
		cfg.EnrollmentToken = strings.TrimSpace(v)
	}
	if v := os.Getenv("CLOUDLYNET_BASE_URL"); strings.TrimSpace(v) != "" {
		decoded, err := DecodeEnrollmentToken(cfg.EnrollmentToken)
		if err == nil {
			decoded.BaseURL = strings.TrimRight(strings.TrimSpace(v), "/")
			b, _ := json.Marshal(decoded)
			cfg.EnrollmentToken = base64.RawURLEncoding.EncodeToString(b)
		}
	}
	if v := os.Getenv("GENIEACS_NBI_URL"); strings.TrimSpace(v) != "" {
		cfg.GenieACSNBIURL = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if v := os.Getenv("FTP_WATCH_DIR"); strings.TrimSpace(v) != "" {
		cfg.FTPWatchDir = strings.TrimSpace(v)
	}
	if v := os.Getenv("BUFFER_DB"); strings.TrimSpace(v) != "" {
		cfg.BufferDB = strings.TrimSpace(v)
	}
}

func DecodeEnrollmentToken(token string) (Enrollment, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Enrollment{}, fmt.Errorf("enrollment_token is required")
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(token)
	}
	if err != nil {
		return Enrollment{}, fmt.Errorf("decode enrollment token: %w", err)
	}
	var e Enrollment
	if err := json.Unmarshal(b, &e); err != nil {
		return Enrollment{}, fmt.Errorf("parse enrollment token: %w", err)
	}
	if e.TenantID == "" || e.EdgeID == "" || e.BaseURL == "" || e.APIKey == "" {
		return Enrollment{}, fmt.Errorf("enrollment token missing tenant_id, edge_id, base_url, or api_key")
	}
	e.BaseURL = strings.TrimRight(e.BaseURL, "/")
	return e, nil
}
