package collector

import (
	"context"
	"testing"

	"cloudlynet_edgeagent/goagent/internal/buffer"
	"cloudlynet_edgeagent/goagent/internal/cwmp"
)

// TestInventoryEnrichesLivenessFromCache locks the field mapping that moved from
// the old NBI document dig into collector.Inventory: RFTxStatus, OpState,
// AdminLANIP, and WANIP are populated from the CWMP parameter cache.
func TestInventoryEnrichesLivenessFromCache(t *testing.T) {
	buf, err := buffer.Open(":memory:", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer buf.Close()

	const id = "8C1F64-ENB%2DN03002%2DB3-2205600282"
	buf.UpsertDevice(cwmp.DeviceRecord{
		DeviceID: id, IP: "192.168.8.100", SerialNumber: "2205600282",
		ProductClass: "ENB-N03002-B3", SWVersion: "V1.7.1",
	})
	buf.CacheParam(id, pathRFTxStatus, "1")
	buf.CacheParam(id, pathOpState, "0")
	buf.CacheParam(id, pathLANIP, "192.168.8.248")
	buf.CacheParam(id, pathWANIP, "10.0.0.10")

	c := New(cwmp.NewServer("0.0.0.0:7547", buf), nil, "")
	items, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if it.CWMPID != id || it.SerialNumber != "2205600282" || it.SWVersion != "V1.7.1" {
		t.Errorf("identity fields = %+v", it)
	}
	if it.AdminLANIP != "192.168.8.248" || it.WANIP != "10.0.0.10" {
		t.Errorf("IP fields = admin:%q wan:%q", it.AdminLANIP, it.WANIP)
	}
	if it.RFTxStatus == nil || !*it.RFTxStatus {
		t.Errorf("RFTxStatus = %v, want true", it.RFTxStatus)
	}
	if it.OpState == nil || *it.OpState {
		t.Errorf("OpState = %v, want false", it.OpState)
	}
}
