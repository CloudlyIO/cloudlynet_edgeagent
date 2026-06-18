package genieacs

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

	"cloudlynet_edgeagent/goagent/internal/cloud"
)

const AutonomousTransferCompletePolicy = "Device.X_8C1F64_DebugMgmt.Upload.AutonomousTransferCompletePolicy"

type Client struct {
	base string
	http *http.Client
}

func New(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) Inventory(ctx context.Context) ([]cloud.InventoryItem, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/devices", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("genieacs inventory: %d", resp.StatusCode)
	}
	var docs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&docs); err != nil {
		return nil, err
	}
	out := make([]cloud.InventoryItem, 0, len(docs))
	for _, doc := range docs {
		id := firstString(doc, "_id", "genieacs_id")
		if id == "" {
			continue
		}
		item := cloud.InventoryItem{
			GenieACSID:   id,
			SerialNumber: digString(doc, "Device.DeviceInfo.SerialNumber"),
			ProductClass: digString(doc, "Device.DeviceInfo.ProductClass"),
			AdminLANIP:   digString(doc, "Device.LAN.IPAddress"),
			WANIP:        digString(doc, "Device.WAN.IPAddress"),
			SWVersion:    digString(doc, "Device.DeviceInfo.SoftwareVersion"),
			LastInformAt: firstString(doc, "_lastInform", "last_inform_at"),
		}
		if v, ok := digBool(doc, "Device.Services.FAPService.1.FAPControl.LTE.RFTxStatus"); ok {
			item.RFTxStatus = &v
		}
		if v, ok := digBool(doc, "Device.Services.FAPService.1.FAPControl.LTE.OpState"); ok {
			item.OpState = &v
		}
		out = append(out, item)
	}
	return out, nil
}

func (c *Client) SetParams(ctx context.Context, deviceID string, writes []cloud.Write) (string, error) {
	values := make([][]any, 0, len(writes))
	for _, w := range writes {
		xsd := w.XSDType
		if xsd == "" {
			xsd = "xsd:string"
		}
		values = append(values, []any{w.Path, w.Value, xsd})
	}
	payload, _ := json.Marshal(map[string]any{"name": "setParameterValues", "parameterValues": values})
	return c.postTask(ctx, deviceID, payload)
}

func (c *Client) GetParams(ctx context.Context, deviceID string, paths []string) (map[string]any, error) {
	refresh, _ := json.Marshal(map[string]any{"name": "getParameterValues", "parameterNames": paths})
	_, _ = c.postTask(ctx, deviceID, refresh)
	q := url.QueryEscape(fmt.Sprintf(`{"_id":%q}`, deviceID))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/devices/?query="+q, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("genieacs get params: %d", resp.StatusCode)
	}
	var docs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&docs); err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(docs) == 0 {
		return out, nil
	}
	for _, p := range paths {
		if v, ok := digValue(docs[0], p); ok {
			out[p] = v
		}
	}
	return out, nil
}

func (c *Client) Reboot(ctx context.Context, deviceID string) (string, error) {
	payload, _ := json.Marshal(map[string]any{"name": "reboot"})
	return c.postTask(ctx, deviceID, payload)
}

func (c *Client) EnsureBaseline(ctx context.Context, deviceID string) error {
	_, err := c.SetParams(ctx, deviceID, []cloud.Write{{
		Path:    AutonomousTransferCompletePolicy,
		Value:   "None",
		XSDType: "xsd:string",
	}})
	return err
}

func (c *Client) postTask(ctx context.Context, deviceID string, payload []byte) (string, error) {
	u := fmt.Sprintf("%s/devices/%s/tasks?connection_request", c.base, url.PathEscape(deviceID))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("genieacs task: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var task struct {
		ID string `json:"_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return "", err
	}
	return task.ID, nil
}

func firstString(doc map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := doc[k].(string); ok {
			return v
		}
	}
	return ""
}

func digString(doc map[string]any, path string) string {
	if v, ok := digValue(doc, path); ok {
		return fmt.Sprint(v)
	}
	return ""
}

func digBool(doc map[string]any, path string) (bool, bool) {
	v, ok := digValue(doc, path)
	if !ok {
		return false, false
	}
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		return t == "1" || strings.EqualFold(t, "true") || strings.EqualFold(t, "up"), true
	default:
		return false, false
	}
}

func digValue(doc map[string]any, path string) (any, bool) {
	if v, ok := doc[path]; ok {
		return unwrapValue(v), true
	}
	cur := any(doc)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return unwrapValue(cur), true
}

func unwrapValue(v any) any {
	if m, ok := v.(map[string]any); ok {
		if val, ok := m["_value"]; ok {
			return val
		}
	}
	return v
}
