package buffer

import (
	"testing"
	"time"

	"cloudlynet_edgeagent/goagent/internal/cwmp"
)

func openMem(t *testing.T) *Buffer {
	t.Helper()
	b, err := Open(":memory:", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestCWMPUpsertAndListDevices(t *testing.T) {
	b := openMem(t)
	now := time.Now().UTC().Truncate(time.Second)
	b.UpsertDevice(cwmp.DeviceRecord{
		DeviceID: "8C1F64-ENB%2DN03002%2DB3-2205600282", IP: "192.168.8.100",
		Manufacturer: "NybSys", ProductClass: "ENB-N03002-B3", SerialNumber: "2205600282",
		SWVersion: "V1.7.1", LastInformAt: now,
	})
	devices := b.ListDevices()
	if len(devices) != 1 {
		t.Fatalf("ListDevices len = %d, want 1", len(devices))
	}
	d := devices[0]
	if d.DeviceID != "8C1F64-ENB%2DN03002%2DB3-2205600282" || d.IP != "192.168.8.100" || d.SWVersion != "V1.7.1" {
		t.Errorf("device = %+v", d)
	}
	if !d.LastInformAt.Equal(now) {
		t.Errorf("LastInformAt = %v, want %v", d.LastInformAt, now)
	}
	if got := b.DeviceIP("8C1F64-ENB%2DN03002%2DB3-2205600282"); got != "192.168.8.100" {
		t.Errorf("DeviceIP = %q", got)
	}
	// Re-upsert must not create a duplicate row.
	b.UpsertDevice(cwmp.DeviceRecord{DeviceID: "8C1F64-ENB%2DN03002%2DB3-2205600282", IP: "192.168.8.101"})
	if devices := b.ListDevices(); len(devices) != 1 || devices[0].IP != "192.168.8.101" {
		t.Errorf("re-upsert devices = %+v", devices)
	}
}

func TestCWMPParamCacheAndGet(t *testing.T) {
	b := openMem(t)
	const id = "dev-1"
	b.CacheParam(id, "Device.A", "1")
	b.CacheParam(id, "Device.B", "-8")
	got := b.GetParams(id, []string{"Device.A", "Device.B", "Device.Missing"})
	if len(got) != 2 || got["Device.A"] != "1" || got["Device.B"] != "-8" {
		t.Errorf("GetParams = %+v", got)
	}
	// Re-cache updates the value in place.
	b.CacheParam(id, "Device.B", "-10")
	if got := b.GetParams(id, []string{"Device.B"}); got["Device.B"] != "-10" {
		t.Errorf("updated value = %q, want -10", got["Device.B"])
	}
	// Empty path list returns empty, no query.
	if got := b.GetParams(id, nil); len(got) != 0 {
		t.Errorf("empty paths = %+v", got)
	}
}

func TestCWMPWritability(t *testing.T) {
	b := openMem(t)
	const id = "dev-1"
	b.UpsertDevice(cwmp.DeviceRecord{DeviceID: id})
	if b.HasWritabilityMap(id) {
		t.Fatal("writability should be unset on a fresh device")
	}
	// A value cached before the writability walk must survive the walk.
	b.CacheParam(id, "Device.ManagementServer.PeriodicInformInterval", "300")
	b.SaveWritability(id, []cwmp.ParameterInfoStruct{
		{Name: "Device.ManagementServer.PeriodicInformInterval", Writable: true},
		{Name: "Device.DeviceInfo.SerialNumber", Writable: false},
	})
	if !b.HasWritabilityMap(id) {
		t.Error("writability should be loaded after SaveWritability")
	}
	if got := b.GetParams(id, []string{"Device.ManagementServer.PeriodicInformInterval"}); got["Device.ManagementServer.PeriodicInformInterval"] != "300" {
		t.Errorf("value clobbered by writability walk: %+v", got)
	}
}

func TestCWMPEventDrain(t *testing.T) {
	b := openMem(t)
	const id = "dev-1"
	b.EmitEvent(id, "TR69", "autonomous_transfer_complete", "info", "ATC DeviceLog (2048B) fault=0")
	b.EmitEvent(id, "TR69", "autonomous_transfer_complete", "major", "ATC failed fault=9002")
	events := b.DrainEvents()
	if len(events) != 2 {
		t.Fatalf("drained %d events, want 2", len(events))
	}
	if events[0].EventType != "autonomous_transfer_complete" || events[0].Severity != "info" {
		t.Errorf("event[0] = %+v", events[0])
	}
	if events[1].Severity != "major" {
		t.Errorf("event[1] severity = %q, want major", events[1].Severity)
	}
	// A second drain returns nothing (uploaded flag flipped).
	if again := b.DrainEvents(); len(again) != 0 {
		t.Errorf("second drain returned %d events, want 0", len(again))
	}
}
