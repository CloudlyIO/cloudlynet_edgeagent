package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		base:   strings.TrimRight(baseURL, "/"),
		apiKey: apiKey,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

type InventoryItem struct {
	GenieACSID   string `json:"genieacs_id"`
	SerialNumber string `json:"serial_number,omitempty"`
	ProductClass string `json:"product_class,omitempty"`
	AdminLANIP   string `json:"admin_lan_ip,omitempty"`
	WANIP        string `json:"wan_ip,omitempty"`
	SWVersion    string `json:"sw_version,omitempty"`
	RFTxStatus   *bool  `json:"rf_tx_status,omitempty"`
	OpState      *bool  `json:"op_state,omitempty"`
	LastInformAt string `json:"last_inform_at,omitempty"`
}

type RegisterRequest struct {
	AgentVersion string         `json:"agent_version"`
	Meta         map[string]any `json:"meta,omitempty"`
}

type HeartbeatRequest struct {
	Devices []InventoryItem `json:"devices"`
}

type MetricSample struct {
	GenieACSID string         `json:"genieacs_id"`
	Timestamp  string         `json:"ts"`
	Tier       int            `json:"tier"`
	Metrics    map[string]any `json:"metrics"`
}

type EventItem struct {
	GenieACSID string         `json:"genieacs_id"`
	Timestamp  string         `json:"ts"`
	Module     string         `json:"module"`
	EventType  string         `json:"event_type"`
	Severity   string         `json:"severity"`
	Message    string         `json:"message,omitempty"`
	Attrs      map[string]any `json:"attrs,omitempty"`
	DedupKey   string         `json:"dedup_key,omitempty"`
}

type AlarmItem struct {
	GenieACSID string `json:"genieacs_id"`
	Timestamp  string `json:"ts"`
	Severity   string `json:"severity"`
	AlarmType  string `json:"alarm_type,omitempty"`
	Message    string `json:"message,omitempty"`
	Active     bool   `json:"active"`
}

type TelemetryRequest struct {
	Metrics []MetricSample `json:"metrics,omitempty"`
	Events  []EventItem    `json:"events,omitempty"`
	Alarms  []AlarmItem    `json:"alarms,omitempty"`
}

type SnapshotRequest struct {
	Params map[string]any `json:"params"`
	Source string         `json:"source"`
}

type Write struct {
	Path    string `json:"path"`
	Value   any    `json:"value"`
	XSDType string `json:"xsd_type,omitempty"`
}

type CommandPayload struct {
	Writes         []Write        `json:"writes,omitempty"`
	Verify         map[string]any `json:"verify,omitempty"`
	RollbackOnFail bool           `json:"rollback_on_fail,omitempty"`
	ReadPaths      []string       `json:"read_paths,omitempty"`
}

type Command struct {
	ID         string         `json:"id"`
	DeviceID   string         `json:"device_id"`
	GenieACSID string         `json:"genieacs_id"`
	Type       string         `json:"type"`
	Payload    CommandPayload `json:"payload"`
}

type PollData struct {
	ServerTime string    `json:"server_time"`
	Commands   []Command `json:"commands"`
}

type AckResult struct {
	Readback map[string]any   `json:"readback,omitempty"`
	Mismatch []map[string]any `json:"mismatch,omitempty"`
	TaskID   string           `json:"task_id,omitempty"`
	Detail   string           `json:"detail,omitempty"`
}

type AckRequest struct {
	Status string    `json:"status"`
	Result AckResult `json:"result,omitempty"`
}

func (c *Client) Register(ctx context.Context, req RegisterRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/agent/register", req, nil)
}

func (c *Client) Poll(ctx context.Context) (PollData, error) {
	var out PollData
	err := c.do(ctx, http.MethodGet, "/v1/agent/poll", nil, &out)
	return out, err
}

func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/agent/heartbeat", req, nil)
}

func (c *Client) SendTelemetry(ctx context.Context, req TelemetryRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/agent/telemetry", req, nil)
}

func (c *Client) SendTelemetryRaw(ctx context.Context, body []byte) error {
	return c.doRaw(ctx, http.MethodPost, "/v1/agent/telemetry", body, nil)
}

func (c *Client) SendSnapshot(ctx context.Context, genieacsID string, req SnapshotRequest) error {
	// GenieACS device IDs may contain literal percent-escaped bytes (for example
	// "%2D"). They are values, not pre-escaped URL path segments. Escape the
	// entire value so the HTTP server decodes it once and receives the exact ID
	// that heartbeat/telemetry persist in nanolink_devices.
	return c.do(ctx, http.MethodPost, "/v1/agent/devices/"+url.PathEscape(genieacsID)+"/config-snapshot", req, nil)
}

func (c *Client) Ack(ctx context.Context, commandID string, req AckRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/agent/commands/"+commandID+"/ack", req, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in any, out any) error {
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	return c.doRaw(ctx, method, path, body, out)
}

func (c *Client) doRaw(ctx context.Context, method, path string, body []byte, out any) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.url(path), reader)
		if err != nil {
			return err
		}
		req.Header.Set("X-Edge-Key", c.apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err == nil && resp.StatusCode < 500 {
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				return fmt.Errorf("%s %s -> %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
			}
			if out == nil {
				io.Copy(io.Discard, resp.Body)
				return nil
			}
			return decodeEnvelope(resp.Body, out)
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("%s %s -> %d", method, path, resp.StatusCode)
		}
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
	return fmt.Errorf("cloud unreachable: %v", last)
}

func (c *Client) url(path string) string {
	if strings.HasSuffix(c.base, "/v1") && strings.HasPrefix(path, "/v1/") {
		return c.base + strings.TrimPrefix(path, "/v1")
	}
	return c.base + path
}

func decodeEnvelope(r io.Reader, out any) error {
	var env struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return err
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return err
	}
	return nil
}
