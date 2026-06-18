package config

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestDecodeEnrollmentToken(t *testing.T) {
	payload := Enrollment{
		Version:  1,
		TenantID: "00000000-0000-0000-0000-000000000001",
		EdgeID:   "00000000-0000-0000-0000-000000000002",
		BaseURL:  "http://cloudlynet:8080/",
		APIKey:   "00000000-0000-0000-0000-000000000002.secret",
	}
	b, _ := json.Marshal(payload)
	got, err := DecodeEnrollmentToken(base64.RawURLEncoding.EncodeToString(b))
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "http://cloudlynet:8080" || got.APIKey != payload.APIKey {
		t.Fatalf("unexpected enrollment: %+v", got)
	}
}
