package rules

import "testing"

// TestDedupKeyContentDerived locks in the fix: the dedup key must be derived
// from (device, eventType, raw) only — no parse-time component — so the same
// log line yields the same key across separate parses (ring vs Devicelog
// overlap, and post-restart re-parse), letting the cloud dedup it.
func TestDedupKeyContentDerived(t *testing.T) {
	e := DefaultEngine()
	line := "0000000217 2024-06-02 23:51:22.814 [FILE_TRANS] File upload success, curl code=(0)"
	first := e.Apply([]string{line}, "dev-1")
	second := e.Apply([]string{line}, "dev-1")
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("events = %d and %d, want 1 each", len(first), len(second))
	}
	if first[0].DedupKey != second[0].DedupKey {
		t.Errorf("dedup key not stable across parses: %q vs %q", first[0].DedupKey, second[0].DedupKey)
	}
	other := e.Apply([]string{"0000000218 2024-06-02 23:51:23.000 [FILE_TRANS] File upload success, curl code=(0)"}, "dev-1")
	if len(other) == 1 && other[0].DedupKey == first[0].DedupKey {
		t.Errorf("distinct raw lines must not share a dedup key")
	}
}

func TestDefaultRules(t *testing.T) {
	events := DefaultEngine().Apply([]string{"0000000031 2024-06-02 07:03:19.616 [TR69] RPC Unknown received from ACS"}, "dev-1")
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].EventType != "atc_fault_loop" || events[0].DedupKey == "" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
}

func TestModuleFromLine(t *testing.T) {
	cases := map[string]string{
		"0000000055 2024-06-02 11:25:28.933 [FILE_TRANS] File upload failure, curl code=(25)": "FILE_TRANS",
		"0000000030 2024-06-02 07:07:38.097 [TR69] Alarm Report, id: 0x18020500":               "TR69",
		"no tag here at all":                                                                   "",
	}
	for line, want := range cases {
		if got := moduleFromLine(line); got != want {
			t.Errorf("moduleFromLine(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestApplyRoutesModulePerLine locks in fix A: the module is derived from the
// inline "[MODULE]" tag on each line, not a filename/entry name passed in
// separately — real ring archives interleave every module in one file.
func TestApplyRoutesModulePerLine(t *testing.T) {
	lines := []string{
		"0000000057 2024-06-02 11:25:29.190 [SCM] Process(pid=1843) tr69c started",
		"0000000217 2024-06-02 23:51:22.814 [FILE_TRANS] File upload success, curl code=(0), command (...)",
		"0000000030 2024-06-02 07:07:38.097 [TR69] RPC Unknown received from ACS",
	}
	events := DefaultEngine().Apply(lines, "dev-1")
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (SCM line has no matching rule/alarm keyword): %+v", len(events), events)
	}
	byType := map[string]bool{}
	for _, e := range events {
		byType[e.EventType] = true
		if e.CWMPID != "dev-1" {
			t.Errorf("event device = %q, want dev-1", e.CWMPID)
		}
	}
	if !byType["ftp_upload_ok"] || !byType["atc_fault_loop"] {
		t.Fatalf("unexpected event types: %+v", byType)
	}
}

func TestCurlConnFailAndTimeoutRules(t *testing.T) {
	lines := []string{
		"0000000017 2024-06-02 11:29:41.293 [FILE_TRANS] File upload failure, curl code=(7), command (...)",
		"0000000116 2024-06-02 22:42:21.226 [FILE_TRANS] File upload failure, curl code=(28), command (...)",
	}
	events := DefaultEngine().Apply(lines, "dev-1")
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(events), events)
	}
	if events[0].EventType != "ftp_conn_fail" {
		t.Errorf("events[0].EventType = %q, want ftp_conn_fail", events[0].EventType)
	}
	if events[1].EventType != "ftp_upload_timeout" {
		t.Errorf("events[1].EventType = %q, want ftp_upload_timeout", events[1].EventType)
	}
}

// TestVendorACSUnreachableMatchesRealTR69Text locks in the E fix: the rule
// gates on TR69 module + real ACS-failure text, IP-agnostic (not the
// hardcoded 124.93.160.157 substring the old rule required).
func TestVendorACSUnreachableMatchesRealTR69Text(t *testing.T) {
	lines := []string{
		"0000080780 2024-05-31 06:25:51.298 [TR69] Alarm Report, id: 0x18020500 file: main/informer.c line: 1299 detail: ACS Connect Status = 2 Connection timed out to host 124.93.160.157:8080",
		"0000000032 2024-06-02 07:07:38.745 [TR69] Alarm Logged, id: 0x18020400 file: main/informer.c line: 175 detail: ACS connect failed, retryCount = 1, backOffTime = 6000ms",
		"0000000030 2024-06-02 07:07:38.097 [TR69] Alarm Report, id: 0x18020500 file: main/informer.c line: 307 detail: ACS Disconnect with error 1 (0:eOK, 1:eConnectError, 2:eGetError, 3:ePostError, 4:eAuthError)",
	}
	events := DefaultEngine().Apply(lines, "dev-1")
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(events), events)
	}
	for _, e := range events {
		if e.EventType != "vendor_acs_unreachable" {
			t.Errorf("EventType = %q, want vendor_acs_unreachable: %+v", e.EventType, e)
		}
	}
}

// TestUntaggedLineFallsBackToAlarmy: a line with no "[MODULE]" tag can never
// match a rule (no rule declares an empty Module) — it only ever hits the
// alarmy() generic fallback.
func TestUntaggedLineFallsBackToAlarmy(t *testing.T) {
	events := DefaultEngine().Apply([]string{"a plain line reporting a fault with no module tag"}, "dev-1")
	if len(events) != 1 || events[0].EventType != "unclassified" {
		t.Fatalf("events = %+v, want one unclassified event", events)
	}
}
