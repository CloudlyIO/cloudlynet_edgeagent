package collector

import (
	"fmt"
	"strconv"

	"cloudlynet_edgeagent/goagent/internal/cloud"
)

// Tiered telemetry metric catalogue — handover §3.4 (FINALISED 2026-06-10 against the live
// NanoLink dump). Each tier is read from GenieACS at its own cadence and POSTed with its
// canonical metric key. The cloud's MetricSample.metrics is open (additionalProperties), so
// keys not present on a given device are simply omitted.
//
// PM-derived T3 counters (prb_dl_pct, rrc_success_pct, sinr_avg_db, …) live under the device's
// PeriodicStatistics SampleSet. The Parameter indexes below are pinned to dmcli.new.conf from
// the NanoLink device dump used for this integration; if a future firmware changes the SampleSet
// ordering, update these constants in one place.

const (
	fap       = "Device.Services.FAPService.1."
	sampleSet = "Device.PeriodicStatistics.SampleSet.1."
)

type metricDef struct {
	Key  string
	Path string
}

// tier1Metrics — critical, live, instantaneous (30 s).
var tier1Metrics = []metricDef{
	{"op_state", fap + "FAPControl.LTE.OpState"},
	{"rf_tx_status", fap + "FAPControl.LTE.RFTxStatus"},
	{"admin_state", fap + "FAPControl.LTE.AdminState"},
	{"s1_status", fap + "FAPControl.LTE.Gateway.X_8C1F64_S1Status"},
	{"sctp_status", fap + "Transport.SCTP.Assoc.1.Status"},
	{"connected_ues", fap + "X_8C1F64_Status.UeNumber"},
	{"volte_ues", fap + "X_8C1F64_Status.VolteUeNumber"},
}

// tier2Metrics — RF/coverage, live SON/RF GPV (60 s).
var tier2Metrics = []metricDef{
	{"rip_average", fap + "X_8C1F64_SON.RIP.RIPAverage"},
	{"rip_prb", fap + "X_8C1F64_SON.RIP.RIPPRB"},
	{"rip_threshold", fap + "X_8C1F64_SON.RIP.RIPThreshold"},
	{"earfcn_dl_inuse", fap + "CellConfig.LTE.RAN.RF.X_8C1F64_EARFCNDLInUse"},
	{"pci_inuse", fap + "CellConfig.LTE.RAN.RF.X_8C1F64_PhyCellIDInUse"},
	{"rs_power", fap + "CellConfig.LTE.RAN.RF.ReferenceSignalPower"},
	{"dl_bw", fap + "CellConfig.LTE.RAN.RF.DLBandwidth"},
	{"ul_bw", fap + "CellConfig.LTE.RAN.RF.ULBandwidth"},
}

// tier3Metrics — PM counters (900 s granularity) + live hardware (5 min).
var tier3Metrics = []metricDef{
	{"prb_dl_pct", pmValuePath(316)},   // RRU.TotalPrbUsageMeanDl
	{"prb_ul_pct", pmValuePath(315)},   // RRU.TotalPrbUsageMeanUl
	{"sinr_avg_db", pmValuePath(412)},  // RRU.Sinr.Average
	{"rrc_conn_mean", pmValuePath(10)}, // RRC.ConnMean
	{"thp_dl", pmValuePath(118)},       // MAC.ThroughputDl
	{"thp_ul", pmValuePath(119)},       // MAC.ThroughputUl
	{"uptime", "Device.DeviceInfo.UpTime"},
	{"mem_free", "Device.DeviceInfo.MemoryStatus.Free"},
	{"mem_total", "Device.DeviceInfo.MemoryStatus.Total"},
	{"cpu_usage", "Device.DeviceInfo.ProcessStatus.CPUUsage"},
}

// rrc_success_pct = SuccConnEstab / AttConnEstab · 100 — derived, T3.
const (
	pathRRCAtt  = sampleSet + "Parameter.168.X_8C1F64_CurrentValue" // RRC.AttConnEstab
	pathRRCSucc = sampleSet + "Parameter.170.X_8C1F64_CurrentValue" // RRC.SuccConnEstab
)

// maxAlarmRows bounds the FaultMgmt.CurrentAlarm table scan (T3).
const maxAlarmRows = 16

func tierDefs(tier int) []metricDef {
	switch tier {
	case 1:
		return tier1Metrics
	case 2:
		return tier2Metrics
	case 3:
		return tier3Metrics
	default:
		return nil
	}
}

func pmValuePath(parameterIndex int) string {
	return fmt.Sprintf("%sParameter.%d.X_8C1F64_CurrentValue", sampleSet, parameterIndex)
}

// tierReadPaths returns the GenieACS paths to GPV for a tier (includes derived-counter inputs).
func tierReadPaths(tier int) []string {
	defs := tierDefs(tier)
	paths := make([]string, 0, len(defs)+2)
	for _, d := range defs {
		paths = append(paths, d.Path)
	}
	if tier == 3 {
		paths = append(paths, pathRRCAtt, pathRRCSucc)
	}
	return paths
}

// buildMetrics maps a raw GPV result to canonical metric keys for the tier.
func buildMetrics(tier int, raw map[string]any) map[string]any {
	metrics := map[string]any{}
	for _, d := range tierDefs(tier) {
		if v, ok := raw[d.Path]; ok {
			metrics[d.Key] = v
		}
	}
	if tier == 3 {
		if pct, ok := rrcSuccessPct(raw); ok {
			metrics["rrc_success_pct"] = pct
		}
	}
	return metrics
}

func rrcSuccessPct(raw map[string]any) (float64, bool) {
	att, ok1 := toFloat(raw[pathRRCAtt])
	succ, ok2 := toFloat(raw[pathRRCSucc])
	if !ok1 || !ok2 || att <= 0 {
		return 0, false
	}
	return succ / att * 100, true
}

// alarmReadPaths returns the bounded set of FaultMgmt.CurrentAlarm paths to GPV (T3).
func alarmReadPaths() []string {
	paths := make([]string, 0, maxAlarmRows*4+1)
	paths = append(paths, "Device.FaultMgmt.CurrentAlarmNumberOfEntries")
	for i := 1; i <= maxAlarmRows; i++ {
		base := fmt.Sprintf("Device.FaultMgmt.CurrentAlarm.%d.", i)
		paths = append(paths, base+"EventType", base+"PerceivedSeverity", base+"EventTime", base+"SpecificProblem")
	}
	return paths
}

// buildAlarms folds a raw GPV result over the FaultMgmt.CurrentAlarm table into alarm items.
func buildAlarms(genieacsID, ts string, raw map[string]any) []cloud.AlarmItem {
	var out []cloud.AlarmItem
	limit := maxAlarmRows
	if count, ok := toFloat(raw["Device.FaultMgmt.CurrentAlarmNumberOfEntries"]); ok && count >= 0 && int(count) < limit {
		limit = int(count)
	}
	for i := 1; i <= limit; i++ {
		base := fmt.Sprintf("Device.FaultMgmt.CurrentAlarm.%d.", i)
		etype, ok := raw[base+"EventType"]
		if !ok || fmt.Sprint(etype) == "" {
			continue
		}
		item := cloud.AlarmItem{
			GenieACSID: genieacsID,
			Timestamp:  alarmTime(raw[base+"EventTime"], ts),
			Severity:   normalizeSeverity(raw[base+"PerceivedSeverity"]),
			AlarmType:  fmt.Sprint(etype),
			Active:     true,
		}
		if msg, ok := raw[base+"SpecificProblem"]; ok {
			item.Message = fmt.Sprint(msg)
		}
		out = append(out, item)
	}
	return out
}

func alarmTime(v any, fallback string) string {
	if v != nil {
		if s := fmt.Sprint(v); s != "" {
			return s
		}
	}
	return fallback
}

// normalizeSeverity coerces TR-069 FaultMgmt severity strings to the cloud's enum.
func normalizeSeverity(v any) string {
	switch s := fmt.Sprint(v); s {
	case "Critical", "critical":
		return "critical"
	case "Major", "major":
		return "major"
	case "Minor", "minor":
		return "minor"
	case "Warning", "warning":
		return "warning"
	case "Cleared", "cleared", "clear":
		return "clear"
	default:
		return "warning"
	}
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	default:
		return 0, false
	}
}
