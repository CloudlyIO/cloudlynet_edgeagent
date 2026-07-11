package worker

import "testing"

// TestVerifyExpectedNormalizesBoolean locks the fix for false "failed" acks:
// a JSON boolean write is canonicalized by the CPE to "1"/"0" on read-back, and
// must still verify as a match.
func TestVerifyExpectedNormalizesBoolean(t *testing.T) {
	if m := verifyExpected(map[string]any{"p": true}, map[string]any{"p": "1"}); len(m) != 0 {
		t.Errorf("true vs \"1\" should match, got mismatch %v", m)
	}
	if m := verifyExpected(map[string]any{"p": false}, map[string]any{"p": "0"}); len(m) != 0 {
		t.Errorf("false vs \"0\" should match, got mismatch %v", m)
	}
	// A genuine value mismatch is still reported.
	if m := verifyExpected(map[string]any{"p": "-8"}, map[string]any{"p": "-10"}); len(m) != 1 {
		t.Errorf("-8 vs -10 should mismatch, got %v", m)
	}
	// A missing read-back path is still reported.
	if m := verifyExpected(map[string]any{"p": "-8"}, map[string]any{}); len(m) != 1 || !m[0]["missing"].(bool) {
		t.Errorf("missing path should mismatch with missing=true, got %v", m)
	}
}

func TestKeysOf(t *testing.T) {
	got := keysOf(map[string]any{"a": 1, "b": 2})
	if len(got) != 2 {
		t.Errorf("keysOf len = %d, want 2", len(got))
	}
}

// TestFormatValueNoExponent locks the fix for large numeric command values: a
// JSON number (float64) must render as a plain integer, not "1e+06".
func TestFormatValueNoExponent(t *testing.T) {
	cases := map[float64]string{
		1000000:    "1000000",
		4294967296: "4294967296",
		503:        "503",
		-8:         "-8",
		0.5:        "0.5",
	}
	for in, want := range cases {
		if got := formatValue(in); got != want {
			t.Errorf("formatValue(%v) = %q, want %q", in, got, want)
		}
	}
	// A large integer written as a JSON number verifies against the device string.
	if m := verifyExpected(map[string]any{"p": float64(1000000)}, map[string]any{"p": "1000000"}); len(m) != 0 {
		t.Errorf("1000000 should match device \"1000000\", got mismatch %v", m)
	}
}
