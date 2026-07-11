package cwmp

import "time"

// TaskType is a queued ACS→CPE operation the session applies on the device's
// next turn.
type TaskType string

const (
	TaskGPV    TaskType = "gpv"
	TaskSPV    TaskType = "spv"
	TaskGPN    TaskType = "gpn"
	TaskReboot TaskType = "reboot"
)

// Task is one unit of work queued for a device session.
type Task struct {
	Type      TaskType
	Paths     []string               // gpv / gpn
	Writes    []ParameterValueStruct // spv
	CmdKey    string                 // ParameterKey / CommandKey echoed to the device
	CommandID string                 // cloud command id, echoed back in the result
}

// TaskResult carries the outcome of a Task back to the worker via Await.
type TaskResult struct {
	CommandID string
	Type      TaskType
	Params    map[string]string     // gpv
	Names     []ParameterInfoStruct // gpn (with writable bits)
	Status    int                   // spv (0=applied, 1=applied-after-reboot)
	Err       string
}

// DeviceRecord is the persisted identity of a CWMP device (from the Inform).
// Status/liveness fields (RFTxStatus, OpState, IPs) are NOT stored here — the
// collector enriches those from the parameter cache when building inventory.
type DeviceRecord struct {
	DeviceID     string // canonical id (CanonicalID)
	IP           string // source IP of the CWMP session
	Manufacturer string
	ProductClass string
	SerialNumber string
	SWVersion    string
	LastInformAt time.Time
}

// StoredEvent is a device event (e.g. ATC) buffered for the telemetry batch.
type StoredEvent struct {
	DeviceID  string
	Module    string
	EventType string
	Severity  string
	Message   string
	TS        time.Time
}

// Store is the persistence the CWMP server/session depend on. The agent's
// buffer.Buffer satisfies it (extends the existing SQLite DB), so the CWMP
// state lives in one file with the outbox/applied tables. Defined here
// (dependency inversion): cwmp owns the abstraction, buffer the implementation.
type Store interface {
	UpsertDevice(d DeviceRecord)
	CacheParam(deviceID, path, value string)
	SaveWritability(deviceID string, names []ParameterInfoStruct)
	HasWritabilityMap(deviceID string) bool
	GetParams(deviceID string, paths []string) map[string]string
	ListDevices() []DeviceRecord
	DeviceIP(deviceID string) string
	EmitEvent(deviceID, module, eventType, severity, message string)
	DrainEvents() []StoredEvent
}
