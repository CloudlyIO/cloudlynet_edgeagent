package cwmp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTriggerConnectionRequestDigestHandshake(t *testing.T) {
	var sawAuth bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="NanoLink", nonce="abc123", qop="auth"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sawAuth = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if err := TriggerConnectionRequest("192.168.8.248", "8C1F64-ENB%2DN03002%2DB3-2205609999", "secret", ts.URL); err != nil {
		t.Fatalf("digest CR failed: %v", err)
	}
	if !sawAuth {
		t.Error("second request should carry a Digest Authorization header")
	}
}

func TestTriggerConnectionRequestImmediate200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if err := TriggerConnectionRequest("192.168.8.248", "u", "p", ts.URL); err != nil {
		t.Errorf("200 CR should succeed: %v", err)
	}
}

func TestTriggerConnectionRequestUnreachable(t *testing.T) {
	// A just-closed httptest server yields an immediate connection-refused
	// (fast, unlike a blackhole address that would burn the 8s client timeout).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := ts.URL
	ts.Close()
	if err := TriggerConnectionRequest("127.0.0.1", "u", "p", closedURL); err == nil {
		t.Error("expected an error for an unreachable CR endpoint")
	}
}

func TestParseDigest(t *testing.T) {
	m := parseDigest(`Digest realm="NanoLink", nonce="abc123", qop="auth"`)
	if m["realm"] != "NanoLink" || m["nonce"] != "abc123" || m["qop"] != "auth" {
		t.Errorf("parseDigest = %+v", m)
	}
}
