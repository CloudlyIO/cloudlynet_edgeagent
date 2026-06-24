package worker

import "testing"

func TestVerifyExpectedReportsActualReadbackValue(t *testing.T) {
	path := "Device.ManagementServer.PeriodicInformInterval"
	mismatch := verifyExpected(map[string]any{path: 126}, map[string]any{path: "300"})
	if len(mismatch) != 1 {
		t.Fatalf("verifyExpected() mismatches = %d, want 1", len(mismatch))
	}
	if mismatch[0]["actual"] != "300" {
		t.Fatalf("verifyExpected() actual = %v, want 300", mismatch[0]["actual"])
	}
}
