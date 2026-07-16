# testsuite — configurable NanoLink emulator

A local, Docker-Compose functional test for the edge agent (`goagent/`). Post-CWMP-cutover the
agent **is** the ACS (binds `:7547`, the device dials it), so this module plays every other role a
real NanoLink deployment needs: a mock **CPE device**, a mock **CloudlyNet cloud**, and a real
**vsftpd** for the FTP log-upload path. It drives the agent end-to-end and reports pass/fail via a
`/health` gate.

Background: issue [CloudlyIO/cloudlynet_ai#346](https://github.com/CloudlyIO/cloudlynet_ai/issues/346).

## Architecture

Three containers on one Docker network, one shared volume (`edgeagent_ftp`) as the FTP hand-off:

```
 device (mock) ──curl upload──▶ ftp (vsftpd) ──writes──▶ [ shared volume ] ◀──poll── agent (WatchFTP, 2s)
 device (mock) ──dials :7547──▶ agent (in-agent CWMP ACS)
 agent ──register/heartbeat/telemetry/poll/ack──▶ testsuite (mock cloud :9000)
```

| Container | Role | Ports |
|---|---|---|
| `cloudlynet-edgeagent-testsuite` | mock **device** (dials the ACS + curl-uploads logs) **and** mock **cloud** | `:9000` (health), `:30005` (connection-request, internal) |
| `cloudlynet-edgeagent` | agent under test — CWMP ACS + FTP-log collector + cloud client | `:7547` |
| `ftp` | real `vsftpd` — receives uploads, writes to the shared volume | `:21` + passive `21100–21110` (internal) |
| `ftp-init` | one-shot — pre-stages a write-protected `/uploads` dir for the `ftp-path-reject` scenario | — |

The mock device uploads via real `curl → vsftpd → volume` (not a direct volume write); the agent's
`WatchFTP` picks the files up from there.

## Prerequisites

- **Docker** running (Compose v2).
- **Go** (only for the unit-test targets). On Apple Silicon the FTP image is
  `delfer/alpine-ftp-server` (multi-arch, already wired — no setup).

## Quick start

```sh
make e2e-all                                   # full end-to-end: all 7 scenarios (up → assert → down)
```

Or drive it by hand:

```sh
make docker-up                                 # build + start agent + testsuite + ftp
curl -fsS http://localhost:9000/health | jq    # read the gate ("ok": true when it passes)
make docker-logs                               # tail agent logs (optional)
make docker-down                               # stop + remove (incl. the shared volume)
```

## Commands

| Command | What it does |
|---|---|
| `make test` | Agent unit tests (`goagent/`). |
| `make test-suite` | Testsuite unit tests (`loggen` generator). |
| `make e2e` | End-to-end test of the `happy` scenario (up → assert `/health` → down). |
| `make e2e-scenario SCENARIO=<name>` | Same, for one named scenario (see below). |
| `make e2e-all` | End-to-end sweep across all 7 scenarios; non-zero exit if any fail. |
| `make docker-up` / `make docker-down` | Start / stop+remove the stack (default scenario `happy`). |
| `make docker-logs` | Tail the agent container logs. |
| `make build` | Build the agent binary. |
| `make sync-manifest` | Re-copy the param manifest into the testsuite's embedded asset. |

`make help` lists all targets.

## Scenarios

Each scenario stages a fault-signature log line that must classify to its own typed event; the
`/health` gate asserts that event is present (`saw_expected_event`). Two scenarios additionally fire
a **real transport probe** that reproduces the genuine `curl` failure code.

Run one:

```sh
make e2e-scenario SCENARIO=ftp-auth-fail
# equivalent raw form:
EDGEAGENT_TESTSUITE_SCENARIO=ftp-auth-fail docker compose up -d --build
```

| `SCENARIO=` | Expected typed event | Real transport probe |
|---|---|---|
| `happy` | `ftp_upload_ok` | — (this is the working path) |
| `ftp-path-reject` | `ftp_upload_path_reject` | uploads to write-protected `/uploads` → `curl (25)` (STOR denied) |
| `ftp-auth-fail` | `ftp_auth_fail` | uploads with a wrong password → `curl (67)` (login denied) |
| `ftp-conn-fail` | `ftp_conn_fail` | — (content-only; staged `curl (7)` line) |
| `ftp-timeout` | `ftp_upload_timeout` | — (content-only; staged `curl (28)` line) |
| `atc-fault` | `atc_fault_loop` | — (content-only) |
| `reboot` | `device_reboot` | — (content-only) |

The content-carrying upload always targets the FTP root with working credentials, so the staged
event reaches the agent regardless of scenario; the probe (path-reject / auth-fail) is a separate
best-effort upload whose failure code is visible in the testsuite logs. `s1-failure` is intentionally
not a scenario — no typed rule exists for it yet.

## Configuration

Optional file `conf/nanolink.conf` (all fields default; env vars win over the file; custom path via
`NANOLINK_CONF`):

| `.conf` key | Default | Env override |
|---|---|---|
| `identity.oui` | `8C1F64` | — |
| `identity.product_class` | `ENB-N03002-B3` | — |
| `identity.serial` | `2205600282` | — |
| `ftp.host` | `ftp` | `FTP_HOST` |
| `ftp.user` | `nybsys` | `FTP_USER` |
| `ftp.pass` | *(empty)* | `FTP_PASS` |
| `ftp.upload_interval` | `60s` | — |
| `scenario` | `happy` | `NANOLINK_SCENARIO` |

Compose-level overrides (all optional, with defaults):

| Env var | Default | Effect |
|---|---|---|
| `EDGEAGENT_TESTSUITE_SCENARIO` | `happy` | selects the scenario (→ `NANOLINK_SCENARIO`) |
| `EDGEAGENT_TESTSUITE_PORT` | `9000` | host port for the mock cloud / `/health` |
| `EDGEAGENT_TESTSUITE_MODE` | `full` | `full` / `acs` / `acsftp` (see Debug modes) |
| `EDGEAGENT_CWMP_PORT` | `7547` | host port for the agent's CWMP ACS |
| `FTP_USER` / `FTP_PASS` | `nybsys` / `nybsys-local-test` | vsftpd + device credentials |

## Reading `/health`

`GET :9000/health` (always HTTP 200; `ok` is a body field). `"ok": true` requires **all** of:
`registered`, `heartbeats`, `telemetry`, `events`, `failures`, `snapshots` > 0; `snapshot_params == 24`;
`acks` ≥ 3; at least one entry in `typed_events`; `typed_event_on_canonical: true` (a real-log event
keyed on the canonical device id, not a bare OUI or `unknown`); and `saw_expected_event: true` (the
selected scenario's `expected_event` classified). The response also echoes `expected_event` and the
sorted `typed_events` list for inspection.

## Log generation

`loggen.Generate` builds the two real upload shapes:

- **`<OUI>_<serial>_PowerOn_<ts>_continuouslogging.tgz`** — the numbered ring (entries `1…10` +
  `index`/`max`, all modules interleaved), module living inline as `[FILE_TRANS]`/`[TR69]`/`[FM]`/… on
  each line.
- **`<OUI>_<serial>_PowerOn_<ts>_Devicelog`** — a bare (no `.tgz`) alarm-class subset.

Content is a small **redacted real sample** (`fixtures/real_sample.log`) plus **synthetic per-module
filler** and the scenario's fault line. Fidelity scope: `continuouslogging` + `Devicelog` only, ring
layout without live rotation, single device. Real FTP credentials are redacted in the fixture —
**never commit real credentials.**

## Manifest-seeded param store

The mock device answers `GetParameterValues` from the full 20,260-param NanoLink manifest
(`assets/nanolink_param_manifest.json`) with each path's real `xsi:type`. The single source of truth
is the agent's in-repo copy (`goagent/internal/cwmp/assets/…`); re-sync with `make sync-manifest`
after it changes, then rebuild the testsuite image.

## Debug modes

`EDGEAGENT_TESTSUITE_MODE=acs` (or `acsftp`) disables the mock cloud and serves a lighter `/health`
(CWMP dial status + FTP archive count) for debugging the CWMP or FTP path in isolation.

## Real-box parity

Before treating a laptop run as representative of the real edge box, confirm from the box:

- The real `vsftpd.conf` passive port range and the FTP user/password.
- The FTP-root watch directory (moot in this stack — the shared volume is the same data regardless of
  each container's mount path).

## Known limitations (emulator, not agent bugs)

- **Growth over long runs** — each upload cycle writes a new `_PowerOn_<ts>_` archive + Devicelog and
  re-ingests, so `events`, disk, and the collector's seen-path map grow with runtime. Fine for the
  short functional runs this suite is for.
- **Alarm overlap** — alarm lines appear in both the ring and the Devicelog (realistic); the agent's
  content-derived dedup key collapses them at the cloud.
- **Cold-start deferral** is covered by unit tests, not exercised in `docker-up` (the device Informs
  before its first upload, so device-id resolution always succeeds live).
