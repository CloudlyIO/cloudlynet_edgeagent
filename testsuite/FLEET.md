# Fleet mode — N mock NanoLink femtocells in one process

The testsuite historically emulated exactly **one** mock device. Fleet mode extends the same
binary to run **N devices in a single process** — one CWMP session loop, one FTP upload loop,
one connection-request listener, and one param store *per device* — for multi-femtocell demos
(e.g. the 14-cell NanoLink estate demo).

Fleet mode is **conf-file-only**: it is switched on by a `devices:` list in the NanoLink conf
(default `conf/nanolink.conf`, path overridable via `NANOLINK_CONF`). There is no env-var
equivalent for the list. With **no** `devices:` list, the process behaves byte-for-byte like the
historical single device (identity block + `NANOLINK_*` env overrides, one loop of each kind,
`:30005` CR listener, unchanged `/health`).

## Conf schema

```yaml
# Global identity block — in fleet mode it only supplies DEFAULTS for entries
# that omit oui/product_class. (Built-ins: 8C1F64 / ENB-N03002-B3.)
identity:
  oui: "8C1F64"
  product_class: "ENB-N03002-B3"
  serial: "2205609999"        # single-device mode only; unused when devices: present

ftp:                          # shared by every device (env-overridable as before)
  host: "ftp"
  user: "nybsys"
  pass: ""
  upload_interval: 60s

scenario: "happy"             # shared by every device (NANOLINK_SCENARIO overrides)

# The fleet. Presence of this list = fleet mode.
devices:
  - serial: "2205610001"      # REQUIRED, must be unique across the list
    # oui:            optional, defaults from identity block
    # product_class:  optional, defaults from identity block
    params:                   # optional TR-069 path -> value overlay (see below)
      Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.PhyCellID: "101"
      Device.Services.FAPService.1.CellConfig.LTE.RAN.Common.CellIdentity: "10000001"
      Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNDL: "1850"
      Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower: "-10"
      Device.PeriodicStatistics.SampleSet.1.Parameter.412.X_8C1F64_CurrentValue: "12.5"
  - serial: "2205610002"
    params:
      Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.PhyCellID: "102"
      Device.Services.FAPService.1.CellConfig.LTE.RAN.Common.CellIdentity: "10000002"
      Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.EARFCNDL: "1850"
      Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower: "-10"
```

The example serials (`22056xxxxx`) and cell identities above are **synthetic placeholders** —
never put real device serials or real network cell ids in tracked files.

### Rules

- `serial` is required per entry; a missing or duplicate serial **fails startup** (the canonical
  `cwmp_id` is unique per tenant on the real cloud — two entries with one serial would fold into
  the same id and silently fight each other).
- `params` values must be quoted YAML **strings** (`"449"`, not `449`) — CWMP carries strings,
  and an unquoted number is a parse error, not a coercion.
- The overlay is applied **last** when seeding the 20,260-param manifest store: after identity
  seeding (`SerialNumber`/`ProductClass`) and after the healthy T3 PM pins — so it can give each
  device its own PhyCellID, CellIdentity, EARFCNDL, ReferenceSignalPower, or per-cell SampleSet
  `CurrentValue`s, and can deliberately override any seeded default.

## Per-device runtime behavior

| Aspect | Fleet behavior |
| --- | --- |
| cwmp_id | `canonicalID(oui, product_class, serial)` per device (`-` → `%2D`), byte-matching the agent's encoding |
| CWMP session loop | one per device against the agent ACS (Inform → GetRPCMethods → TransferComplete → ATC → task drain), Inform carrying the device's own DeviceID |
| FTP upload loop | one per device with its own loggen cycle state; log content + filenames identity-rewritten to the owning device |
| Connection request | one listener per device on port **30005 + index** (device 0 keeps the historical `:30005`); each device's store `Device.ManagementServer.ConnectionRequestURL` and its Inform advertise its own port (host from `NANOLINK_CR_HOST`, default the container hostname) |
| Start stagger | deterministic `index * 700ms` offset per device so N devices don't thundering-herd the agent; device 0 starts immediately |
| `/device/params` | optional `?device=<serial or cwmp_id>` selector (default: first device); unknown selector → 404 |

Note: the agent resolves the connection-request URL per device (`CWMP_CR_URL_OVERRIDE` >
the device's advertised `ConnectionRequestURL` > `deviceIP:30005`), so each fleet device's own
listener gets poked. Leave `CWMP_CR_URL_OVERRIDE` unset in fleet mode — a fixed override would
funnel every CR to one device. CR remains an optimisation: queued tasks also apply on each
device's next ~500ms session.

## Health gates

**Full mode** (`TESTSUITE_MODE=full`, mock cloud on): the single-device gate is unchanged. In
fleet mode `/health` additionally reports

```json
"fleet": {
  "expected": 14,
  "onboarded": 12,
  "ok": false,
  "per_device": {
    "8C1F64-ENB%2DN03002%2DB3-2205610001": {
      "registered": true, "telemetry": 3, "events": 5, "snapshots": 1, "complete": true
    }
  }
}
```

- `registered` — the device appeared in a heartbeat inventory (the agent onboarded it)
- `telemetry` — metric samples received for its cwmp_id; `events` — deduped events
- `snapshots` — config-snapshot posts for its cwmp_id
- `complete` = registered ∧ telemetry>0 ∧ snapshots>0; `onboarded` counts registered devices
- top-level `ok` = the unchanged single-device gate **AND** every expected device `complete`

**acs / acsftp mode** (mock cloud off — what a fleet run against the real local stack uses):
`ok` requires **every** device to have Informed at least once, and a `fleet` section reports
`{expected, informed, per_device: {cwmp_id: informs_sent}}`. Single-device output is unchanged.

## Env vars

- Fleet composition is conf-file-only. `NANOLINK_SERIAL` never touches fleet entries (they carry
  explicit serials); `NANOLINK_OUI` / `NANOLINK_PRODUCT_CLASS` override the identity **block**,
  which fleet entries only inherit when they omit `oui`/`product_class`.
- `NANOLINK_CONF` points at a fleet conf (e.g. a mounted `conf/fleet.conf`).
- `NANOLINK_CR_HOST` sets the host baked into advertised per-device CR URLs.
- `FTP_HOST`/`FTP_USER`/`FTP_PASS`/`NANOLINK_SCENARIO` keep working and apply to all devices.

## Running a fleet

Write a fleet conf, mount it, and point `NANOLINK_CONF` at it — everything else (compose
services, ports, modes) is unchanged. In compose, a 14-device fleet still exposes just `:9000`
(health) — the per-device CR ports are internal only.

Unit tests: `go test ./...` in `testsuite/` covers conf parsing (single/fleet/invalid),
per-device store seeding + overlay, canonical ids, CR port assignment, the fleet health gate,
and the single-device back-compat path.
