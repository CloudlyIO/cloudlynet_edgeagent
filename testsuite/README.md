# testsuite — configurable NanoLink emulator

A local, Docker-Compose functional test for the edge agent (`goagent/`). Post-CWMP-cutover the
agent **is** the ACS (binds `:7547`, the device dials it), so this module plays every other role a
real NanoLink deployment needs: a mock **CPE device**, a mock **CloudlyNet cloud**, and a real
**vsftpd** for the FTP log-upload path. It drives the agent end-to-end and reports pass/fail via a
`/health` gate.

Background: issue [CloudlyIO/cloudlynet_ai#346](https://github.com/CloudlyIO/cloudlynet_ai/issues/346).

## Two ways to run

| Goal | Mode | How | `.env` |
|---|---|---|---|
| Test the agent locally (CI / pre-PR) | **full** (all-mock) | `make e2e` / `e2e-all` / `verify` | ignored |
| Validate against a real cloud (onboard / read / push from the dashboard) | **acsftp** | copy `.env.example` → `.env`, set the token, `docker compose up` | required |

- **Full mock** is self-contained (mock cloud + mock device + real vsftpd). The `make` targets always
  run this and **ignore `.env`** (`docker compose --env-file /dev/null`), so a real-cloud `.env` can
  never redirect them. No `.env` needed — just run the command.
- **acsftp** switches the mock cloud off so the agent talks to your real platform. It's a manual
  `docker compose up` driven by `.env` — see [Validating against the real platform](#validating-against-the-real-platform).

## Architecture

Three containers on one Docker network, one shared volume (`edgeagent_ftp`) as the FTP hand-off:

```
 device (mock) ──curl upload──▶ ftp (vsftpd) ──writes──▶ [ shared volume ] ◀──poll── agent (WatchFTP, 2s)
 device (mock) ──dials :7547──▶ agent (in-agent CWMP ACS)
 agent ──register/heartbeat/telemetry/poll/ack──▶ testsuite (mock cloud :9000)
```

| Container                        | Role                                                                                      | Ports                                                     |
| -------------------------------- | ----------------------------------------------------------------------------------------- | --------------------------------------------------------- |
| `cloudlynet-edgeagent-testsuite` | mock **device** (dials the ACS + curl-uploads logs) **and** mock **cloud**                | `:9000` (health), `:30005` (connection-request, internal) |
| `cloudlynet-edgeagent`           | agent under test — CWMP ACS + FTP-log collector + cloud client                            | `:7547`                                                   |
| `ftp`                            | real `vsftpd` — receives uploads, writes to the shared volume                             | `:21` + passive `21100–21110` (internal)                  |
| `ftp-init`                       | one-shot — pre-stages a write-protected `/uploads` dir for the `ftp-path-reject` scenario | —                                                         |

The mock device uploads via real `curl → vsftpd → volume` (not a direct volume write); the agent's
`WatchFTP` picks the files up from there.

## Quick start

```sh
make verify                                    # unit tests (agent) + unit tests (testsuite) + e2e sweep (7 scenarios)
make e2e-all                                   # just the e2e sweep (7 scenarios) [up → assert → down]
make e2e-all VERBOSE=1                         # ...with the per-check breakdown
```

Or drive it by hand:

```sh
make docker-up                                 # build + start agent + testsuite + ftp
curl -fsS http://localhost:9000/health | jq    # read the gate ("ok": true when it passes)
make docker-logs                               # tail agent logs (optional)
make docker-down                               # stop + remove (incl. the shared volume)
```

## How the end-to-end test works

Only the **agent is under test**; the testsuite stands in for everything around it — mock device, mock
cloud, and a **real** vsftpd (see Architecture). A run drives the agent through the full device
lifecycle, then asserts the result via the `/health` gate.

**One run, end to end:**

```
 device Inform ─▶ agent stores the canonical device id                   [onboard]
      ├─▶ AutonomousTransferComplete → agent answers EMPTY, never Fault   [CWMP session survives]
      ├─▶ cloud sends 3 commands → agent applies over CWMP → acks         [command loop]
      ├─▶ agent reads 24 managed params (GPV) → config snapshot           [snapshot == 24]
      └─▶ device curl-uploads  Log_*.gz + ErrorLog_*.gz (routine ~60s feed)
             └▶ real vsftpd → shared volume → agent WatchFTP → parse:
                  · module from the inline [MODULE] tag   · single-file gzip un-gzipped
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

## Commands

| Command                               | What it does                                                                          |
| ------------------------------------- | ------------------------------------------------------------------------------------- |
| `make test`                           | Agent unit tests (`goagent/`).                                                        |
| `make test-suite`                     | Testsuite unit tests (`loggen` generator).                                            |
| `make test-all`                       | Unit tests for **both** modules (`goagent` + `testsuite`).                            |
| `make e2e`                            | End-to-end test of the `happy` scenario. Add `VERBOSE=1` for the per-check breakdown. |
| `make e2e-scenario SCENARIO=<name>`   | Same, for one named scenario (see below).                                             |
| `make e2e-all`                        | End-to-end sweep across all 7 scenarios; non-zero exit if any fail.                   |
| `make verify`                         | **Full pre-PR gate**: `test-all` (fail-fast) → `e2e-all`.                             |
| `make docker-up` / `make docker-down` | Start / stop+remove the stack (default scenario `happy`).                             |
| `make docker-logs`                    | Tail the agent container logs.                                                        |
| `make build`                          | Build the agent binary.                                                               |
| `make sync-manifest`                  | Re-copy the param manifest into the testsuite's embedded asset.                       |

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

| `SCENARIO=`       | Represents (real-world condition)                                                               | Typed event              | Live transport probe                               |
| ----------------- | ----------------------------------------------------------------------------------------------- | ------------------------ | -------------------------------------------------- |
| `happy`           | log upload succeeds                                                                             | `ftp_upload_ok`          | — (this _is_ the working path)                     |
| `ftp-path-reject` | server refuses the write — misconfigured/read-only upload path (the **dominant** real failure)  | `ftp_upload_path_reject` | yes → real `curl (25)` (STOR denied at `/uploads`) |
| `ftp-auth-fail`   | wrong / rotated FTP credentials                                                                 | `ftp_auth_fail`          | yes → real `curl (67)` (login denied)              |
| `ftp-conn-fail`   | FTP server down / unreachable                                                                   | `ftp_conn_fail`          | — (staged `curl (7)`)                              |
| `ftp-timeout`     | slow / unresponsive server, congested link                                                      | `ftp_upload_timeout`     | — (staged `curl (28)`)                             |
| `atc-fault`       | the ACS breaks the CWMP session — the bug the in-agent-CWMP rework fixed (**regression guard**) | `atc_fault_loop`         | —                                                  |
| `reboot`          | a critical fault auto-reboots the device (e.g. S1-setup max-retry)                              | `device_reboot`          | —                                                  |

The content-carrying upload always targets the FTP root with working credentials, so the staged event
reaches the agent regardless of scenario; the probe (path-reject / auth-fail) is a separate
best-effort upload whose real `curl` code shows in the testsuite logs.

**Why these seven?** Each is a fault the NanoLink actually emits (validated against the captured logs)
**and** one the agent has a typed rule for — so a scenario drives exactly one rule end-to-end. The five
FTP scenarios are the five `curl` outcomes the device's log-upload really produces (`0/7/25/28/67`);
`atc-fault` guards the CWMP session-killer the rework fixed; `reboot` is the critical device event. A
fault with no rule (e.g. S1-setup failure) is **not** a scenario — it would only land as generic
`unclassified`. New rule → new scenario.

## Reading the console output

Two verbosity levels:

**Default (`make e2e-all`)** — one line per scenario, plus a footer listing what every run also checks:

```
NanoLink emulator · end-to-end (7 scenarios)

 [1/7] happy            PASS   upload OK (curl 0) -> ftp_upload_ok
 [2/7] ftp-path-reject  PASS   STOR denied at /uploads (curl 25) -> ftp_upload_path_reject
 ...
 RESULT: 7/7 PASS
```

**Verbose (`make e2e-all VERBOSE=1`, or `scripts/e2e.sh --verbose`)** — the per-check breakdown behind
each PASS. Every scenario prints the **same 8 checks** (only the number/label after each changes),
**plus a 9th** that appears _only_ for the two scenarios with a live wire-level probe:

```
── [2/7] ftp-path-reject ─────────────────────────────────────
 verifies: STOR denied at /uploads (curl 25) -> ftp_upload_path_reject
   agent registered with cloud        ok  registered=1
   CWMP session reached ACS turn      ok  acks=3 (ATC answered, no Fault)
   config snapshot                    ok  snapshot_params=24
   commands applied + acked           ok  acks=3
   outbox retry exercised             ok  failures=1
   real FTP upload ingested           ok  events=38
   scenario signal classified         ok  expected_event=ftp_upload_path_reject
   event keyed to real device         ok  typed_event_on_canonical=true
   transport fault reproduced         ok  curl: (25) in device logs   ← 9th, probe-only
 => PASS
```

What each line means (`ok`/`XX` = pass/fail). The text after is the `/health` value the check read —
except the 9th (probe) line, which greps the mock device's container logs, and line 2's parenthetical,
which is an annotation, not a field:

| Console line                                        | Passes when                        | Reading the value                       | What it proves                                                                                                                                                                                                                                                      |
| --------------------------------------------------- | ---------------------------------- | --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **agent registered with cloud**                     | `registered > 0`                   | `registered=1`                          | Agent found the mock cloud and enrolled. Nothing downstream works if this is 0.                                                                                                                                                                                     |
| **CWMP session reached ACS turn**                   | `acks > 0`                         | `acks=3 (ATC answered, no Fault)`       | Mock device dialed `:7547`; agent answered the Inform **and** the ATC — session survived past the ATC instead of faulting (the GenieACS bug this replaces).                                                                                                         |
| **config snapshot**                                 | `snapshot_params == 24`            | `snapshot_params=24`                    | Agent read the full 24-path managed catalogue via GPV and posted a config snapshot. Exactly 24 — not 23, not 25.                                                                                                                                                    |
| **commands applied + acked**                        | `acks >= 3`                        | `acks=3`                                | Cloud pushed configure / query / reboot; agent applied all three over CWMP and acked. (Same counter as line 2, higher bar.)                                                                                                                                         |
| **outbox retry exercised**                          | `failures > 0`                     | `failures=1`                            | At least one telemetry POST was force-failed so the SQLite outbox retry path actually runs.                                                                                                                                                                         |
| **real FTP upload ingested**                        | `events > 0`                       | `events=38`                             | Device `curl`ed logs into the **real vsftpd**; agent's `WatchFTP` picked them off the shared volume, parsed them, emitted events.                                                                                                                                   |
| **scenario signal classified**                      | `saw_expected_event == true`       | `expected_event=ftp_upload_path_reject` | **The per-scenario discriminator** — the one line whose _label_ changes per scenario. Confirms _this_ scenario's expected typed event fired.                                                                                                                        |
| **event keyed to real device**                      | `typed_event_on_canonical == true` | `typed_event_on_canonical=true`         | The typed event was attributed to the canonical `%2D`-encoded `cwmp_id`, not a phantom/unresolved id.                                                                                                                                                               |
| **transport fault reproduced** _(9th — probe-only)_ | `curl: (NN)` found in device logs  | `curl: (25) in device logs`             | Appears **only** for `ftp-path-reject` (curl 25) and `ftp-auth-fail` (curl 67) — the two scenarios that make a real `curl → vsftpd` upload genuinely fail with that code over the wire. The other five are content-only, so this line is absent (you see 8, not 9). |

Lines 2 and 4 read the **same** `acks` counter (`>0` vs `>=3`), so a run that fails only line 4 got a
partial command loop.

## Configuration

> **`make e2e` / `e2e-all` / `verify` ignore `.env`** — they run the self-contained mock-cloud gate
> (`docker compose --env-file /dev/null`, always full mode + mock cloud), so nothing here can point the
> agent at a real cloud by accident. The knobs below apply to a **manual `docker compose up`** (e.g. the
> acsftp run in [Validating against the real platform](#validating-against-the-real-platform)).

Optional file `conf/nanolink.conf` (all fields default; env vars win over the file; custom path via
`NANOLINK_CONF`):

| `.conf` key              | Default         | Env override             |
| ------------------------ | --------------- | ------------------------ |
| `identity.oui`           | `8C1F64`        | `NANOLINK_OUI`           |
| `identity.product_class` | `ENB-N03002-B3` | `NANOLINK_PRODUCT_CLASS` |
| `identity.serial`        | `2205609999`    | `NANOLINK_SERIAL`        |
| `ftp.host`               | `ftp`           | `FTP_HOST`               |
| `ftp.user`               | `nybsys`        | `FTP_USER`               |
| `ftp.pass`               | _(empty)_       | `FTP_PASS`               |
| `ftp.upload_interval`    | `60s`           | —                        |
| `scenario`               | `happy`         | `NANOLINK_SCENARIO`      |

> **Why the serial is synthetic.** `cwmp_id = OUI-ProductClass-Serial` is UNIQUE per tenant on the
> real cloud, so reusing a *real* device's serial makes an `acsftp` run collide with that device's
> cell (your heartbeat updates it instead of creating one under your edge → nothing shows on the
> dashboard). Default `2205609999` is test-only; override any identity field per-tester when sharing
> a cloud. Full mode is unaffected (mock cloud has no unique constraint). The configured identity also
> drives the device id in the **generated FTP log content** — the corpus's captured id is rewritten to
> match, so filenames *and* log lines read consistently.

Compose-level overrides (all optional, with defaults):

| Env var                        | Default                        | Effect                                       |
| ------------------------------ | ------------------------------ | -------------------------------------------- |
| `EDGEAGENT_TESTSUITE_SCENARIO` | `happy`                        | selects the scenario (→ `NANOLINK_SCENARIO`) |
| `EDGEAGENT_TESTSUITE_OUI` / `_PRODUCT_CLASS` / `_SERIAL` | _(empty → conf default)_ | device identity for acsftp (→ `NANOLINK_OUI` / `_PRODUCT_CLASS` / `_SERIAL`) |
| `EDGEAGENT_TESTSUITE_PORT`     | `9000`                         | host port for the mock cloud / `/health`     |
| `EDGEAGENT_TESTSUITE_MODE`     | `full`                         | `full` / `acs` / `acsftp` (see Debug modes)  |
| `EDGEAGENT_CWMP_PORT`          | `7547`                         | host port for the agent's CWMP ACS           |
| `FTP_USER` / `FTP_PASS`        | `nybsys` / `nybsys-local-test` | vsftpd + device credentials                  |

## Log generation

`loggen.Generate` builds the real **periodic-feed** upload shapes — the ~60s VendorLog stream the
device pushes (not the reboot-only `continuouslogging.tgz` dump, whose agent intake is parked):

- **`Log_<date>.<time>+<tz>_<OUI>.<serial>.gz`** — a single-file gzip (NOT a tar), emitted **every
  cycle**. Carries the **operational** stream (boot / success / config / status), **jittered** each
  cycle (advancing seq + fresh ts; the upload-log line references this cycle's own filename) so it's a
  distinct delta, not a replay.
- **`ErrorLog_<date>.<time>+<tz>_<OUI>.<serial>.gz`** — same shape, emitted **only during an incident
  window** (occasional burst; see `loggen.IsIncidentCycle`). Carries the **incident** lines (curl
  failures / ACS / reboot / SCTP·SON faults) **verbatim** — the same lines also land in that cycle's
  `Log`, so the cloud's content-dedup collapses the overlap (correlated, like a real device).

Content is a small **redacted real sample** (`fixtures/real_sample.log`) plus **synthetic per-module
filler** and the scenario's signature line (staged in every `Log`). Fidelity scope: `Log_*.gz` +
`ErrorLog_*.gz` only; operational lines jittered / incident lines sticky (a fixed corpus — message text
repeats, seq·ts·filename advance); cycle-based incident windows; single device. Real FTP credentials
are redacted in the fixture — **never commit real credentials.**

**Full detail — the line format, the two artifacts, the per-scenario staged lines, and the fixture:
[`loggen/README.md`](loggen/README.md).**

## Manifest-seeded param store

The mock device answers `GetParameterValues` from the full 20,260-param NanoLink manifest
(`assets/nanolink_param_manifest.json`) with each path's real `xsi:type`. The single source of truth
is the agent's in-repo copy (`goagent/internal/cwmp/assets/…`); re-sync with `make sync-manifest`
after it changes, then rebuild the testsuite image.

## Debug modes

`EDGEAGENT_TESTSUITE_MODE` selects what the testsuite plays:

| Value | Mock cloud (`:9000`) | Behaviour |
|---|---|---|
| `full` *(default when unset; `.env.example` ships `acsftp`)* | full mock cloud | mock cloud + mock CWMP device + real vsftpd — the complete e2e gate above. |
| `acs` / `acsftp` | **disabled** | testsuite plays only the CWMP device + FTP and serves a lighter `/health` (CWMP dial status + FTP archive count). Point the agent at a **real** cloud, or isolate the CWMP/FTP path. |

`acs` and `acsftp` are currently behaviour-equivalent (both hit the lighter health handler); the two
names exist to signal intent (CWMP-only focus vs. CWMP+FTP against a real cloud).

## Validating against the real platform

`full` mode is self-contained. To validate the live config loop against a **real** NetAI cloud — "add
this as a device and configure it from the dashboard", no lab hardware — run in `acsftp` mode: the mock
cloud switches off and the testsuite plays only the mock NanoLink device (+ FTP), so the real agent
talks to your platform.

This is a **manual `docker compose up`** flow — the `make e2e*` targets deliberately ignore `.env`, so
run compose directly:

```sh
cp .env.example .env
# edit .env: set CLOUDLYNET_ENROLLMENT_TOKEN (dashboard "add device"); MODE is already acsftp
docker compose -f docker-compose.test.yml up -d --build \
  cloudlynet-edgeagent-testsuite cloudlynet-edgeagent ftp ftp-init
```

Then, on the dashboard:

- **Onboard** — the mock device dials the agent's ACS; the agent registers it → it appears as a CWMP
  device.
- **Read (pull)** — the agent posts a 24-param config snapshot + telemetry → visible on the device's
  Config / telemetry views.
- **Push** — edit a writable param (`ReferenceSignalPower`, `PeriodicInformInterval`, anything under
  `.CellConfig.`) → the agent applies it over CWMP (`SetParameterValues`), reads it back, and the
  command reaches `applied` with the matching value.

Notes: use a real enrollment token (override `CLOUDLYNET_BASE_URL` if it embeds `localhost`); the agent
needs **outbound** reachability only (NAT-friendly, no port-forward); commands arrive on the poll
cadence. The mock device is in-memory, so pushed config persists for the session. In this mode `/health`
on `:9000` is the lighter CWMP-dial + FTP-archive view (the mock-cloud gate is off).

## Real-box parity

Before treating a laptop run as representative of the real edge box, confirm from the box:

- The real `vsftpd.conf` passive port range and the FTP user/password.
- The FTP-root watch directory (moot in this stack — the shared volume is the same data regardless of
  each container's mount path).
