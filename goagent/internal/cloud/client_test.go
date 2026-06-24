package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendSnapshotEscapesLiteralPercentInGenieACSIdentifier(t *testing.T) {
	const genieacsID = "8C1F64-ENB%2DN03002%2DB3-2205600282"
	var escapedPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escapedPath = r.URL.EscapedPath()
		if got := r.URL.Path; got != "/v1/agent/devices/"+genieacsID+"/config-snapshot" {
			t.Fatalf("decoded request path = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"stored":true}}`))
	}))
	defer server.Close()

	client := New(server.URL, "test-edge-key")
	err := client.SendSnapshot(context.Background(), genieacsID, SnapshotRequest{
		Params: map[string]any{"Device.ManagementServer.PeriodicInformInterval": 300},
		Source: "agent",
	})
	if err != nil {
		t.Fatalf("SendSnapshot() error = %v", err)
	}

	want := "/v1/agent/devices/8C1F64-ENB%252DN03002%252DB3-2205600282/config-snapshot"
	if escapedPath != want {
		t.Fatalf("escaped request path = %q, want %q", escapedPath, want)
	}
}
