package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type state struct {
	mu             sync.Mutex
	registered     int
	heartbeats     int
	telemetry      int
	events         int
	failures       int
	snapshots      int
	snapshotParams map[string]any
	acks           map[string]string
	params         map[string]any
}

const managedSnapshotParamCount = 24

func main() {
	mode := strings.ToLower(strings.TrimSpace(env("TESTSUITE_MODE", "full")))
	st := &state{
		acks:           map[string]string{},
		snapshotParams: map[string]any{},
		params: map[string]any{
			"_id":                               "NANO-1",
			"Device.DeviceInfo.SerialNumber":    map[string]any{"_value": "SN-0001"},
			"Device.DeviceInfo.ProductClass":    map[string]any{"_value": "ENB-N03002-B3"},
			"Device.DeviceInfo.SoftwareVersion": map[string]any{"_value": "1.0.0"},
			"Device.LAN.IPAddress":              map[string]any{"_value": "192.168.8.248"},
			"Device.WAN.IPAddress":              map[string]any{"_value": "10.0.0.10"},
			"Device.Services.FAPService.1.FAPControl.LTE.RFTxStatus":                                    map[string]any{"_value": true},
			"Device.Services.FAPService.1.FAPControl.LTE.OpState":                                       map[string]any{"_value": true},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower":                   map[string]any{"_value": "-10"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.PhyCellID":                              map[string]any{"_value": "449"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNDL":                               map[string]any{"_value": "1850"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNUL":                               map[string]any{"_value": "19850"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.FreqBandIndicator":                      map[string]any{"_value": "3"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.DLBandwidth":                            map[string]any{"_value": "100"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ULBandwidth":                            map[string]any{"_value": "100"},
			"Device.Services.FAPService.1.Capabilities.MaxTxPower":                                      map[string]any{"_value": "21"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.PHY.PDSCH.Pa":                              map[string]any{"_value": "0"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.PHY.PDSCH.Pb":                              map[string]any{"_value": "0"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.DRXEnabled":                        map[string]any{"_value": "1"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.OnDurationTimer":                   map[string]any{"_value": "40"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.DRXInactivityTimer":                map[string]any{"_value": "1920"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.LongDRXCycle":                      map[string]any{"_value": "128"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.ShortDRXCycle":                     map[string]any{"_value": "128"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.X_8C1F64_PCH.DefaultPagingCycle":       map[string]any{"_value": "rf128"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.IdleMode.IntraFreq.QRxLevMinSIB1": map[string]any{"_value": "-62"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.IdleMode.IntraFreq.SIntraSearch":  map[string]any{"_value": "21"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.A2ThresholdRSRP":   map[string]any{"_value": "50"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.A1ThresholdRSRP":   map[string]any{"_value": "60"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.Hysteresis":        map[string]any{"_value": "2"},
			"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.TimeToTrigger":     map[string]any{"_value": "40"},
			"Device.ManagementServer.PeriodicInformInterval":                                            map[string]any{"_value": "300"},
			"Device.X_8C1F64_DebugMgmt.Upload.AutonomousTransferCompletePolicy":                         map[string]any{"_value": "Always"},
			"_lastInform": "2026-06-17T00:00:00Z",
		},
	}

	ftpDir := env("FTP_DIR", "/ftp")
	if err := os.MkdirAll(ftpDir, 0o755); err == nil {
		go func() {
			time.Sleep(3 * time.Second)
			if err := writeArchive(path.Join(ftpDir, "NANO-1_logs.tgz")); err != nil {
				log.Printf("fixture archive failed: %v", err)
			}
		}()
	}

	go func() {
		log.Printf("mock genieacs listening on :7557")
		log.Fatal(http.ListenAndServe(":7557", genieacsMux(st)))
	}()
	if mode == "acsftp" || mode == "acs" {
		log.Printf("mock acs+ftp health listening on :9000 (platform mock disabled)")
		log.Fatal(http.ListenAndServe(":9000", acsHealthMux(st, ftpDir, mode)))
	}
	log.Printf("mock cloud listening on :9000")
	log.Fatal(http.ListenAndServe(":9000", cloudMux(st)))
}

func cloudMux(st *state) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		ok := st.registered > 0 && st.heartbeats > 0 && st.telemetry > 0 && st.events > 0 && st.failures > 0 && st.snapshots > 0 && len(st.snapshotParams) == managedSnapshotParamCount && len(st.acks) >= 3
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              ok,
			"registered":      st.registered,
			"heartbeats":      st.heartbeats,
			"telemetry":       st.telemetry,
			"events":          st.events,
			"failures":        st.failures,
			"snapshots":       st.snapshots,
			"snapshot_params": len(st.snapshotParams),
			"acks":            st.acks,
		})
	})
	mux.HandleFunc("/v1/agent/register", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.registered++
		st.mu.Unlock()
		envelope(w, map[string]any{"edge_id": "11111111-1111-1111-1111-111111111111"})
	})
	mux.HandleFunc("/v1/agent/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.heartbeats++
		st.mu.Unlock()
		envelope(w, map[string]any{})
	})
	mux.HandleFunc("/v1/agent/telemetry", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		if st.failures == 0 {
			st.failures++
			st.mu.Unlock()
			http.Error(w, "forced telemetry failure", http.StatusServiceUnavailable)
			return
		}
		st.mu.Unlock()
		var body struct {
			Events []any `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.telemetry++
		st.events += len(body.Events)
		st.mu.Unlock()
		envelope(w, map[string]any{"metrics": 1, "events": 1})
	})
	mux.HandleFunc("/v1/agent/poll", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		commands := []map[string]any{}
		if _, ok := st.acks["00000000-0000-0000-0000-000000000101"]; !ok {
			commands = append(commands, configureCommand())
		}
		if _, ok := st.acks["00000000-0000-0000-0000-000000000102"]; !ok {
			commands = append(commands, queryCommand())
		}
		if _, ok := st.acks["00000000-0000-0000-0000-000000000103"]; !ok {
			commands = append(commands, rebootCommand())
		}
		envelope(w, map[string]any{"server_time": time.Now().UTC().Format(time.RFC3339), "commands": commands})
	})
	mux.HandleFunc("/v1/agent/devices/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config-snapshot") {
			var body struct {
				Params map[string]any `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			st.mu.Lock()
			st.snapshots++
			for path, value := range body.Params {
				st.snapshotParams[path] = value
			}
			st.mu.Unlock()
			envelope(w, map[string]any{})
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/v1/agent/commands/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/ack") {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/agent/commands/"), "/ack")
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.acks[id] = body.Status
		st.mu.Unlock()
		envelope(w, map[string]any{})
	})
	return mux
}

func acsHealthMux(st *state, ftpDir, mode string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		devices := 0
		if len(st.params) > 0 {
			devices = 1
		}
		st.mu.Unlock()
		archives, _ := filepath.Glob(path.Join(ftpDir, "*.tgz"))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            devices > 0,
			"mode":          mode,
			"platform_mock": false,
			"genieacs":      map[string]any{"listen": ":7557", "devices": devices},
			"ftp":           map[string]any{"dir": ftpDir, "archives": len(archives)},
		})
	})
	return mux
}

func genieacsMux(st *state) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/devices", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		writeJSON(w, http.StatusOK, []map[string]any{st.params})
	})
	mux.HandleFunc("/devices/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			st.mu.Lock()
			defer st.mu.Unlock()
			writeJSON(w, http.StatusOK, []map[string]any{st.params})
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/tasks") {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Name            string   `json:"name"`
			ParameterValues [][]any  `json:"parameterValues"`
			ParameterNames  []string `json:"parameterNames"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Name == "setParameterValues" {
			st.mu.Lock()
			for _, pv := range body.ParameterValues {
				if len(pv) >= 2 {
					st.params[fmt.Sprint(pv[0])] = map[string]any{"_value": pv[1]}
				}
			}
			st.mu.Unlock()
		}
		writeJSON(w, http.StatusOK, map[string]any{"_id": "task-" + strings.ReplaceAll(body.Name, " ", "-")})
	})
	return mux
}

func configureCommand() map[string]any {
	return map[string]any{
		"id": "00000000-0000-0000-0000-000000000101", "device_id": "00000000-0000-0000-0000-000000000201", "genieacs_id": "NANO-1", "type": "configure",
		"payload": map[string]any{"writes": []map[string]any{{"path": "Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower", "value": "-8", "xsd_type": "xsd:string"}}},
	}
}

func queryCommand() map[string]any {
	return map[string]any{
		"id": "00000000-0000-0000-0000-000000000102", "device_id": "00000000-0000-0000-0000-000000000201", "genieacs_id": "NANO-1", "type": "query",
		"payload": map[string]any{"read_paths": []string{"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower"}},
	}
}

func rebootCommand() map[string]any {
	return map[string]any{
		"id": "00000000-0000-0000-0000-000000000103", "device_id": "00000000-0000-0000-0000-000000000201", "genieacs_id": "NANO-1", "type": "reboot",
		"payload": map[string]any{},
	}
}

func envelope(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "timestamp": time.Now().UTC().Format(time.RFC3339), "data": data, "errors": []any{}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func writeArchive(target string) error {
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	files := map[string]string{
		"TR69.log":       "RPC Unknown received from ACS\n",
		"FILE_TRANS.log": "File upload success, curl code=(0)\n",
		"FM.log":         "device reboot observed\n",
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return err
		}
	}
	return nil
}
