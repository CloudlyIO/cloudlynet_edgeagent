package genieacs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetParamsMatchingWaitsForUpdatedCacheValue(t *testing.T) {
	const path = "Device.ManagementServer.PeriodicInformInterval"
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{"_id": "task-1"})
			return
		}
		value := "300"
		if reads.Add(1) > 1 {
			value = "126"
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"_id": path,
			path:  map[string]any{"_value": value},
		}})
	}))
	defer server.Close()

	got, err := New(server.URL).GetParamsMatching(
		context.Background(), "NANO-1", []string{path}, map[string]any{path: 126}, 2*time.Second,
	)
	if err != nil {
		t.Fatalf("GetParamsMatching() error = %v", err)
	}
	if got[path] != "126" {
		t.Fatalf("GetParamsMatching() value = %v, want 126", got[path])
	}
	if reads.Load() < 2 {
		t.Fatalf("GetParamsMatching() reads = %d, want at least 2", reads.Load())
	}
}
