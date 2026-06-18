package collector

import "testing"

func TestBuildMetricsTier1(t *testing.T) {
	raw := map[string]any{
		fap + "FAPControl.LTE.OpState":      true,
		fap + "X_8C1F64_Status.UeNumber":    "7",
		"Device.Services.FAPService.1.Junk": "ignored",
	}
	m := buildMetrics(1, raw)
	if m["op_state"] != true {
		t.Fatalf("op_state = %v", m["op_state"])
	}
	if m["connected_ues"] != "7" {
		t.Fatalf("connected_ues = %v", m["connected_ues"])
	}
	if _, ok := m["rs_power"]; ok {
		t.Fatalf("tier-1 metrics leaked a tier-2 key: %v", m)
	}
}

func TestBuildMetricsTier3Derived(t *testing.T) {
	raw := map[string]any{
		pathRRCAtt:       "200",
		pathRRCSucc:      "190",
		pmValuePath(412): "12.5",
	}
	m := buildMetrics(3, raw)
	if pct, ok := m["rrc_success_pct"].(float64); !ok || pct < 94.9 || pct > 95.1 {
		t.Fatalf("rrc_success_pct = %v (ok=%v)", m["rrc_success_pct"], ok)
	}
	if m["sinr_avg_db"] != "12.5" {
		t.Fatalf("sinr_avg_db = %v", m["sinr_avg_db"])
	}
}

func TestRRCSuccessPctGuards(t *testing.T) {
	if _, ok := rrcSuccessPct(map[string]any{pathRRCAtt: "0", pathRRCSucc: "5"}); ok {
		t.Fatal("division by zero attempts should not produce a value")
	}
	if _, ok := rrcSuccessPct(map[string]any{pathRRCSucc: "5"}); ok {
		t.Fatal("missing attempts counter should not produce a value")
	}
}

func TestBuildAlarms(t *testing.T) {
	raw := map[string]any{
		"Device.FaultMgmt.CurrentAlarmNumberOfEntries":      "1",
		"Device.FaultMgmt.CurrentAlarm.1.EventType":         "linkDown",
		"Device.FaultMgmt.CurrentAlarm.1.PerceivedSeverity": "Major",
		"Device.FaultMgmt.CurrentAlarm.1.SpecificProblem":   "S1 link down",
	}
	alarms := buildAlarms("NANO-1", "2026-06-17T00:00:00Z", raw)
	if len(alarms) != 1 {
		t.Fatalf("alarms = %d", len(alarms))
	}
	if alarms[0].Severity != "major" || alarms[0].AlarmType != "linkDown" || !alarms[0].Active {
		t.Fatalf("unexpected alarm: %+v", alarms[0])
	}
}
