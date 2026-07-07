package cwmp

import "testing"

func TestCanonicalID(t *testing.T) {
	got := CanonicalID("8C1F64", "ENB-N03002-B3", "2205600282")
	want := "8C1F64-ENB%2DN03002%2DB3-2205600282"
	if got != want {
		t.Errorf("CanonicalID = %q, want %q", got, want)
	}
}

func TestCanonicalIDNoHyphensUnchanged(t *testing.T) {
	got := CanonicalID("8C1F64", "SIMPLE", "SN01")
	want := "8C1F64-SIMPLE-SN01"
	if got != want {
		t.Errorf("CanonicalID = %q, want %q", got, want)
	}
}
