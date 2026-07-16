# testsuite — configurable NanoLink emulator

Local functional test for the edge agent (`goagent/`), driven entirely by
Docker Compose. Post-CWMP-cutover the agent **is** the ACS — it binds `:7547`
and the device dials it — so this module plays every other role a real
NanoLink deployment needs: the mock CPE device, the mock CloudlyNet cloud, and
(via `docker-compose.yml`) a real `vsftpd` for the FTP log-upload path.

See [`docs/task_docs/Nybsys_nanolink/test_suite_ext/`](../../maveric_platform_dev/docs/task_docs/Nybsys_nanolink/test_suite_ext/)
(dev repo, gitignored) for the full analysis/plan behind this (ticket #346).

## Architecture

Three containers, one shared volume (`edgeagent_ftp`) as the FTP hand-off:

```
 device (mock, in this binary) ──curl upload──▶ ftp (vsftpd) ──writes──▶ [ shared volume ] ◀──poll── agent (WatchFTP, 2s)
 device (mock, in this binary) ──dials :7547──▶ agent (in-agent CWMP ACS)
 agent  ──register/heartbeat/telemetry/poll/ack──▶ testsuite (mock cloud :9000)
```

| Container | Role | Ports |
|---|---|---|
| `cloudlynet-edgeagent-testsuite` | mock **device** (dials the ACS + curl-uploads logs) **and** mock **cloud** | `:9000`, `:30005` (connection-request) |
| `cloudlynet-edgeagent` | agent under test — CWMP ACS + FTP-log collector + cloud client | `:7547` |
| `ftp` | real vsftpd (`delfer/alpine-ftp-server`) — receives uploads, writes to the shared volume | `:21` + passive range 21100–21110 |

The mock device does **not** write to the shared volume directly — it builds
a real-shaped log archive (`testsuite/loggen`) and `curl -T ... ftp://ftp/`
uploads it, exactly like the real NanoLink. vsftpd writes it into the volume;
the agent's `WatchFTP` picks it up from there. This is the fix for finding
**C** (no FTP server in the old testsuite — see below).

## Running it

```sh
make docker-up     # builds + starts agent, testsuite, ftp
curl -fsS http://localhost:9000/health | jq
make docker-down    # stop + remove (including the shared volume)
```

`/health` on the mock cloud reports:

- `ok`: true once every gate below has been hit at least once
- `registered/heartbeats/telemetry/events/failures/snapshots/acks`: agent↔cloud lifecycle counters
- `snapshot_params`: must equal 24 (the managed config-snapshot catalogue)
- `typed_events` / `typed_event_on_canonical`: the real-log events that
  classified to a typed rule (not `unclassified`), keyed on the **canonical**
  cwmp_id — this is the assertion that findings A and B actually landed.

## What this emulator fixed vs. the old testsuite

The old testsuite wrote one hand-crafted `.tgz` (with `TR69.log`/`FILE_TRANS.log`/
`FM.log` entries) straight into the shared volume — no FTP protocol, no
real-shaped logs. That hid two real agent bugs:

- **A — module routing was filename-based.** Real NanoLink logs are numbered
  ring files (`1`…`10`/`index`/`max`) with every module interleaved; the module
  lives inline as `[FILE_TRANS]`/`[TR69]`/`[FM]`/… on each line. Fixed in
  `goagent/internal/rules/engine.go` (`moduleFromLine`).
- **A′ — bare `Devicelog` uploads were dropped.** The real alarm log uploads
  as a bare file (no `.tgz`); the collector's glob was `.tgz`-only. Fixed in
  `goagent/internal/collector/collector.go` (`isLogUpload`).
- **B — device-id extraction broke on real filenames.** Real uploads are named
  `<OUI>_<serial>_PowerOn_<ts>_...`; splitting on the first `_` yielded the
  bare OUI, dropping the serial (mis-keyed / phantom device). Fixed by
  resolving `serial → canonical id` via the `cwmp_devices` store the CWMP
  `Inform` populates (`resolveDeviceID`), with bounded deferral for the
  cold-start race (log arrives before the device's first Inform).
- **E — rule-coverage gaps.** Added `curl (7)`/`(28)` rules; retargeted
  `vendor_acs_unreachable` from a hardcoded IP substring to the real
  `TR69`-module ACS-failure text (module-agnostic on the IP).

This module (`testsuite/loggen` + the real `vsftpd`) is the **forcing
function**: feeding real-shaped logs over a real FTP hop is what actually
exercises A/A′/B — the old fake dodged all of it by construction.

## Log generation (`testsuite/loggen`)

`loggen.Generate` builds a real-shaped ring archive (`<OUI>_<serial>_PowerOn_
<ts>_continuouslogging.tgz`, entries `1`…`10`/`index`/`max`, all modules
interleaved) and a bare `Devicelog`, from:

- a small **redacted real sample** (`testsuite/fixtures/real_sample.log`) —
  proves the parser handles the genuine article. FTP creds in the original
  corpus (`nybsys:<password>`, `nybsysftp:<token>`) are redacted to
  `REDACTED` — never commit real credentials in a fixture (or in this doc).
- **synthetic per-module filler** (NCM/SM/etc. the sample doesn't cover)
- the active **scenario's** fault-signature line (see below)

Fidelity scope (deliberate, matches the dev/lead decisions in the epic):
`continuouslogging.tgz` + `Devicelog` only (no hourly `Log_*`/`ErrorLog_*`);
ring layout without live rotation/wrap; single device.

## Manifest-seeded param store

The mock device's param store is seeded from the full NanoLink parameter
manifest (`testsuite/assets/nanolink_param_manifest.json`, 20,260 params) —
not the ~33 hand-picked values the old testsuite hardcoded. A
`GetParameterValues` for any managed path now returns a realistic value and
the correct `xsi:type` (`manifest.go`).

`testsuite/` is a separate Go module and cannot import `goagent/internal`, so
it embeds its **own copy** of the manifest. The single source of truth is the
in-repo, production-embedded copy:

```sh
make sync-manifest   # copies goagent/internal/cwmp/assets/... -> testsuite/assets/...
```

Run this whenever the production manifest changes, then rebuild the
testsuite image.

## Configuration (`testsuite/conf/nanolink.conf`)

One `.conf` drives identity, FTP transport, and the scenario. Every field has
a hardcoded default (`config.go`), so the file is optional. Env vars
(`FTP_HOST`/`FTP_USER`/`FTP_PASS`/`NANOLINK_SCENARIO` — already wired in
`docker-compose.yml`) take precedence over the file, matching this repo's
existing env-first convention.

## Scenarios

Select via `NANOLINK_SCENARIO` (or `docker-compose.yml`'s
`EDGEAGENT_TESTSUITE_SCENARIO`) or the `.conf`'s `scenario:` key.

Every scenario stages its fault-signature line in the generated log content,
and that content-carrying upload **always** targets the FTP root with the
configured (working) credentials — so the fault line reliably reaches the
agent and `/health`'s typed-event gate is real regardless of scenario. For the
two scenarios that are also self-triggering at the transport layer
(`ftp-path-reject`, `ftp-auth-fail`), the mock device *additionally* fires a
separate probe upload against the actual broken path/credentials, proving the
FTP hop itself reproduces the real curl failure code (finding C) — logged,
expected to fail, and deliberately independent of the content-carrying upload
above (if the fault line's own bytes had to survive the broken transport,
they'd never arrive — that would make the typed-event assertion hollow for
exactly the two scenarios meant to prove it).

| Scenario | Stages (content, always delivered) | Self-triggering transport probe |
|---|---|---|
| `happy` | `curl code=(0)` → `ftp_upload_ok` | n/a — this *is* the working path |
| `ftp-path-reject` | `curl code=(25)` → `ftp_upload_path_reject` | yes — probe uploads to `/uploads/`, a directory pre-staged (`ftp-init` in `docker-compose.yml`) as root-owned mode 555: CWD succeeds, `STOR` is denied → genuine curl **(25)**, matching the real box's `/uploads`-era failures exactly (not curl (9), which is what an outright-missing directory would give) |
| `ftp-auth-fail` | `curl code=(67)` → `ftp_auth_fail` | yes — probe uploads with a deliberately wrong password → genuine curl **(67)** (`Access denied: 530`) |
| `ftp-conn-fail` | `curl code=(7)` → `ftp_conn_fail` | **no** — `docker compose stop ftp` reproduces a genuine transport failure, but curl reports it as **(6)** `Could not resolve host: ftp` (Docker's embedded DNS drops the stopped container's network alias), not curl (7) — content delivery (and thus `/health`) doesn't depend on getting the exact code, but don't expect a literal 7 here |
| `ftp-timeout` | `curl code=(28)` → `ftp_upload_timeout` | **no** — inject delay/packet loss on the `ftp` container (e.g. `tc netem`) to reproduce; content delivery doesn't depend on it |
| `atc-fault` | `RPC Unknown received from ACS` → `atc_fault_loop` | n/a (log-content only) |
| `reboot` | FM critical-alarm reboot line → `device_reboot` | n/a (log-content only) |

Verified manually (2026-07-16): `ftp-path-reject` → testsuite logs `curl: (25)
Failed FTP upload: 553`; `ftp-auth-fail` → `curl: (67) Access denied: 530`;
`docker compose stop ftp` → `curl: (6) Could not resolve host: ftp` (see
above — not a literal 7). The first two match the real corpus's recorded curl
codes exactly.

`s1-failure` is deliberately **not** a scenario: no typed rule exists for
S1-setup failures yet (only the generic `unclassified` fallback), so a gate on
it would assert nothing meaningful. Tracked as a rule-coverage follow-up.

## Real-box parity (non-blocking, but read before trusting a laptop run)

The suite builds and passes locally with the defaults above. Before treating a
laptop run as representative of the real edge box, confirm:

- The real box's `vsftpd.conf` passive port range and FTP user/password
  (the FTP user is `nybsys`; get the password from the box or the lead —
  **do not** put the real password in this repo).
- The real box's `FTP_WATCH_DIR` for the FTP root after the `/uploads` → `/`
  upload-path switch (moot in this compose stack, since the shared volume is
  the same data regardless of each container's mount path name).

## Known limitations (emulator, not agent bugs)

- **No upload cleanup / unbounded growth over long runs.** Each upload cycle
  writes a fresh `_PowerOn_<ts>_` archive + Devicelog and re-ingests the full
  corpus under the new filename, so the mock cloud's `events` counter, the
  shared volume, and the collector's `seenPaths` map all grow with runtime.
  Fine for the short functional runs this suite is for; the real device
  rotates/overwrites in place.
- **Deferral / cold-start race is unit-tested, not exercised live.** In
  `docker-up` the device always Informs before its first upload, so the B-fix
  resolves the canonical id immediately and the deferral → `unresolved:`
  sentinel path never fires. That path is covered by `collector_test.go`
  (`TestScanFTPDefersUnknownDeviceThenExhausts`, `TestScanFTPColdStartRace`).
- **Alarm lines appear in both the ring and the Devicelog** — realistic (the
  real device logs an alarm to both its rolling log and its alarm log). The
  agent dedups by `(device, ts, event, raw)`, but that `ts` is parse-time, not
  the log line's own timestamp, so the same alarm can be double-counted if the
  two uploads are parsed in different wall-clock seconds. A line-timestamp dedup
  key is a possible agent follow-up (out of scope here).

## Debug modes

`TESTSUITE_MODE=acs` or `acsftp` disable the mock cloud and expose a
lighter-weight `/health` (CWMP dial status + FTP archive count) instead —
useful when debugging just the CWMP or FTP path in isolation. Note: since the
testsuite no longer mounts the shared FTP volume (it uploads via curl now),
the `ftp.archives` counter in this mode always reads 0 — it globs the
testsuite container's own (now-unmounted) `/ftp` scratch dir, not the shared
volume. Not on the main (`full`-mode) validation path.
