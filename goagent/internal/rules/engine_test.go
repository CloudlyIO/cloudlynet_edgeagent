package rules

import "testing"

func TestDefaultRules(t *testing.T) {
	events := DefaultEngine().Apply("TR69", []string{"RPC Unknown received from ACS"}, "dev-1")
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].EventType != "atc_fault_loop" || events[0].DedupKey == "" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
}
