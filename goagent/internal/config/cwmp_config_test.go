package config

import "testing"

func TestCWMPDefaults(t *testing.T) {
	cfg := defaultConfig()
	if cfg.CWMP.Listen != "0.0.0.0:7547" {
		t.Errorf("cwmp.listen default = %q, want 0.0.0.0:7547", cfg.CWMP.Listen)
	}
	if !cfg.CWMP.FullSnapshotOnFirstContact {
		t.Error("full_snapshot_on_first_contact should default to true")
	}
}

func TestApplyEnvCWMP(t *testing.T) {
	t.Setenv("CWMP_LISTEN", "0.0.0.0:9999")
	t.Setenv("CWMP_CR_USER", "8C1F64-ENB%2DN03002%2DB3-2205609999")
	t.Setenv("CWMP_CR_PASS", "secret")
	t.Setenv("CWMP_CR_URL_OVERRIDE", "http://192.168.8.248:30005/")
	cfg := defaultConfig()
	applyEnv(cfg)
	if cfg.CWMP.Listen != "0.0.0.0:9999" {
		t.Errorf("CWMP_LISTEN not applied: %q", cfg.CWMP.Listen)
	}
	if cfg.CWMP.CRUser != "8C1F64-ENB%2DN03002%2DB3-2205609999" || cfg.CWMP.CRPass != "secret" || cfg.CWMP.CRURLOverride != "http://192.168.8.248:30005/" {
		t.Errorf("CWMP CR env not applied: %+v", cfg.CWMP)
	}
}

func TestApplyRawCWMPFullSnapshotFalseOverridesDefault(t *testing.T) {
	f := false
	cfg := defaultConfig()
	applyRaw(cfg, rawConfig{CWMP: rawCWMP{Listen: "1.2.3.4:7547", FullSnapshotOnFirstContact: &f}})
	if cfg.CWMP.Listen != "1.2.3.4:7547" {
		t.Errorf("cwmp.listen = %q", cfg.CWMP.Listen)
	}
	if cfg.CWMP.FullSnapshotOnFirstContact {
		t.Error("explicit yaml false should override the default true")
	}
}

func TestApplyRawCWMPOmittedFullSnapshotKeepsDefault(t *testing.T) {
	cfg := defaultConfig()
	applyRaw(cfg, rawConfig{CWMP: rawCWMP{Listen: "1.2.3.4:7547"}}) // full-snapshot not set
	if !cfg.CWMP.FullSnapshotOnFirstContact {
		t.Error("omitted full_snapshot_on_first_contact should keep the default true")
	}
}
