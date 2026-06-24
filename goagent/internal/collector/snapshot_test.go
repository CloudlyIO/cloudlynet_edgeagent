package collector

import "testing"

func TestSnapshotPathsCoverManagedParameterCatalogue(t *testing.T) {
	expected := map[string]struct{}{
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.PhyCellID":                              {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNDL":                               {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNUL":                               {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.FreqBandIndicator":                      {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.DLBandwidth":                            {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ULBandwidth":                            {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower":                   {},
		"Device.Services.FAPService.1.Capabilities.MaxTxPower":                                      {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.PHY.PDSCH.Pa":                              {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.PHY.PDSCH.Pb":                              {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.DRXEnabled":                        {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.OnDurationTimer":                   {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.DRXInactivityTimer":                {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.LongDRXCycle":                      {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.DRX.ShortDRXCycle":                     {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.MAC.X_8C1F64_PCH.DefaultPagingCycle":       {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.IdleMode.IntraFreq.QRxLevMinSIB1": {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.IdleMode.IntraFreq.SIntraSearch":  {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.A2ThresholdRSRP":   {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.A1ThresholdRSRP":   {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.Hysteresis":        {},
		"Device.Services.FAPService.1.CellConfig.LTE.RAN.Mobility.ConnMode.EUTRA.TimeToTrigger":     {},
		"Device.ManagementServer.PeriodicInformInterval":                                            {},
		"Device.X_8C1F64_DebugMgmt.Upload.AutonomousTransferCompletePolicy":                         {},
	}
	if len(SnapshotPaths) != len(expected) {
		t.Fatalf("SnapshotPaths count = %d, want %d", len(SnapshotPaths), len(expected))
	}
	seen := make(map[string]struct{}, len(SnapshotPaths))
	for _, path := range SnapshotPaths {
		if _, duplicate := seen[path]; duplicate {
			t.Fatalf("SnapshotPaths contains duplicate %q", path)
		}
		seen[path] = struct{}{}
		if _, ok := expected[path]; !ok {
			t.Fatalf("SnapshotPaths contains unmanaged path %q", path)
		}
	}
	for path := range expected {
		if _, ok := seen[path]; !ok {
			t.Fatalf("SnapshotPaths missing managed path %q", path)
		}
	}
}
