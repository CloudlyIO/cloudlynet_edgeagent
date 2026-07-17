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

## How the end-to-end test works

Only the **agent is under test**; the testsuite mocks everything around it (see Architecture). A run
drives the agent through the full device lifecycle, then asserts the result via the `/health` gate.

**One run, end to end:**

```
 device Inform ─▶ agent stores the canonical device id                   [onboard]
      ├─▶ AutonomousTransferComplete → agent answers EMPTY, never Fault   [CWMP session survives]
      ├─▶ cloud sends 3 commands → agent applies over CWMP → acks         [command loop]
      ├─▶ agent reads 24 managed params (GPV) → config snapshot           [snapshot == 24]
      └─▶ device curl-uploads  ring.tgz + bare Devicelog
             └▶ real vsftpd → shared volume → agent WatchFTP → parse:
                  · module from the inline [MODULE] tag   · bare Devicelog ingested too
                  · device = canonical id via the store   · rules classify the lines
             └▶ typed events → telemetry (1st push force-failed → outbox retry) → cloud
                  └▶ /health flips ok:true once every check below passes
```

**The gate — `ok` is one big AND:**

```
 ok =  registered>0 AND heartbeats>0 AND telemetry>0 AND events>0
   AND failures>0                 (outbox retry exercised)
   AND snapshots>0 AND snapshot_params==24
   AND acks>=3                    (command loop worked)
   AND typed_events not empty
   AND typed_event_on_canonical   (event keyed to the real device, not a phantom)
   AND saw_expected_event         (THIS scenario's own signal classified)   ← per-scenario check
```

`ok:true` collapses ~12 independent checks into one boolean. **Read the `/health` JSON only when `ok`
is `false`** — the field that's `false` points at the stage that broke.

**Why the runs look alike — and where to actually look:** the redacted real corpus
(`fixtures/real_sample.log`) is replayed in every run and already carries curl `0/7/25/28/67` + an FM
reboot + TR69 ACS lines, so the **same 8 `typed_events` fire in every scenario** (expected). The field
that differs — and makes each run a distinct test — is **`expected_event`** (+ `saw_expected_event`).
`atc-fault` is the tell: its event isn't in the corpus, so `atc_fault_loop` appears in `typed_events`
*only* in that run. Per run, look at **`ok` + `expected_event`**; ignore the repetitive list.

`/health` fields: `ok`, `registered`, `heartbeats`, `telemetry`, `events`, `failures`, `snapshots`,
`snapshot_params`, `acks`, `typed_events`, `typed_event_on_canonical`, `expected_event`,
`saw_expected_event`. (Always HTTP 200; `ok` is a body field.)

## Coverage — what this verifies / what it does NOT

Use this to judge whether the suite is sufficient for a given change. A green `make e2e-all`
asserts, **every run**:

- **Agent ↔ cloud lifecycle** — register + heartbeat + telemetry (incl. one forced failure →
  outbox retry recovers).
- **CWMP onboarding** — device `Inform` → agent resolves and stores the **canonical device id**.
- **ATC handled without a SOAP Fault** — the session survives to the ACS's read/write turn (the
  reason the CWMP rework exists).
- **Full CPE RPC round-trip** — `GetRPCMethods` / `TransferComplete` / GPV / SPV / GPN / `Reboot`.
- **Config snapshot** — the 24 managed params read via GPV and pushed to the cloud.
- **Command loop** — 3 cloud commands (configure / query / reboot) applied over CWMP and acked.
- **FTP log ingestion over a real vsftpd** (curl → ftpd → shared volume → `WatchFTP`), covering the
  four ingestion fixes: module routed per-line from the inline `[MODULE]` tag; bare `Devicelog`
  ingested; event keyed to the canonical device (not a phantom); rules classify curl `0/7/25/28/67`,
  the TR-069 vendor-ACS-unreachable text, and the FM reboot alarm.
- **Per-scenario signal** — each of the 7 fault lines classifies to its own typed event
  (`saw_expected_event`).
- **Two transport failures reproduced live over the wire** — `curl (25)` (STOR-denied `/uploads`)
  and `curl (67)` (bad login).
- **Content-derived dedup** — identical log lines collapse to one event.

It does **NOT** cover (out of scope by design):

- **Multiple devices / cells** — single device only.
- **RAN/RF, E2 / A1 / O1, or KPI streams** — none; this exercises CWMP + FTP logs only.
- **Adversarial/malformed CWMP** — well-formed happy-path session shapes only (no SOAP fuzzing,
  oversized payloads, or auth attacks on `:7547`).
- **`curl (7)` / `(28)` over the wire** — their rules are exercised via staged log content, but the
  connection-failure / timeout transports are not actually induced live.
- **Real cloud ingestion / Postgres dedup** — the cloud is a mock; dedup is asserted against its
  in-memory set, not a real `ON CONFLICT(dedup_key)`.
- **Cold-start deferral / not-yet-onboarded device** — unit-tested only; not exercised in
  `docker-up` (the device always Informs before its first upload).
- **Agent restart / buffer persistence / crash recovery**, and **performance / soak** — not driven.
- **Real-box vsftpd parity** (passive range, real credentials) — see Real-box parity below.

Operational caveat: over long runs each upload cycle writes a new `_PowerOn_<ts>_` archive and
re-ingests, so `events` / disk / the collector's seen-path map grow with runtime — fine for the
short functional runs this suite is for; the real device rotates in place.

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
EDGEAGENT_TESTSUITE_SCENARIO=ftp-auth-fail docker compose -f docker-compose.test.yml up -d --build
```

| `SCENARIO=` | Represents (real-world condition) | Typed event | Live transport probe |
|---|---|---|---|
| `happy` | log upload succeeds | `ftp_upload_ok` | — (this *is* the working path) |
| `ftp-path-reject` | server refuses the write — misconfigured/read-only upload path (the **dominant** real failure) | `ftp_upload_path_reject` | yes → real `curl (25)` (STOR denied at `/uploads`) |
| `ftp-auth-fail` | wrong / rotated FTP credentials | `ftp_auth_fail` | yes → real `curl (67)` (login denied) |
| `ftp-conn-fail` | FTP server down / unreachable | `ftp_conn_fail` | — (staged `curl (7)`) |
| `ftp-timeout` | slow / unresponsive server, congested link | `ftp_upload_timeout` | — (staged `curl (28)`) |
| `atc-fault` | the ACS breaks the CWMP session — the bug the in-agent-CWMP rework fixed (**regression guard**) | `atc_fault_loop` | — |
| `reboot` | a critical fault auto-reboots the device (e.g. S1-setup max-retry) | `device_reboot` | — |

The content-carrying upload always targets the FTP root with working credentials, so the staged event
reaches the agent regardless of scenario; the probe (path-reject / auth-fail) is a separate
best-effort upload whose real `curl` code shows in the testsuite logs.

**Why these seven?** Each is a fault the NanoLink actually emits (validated against the captured logs)
**and** one the agent has a typed rule for — so a scenario drives exactly one rule end-to-end. The five
FTP scenarios are the five `curl` outcomes the device's log-upload really produces (`0/7/25/28/67`);
`atc-fault` guards the CWMP session-killer the rework fixed; `reboot` is the critical device event. A
fault with no rule (e.g. S1-setup failure) is **not** a scenario — it would only land as generic
`unclassified`. New rule → new scenario.

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

**Full detail — the line format, the two artifacts, the per-scenario staged lines, and the fixture:
[`loggen/README.md`](loggen/README.md).**

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
