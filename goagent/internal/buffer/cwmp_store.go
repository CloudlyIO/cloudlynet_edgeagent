package buffer

import (
	"database/sql"
	"strings"
	"time"

	"cloudlynet_edgeagent/goagent/internal/cwmp"
)

// This file makes *Buffer satisfy cwmp.Store — the CWMP server/session persist
// device identity, the parameter cache, writability bits, and events in the
// same SQLite DB (single connection) as the outbox/applied tables. Methods take
// no context to match the cwmp.Store interface; they run on the local DB and
// use the safe "read fully, close, then mutate" pattern the single-conn DB
// requires (see Drain in sqlite.go).

var _ cwmp.Store = (*Buffer)(nil)

func (b *Buffer) UpsertDevice(d cwmp.DeviceRecord) {
	// writability_loaded is intentionally NOT in the update set — a re-Inform
	// must not clear a device's learned writability map.
	_, _ = b.db.Exec(`
INSERT INTO cwmp_devices(device_id, ip, manufacturer, product_class, serial_number, sw_version, last_inform_at, writability_loaded)
VALUES (?, ?, ?, ?, ?, ?, ?, 0)
ON CONFLICT(device_id) DO UPDATE SET
  ip=excluded.ip, manufacturer=excluded.manufacturer, product_class=excluded.product_class,
  serial_number=excluded.serial_number, sw_version=excluded.sw_version, last_inform_at=excluded.last_inform_at`,
		d.DeviceID, d.IP, d.Manufacturer, d.ProductClass, d.SerialNumber, d.SWVersion, unixOrZero(d.LastInformAt))
}

func (b *Buffer) CacheParam(deviceID, path, value string) {
	_, _ = b.db.Exec(`
INSERT INTO cwmp_params(device_id, path, value, cached_at) VALUES (?, ?, ?, ?)
ON CONFLICT(device_id, path) DO UPDATE SET value=excluded.value, cached_at=excluded.cached_at`,
		deviceID, path, value, time.Now().Unix())
}

func (b *Buffer) SaveWritability(deviceID string, names []cwmp.ParameterInfoStruct) {
	tx, err := b.db.Begin()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, n := range names {
		if _, err := tx.Exec(`
INSERT INTO cwmp_params(device_id, path, writable, cached_at) VALUES (?, ?, ?, ?)
ON CONFLICT(device_id, path) DO UPDATE SET writable=excluded.writable`,
			deviceID, n.Name, boolToInt(n.Writable), now); err != nil {
			_ = tx.Rollback()
			return
		}
	}
	if _, err := tx.Exec(`UPDATE cwmp_devices SET writability_loaded=1 WHERE device_id=?`, deviceID); err != nil {
		_ = tx.Rollback()
		return
	}
	_ = tx.Commit()
}

func (b *Buffer) HasWritabilityMap(deviceID string) bool {
	var loaded int
	if err := b.db.QueryRow(`SELECT writability_loaded FROM cwmp_devices WHERE device_id=?`, deviceID).Scan(&loaded); err != nil {
		return false
	}
	return loaded == 1
}

func (b *Buffer) GetParams(deviceID string, paths []string) map[string]string {
	out := map[string]string{}
	if len(paths) == 0 {
		return out
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",")
	args := make([]any, 0, len(paths)+1)
	args = append(args, deviceID)
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := b.db.Query(`SELECT path, value FROM cwmp_params WHERE device_id=? AND path IN (`+placeholders+`) AND value IS NOT NULL`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var value sql.NullString
		if err := rows.Scan(&path, &value); err != nil {
			continue
		}
		if value.Valid {
			out[path] = value.String
		}
	}
	return out
}

func (b *Buffer) ListDevices() []cwmp.DeviceRecord {
	rows, err := b.db.Query(`SELECT device_id, ip, manufacturer, product_class, serial_number, sw_version, last_inform_at FROM cwmp_devices ORDER BY device_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []cwmp.DeviceRecord
	for rows.Next() {
		var d cwmp.DeviceRecord
		var ip, man, pc, sn, sw sql.NullString
		var last sql.NullInt64
		if err := rows.Scan(&d.DeviceID, &ip, &man, &pc, &sn, &sw, &last); err != nil {
			continue
		}
		d.IP, d.Manufacturer, d.ProductClass = ip.String, man.String, pc.String
		d.SerialNumber, d.SWVersion = sn.String, sw.String
		if last.Valid && last.Int64 > 0 {
			d.LastInformAt = time.Unix(last.Int64, 0).UTC()
		}
		out = append(out, d)
	}
	return out
}

func (b *Buffer) DeviceIP(deviceID string) string {
	var ip sql.NullString
	_ = b.db.QueryRow(`SELECT ip FROM cwmp_devices WHERE device_id=?`, deviceID).Scan(&ip)
	return ip.String
}

func (b *Buffer) EmitEvent(deviceID, module, eventType, severity, message string) {
	_, _ = b.db.Exec(`INSERT INTO cwmp_events(device_id, module, event_type, severity, message, ts, uploaded) VALUES (?, ?, ?, ?, ?, ?, 0)`,
		deviceID, module, eventType, severity, message, time.Now().Unix())
}

func (b *Buffer) DrainEvents() []cwmp.StoredEvent {
	rows, err := b.db.Query(`SELECT id, device_id, module, event_type, severity, message, ts FROM cwmp_events WHERE uploaded=0 ORDER BY id LIMIT 500`)
	if err != nil {
		return nil
	}
	type row struct {
		id int64
		ev cwmp.StoredEvent
	}
	var batch []row
	for rows.Next() {
		var r row
		var module, et, sev, msg sql.NullString
		var ts int64
		if err := rows.Scan(&r.id, &r.ev.DeviceID, &module, &et, &sev, &msg, &ts); err != nil {
			rows.Close()
			return nil
		}
		r.ev.Module, r.ev.EventType, r.ev.Severity, r.ev.Message = module.String, et.String, sev.String, msg.String
		r.ev.TS = time.Unix(ts, 0).UTC()
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil
	}
	rows.Close()
	out := make([]cwmp.StoredEvent, 0, len(batch))
	for _, r := range batch {
		if _, err := b.db.Exec(`UPDATE cwmp_events SET uploaded=1 WHERE id=?`, r.id); err != nil {
			continue
		}
		out = append(out, r.ev)
	}
	return out
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
