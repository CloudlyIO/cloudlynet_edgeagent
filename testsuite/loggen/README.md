# loggen — NanoLink log generator

`loggen` builds the log files the mock device uploads over FTP each cycle, shaped exactly like a real
NanoLink's — so the agent's log-ingestion path is exercised against genuine formats, not clean fakes.
It is the *input side* of the testsuite emulator (see [`../README.md`](../README.md)).

## The two artifacts it builds

A real NanoLink pushes a **routine periodic feed** every ~60s (`PeriodicUploadInterval=60s`) — a small
single-file gzip slice of its operational log. `loggen.Generate()` reproduces that feed and its
error-weighted sibling:

| Artifact (filename) | Shape | What it represents |
|---|---|---|
| `Log_<date>.<time>+<tz>_<OUI>.<serial>.gz` | **single-file gzip** (not a tar) | the device's **routine ~60s operational slice** — every subsystem interleaved |
| `ErrorLog_<date>.<time>+<tz>_<OUI>.<serial>.gz` | single-file gzip | the **error-weighted slice** — the alarm/fault/fail/error lines of the same feed |

> **Why not `continuouslogging.tgz` + `Devicelog`?** Those are the device's **reboot/power-on dump**
> (a whole 10-slot ring tarred) and its bare alarm log — episodic, not routine. The emulator used to
> emit them; it now emits the **routine feed** a real box actually pushes every minute. The agent's
> `.tgz`/`Devicelog` dispatch is currently **parked** (commented out in `collector.go`) pending the
> ingestion revamp — so it ingests only `Log_*.gz`/`ErrorLog_*.gz`; the parked readers stay unit-tested.

Both carry the same line format:

```
0000000114 2024-06-02 11:26:57.180 [FM] Critical alarm 0x16010400 raised, system reboot ...
└─ seq ───┘ └──── timestamp ─────┘ └mod┘ └───────────────── message ──────────────────┘
```

- **seq** — a monotonic line counter.
- **timestamp** — when the device logged the line.
- **[MODULE]** — the subsystem that emitted it (`FILE_TRANS`, `TR69`, `FM`, `SON`, `SCTP`, `SCM`,
  `NCM`, `DMM`, …). **The agent picks which rules to try from this inline tag** — never from the
  filename.
- **message** — free text; a rule matches a substring of it (e.g. `curl code=(25)`).

The **filename** carries OUI+serial dot-joined in the last `_`-token (`…_8C1F64.2205600282.gz`); the
agent resolves the device from that tail (the date/time/tz are cosmetic — the parser ignores them).

## What goes into a generated slice — operational vs incident

`Generate(cfg, sampleLines)` splits the corpus (via `isErrorLine`) into two streams and lays them out
by cycle:

- **Operational stream** (persistent) — the non-error lines: boot/identity, upload *success*, config
  reads, status, `RPC Unknown` chatter. Emitted **every cycle** into the `Log`, but **jittered**: each
  line gets an **advancing sequence number + this cycle's timestamp** (so each ~60s slice is a distinct
  delta, like a real device — not a byte-identical replay), and the upload-log line is **repointed at
  this cycle's own `Log` filename**.
- **Incident stream** (bursty) — the error/alarm lines: `curl` failures, ACS failures, the reboot
  alarm, SCTP/SON faults. Emitted **only during an incident window** (`IsIncidentCycle`), **verbatim**
  (sticky seq+ts). During a window those lines go into **both** that cycle's `Log` and its `ErrorLog`.
- **Scenario line** — the selected scenario's one signature line, staged in **every** cycle's `Log`
  (the guaranteed `/health` signal), plus synthetic `[NCM]`/`[SM]`/`[SCM]` filler.

**Incident windows** (`IsIncidentCycle`, cycle-based): baseline-clean first, then a **2-cycle burst**
recurring every `incidentPeriod` cycles — at a ~60s Log cadence, first burst ~60s in, then ~5 min quiet,
repeat. This mirrors the real device, which dumps an `ErrorLog` *around an incident*, not on a fixed
timer (the real archive has 15,907 `Log_*.gz` but only 4 `ErrorLog_*.gz`, clustered in one burst).

**Correlation → dedup.** The incident lines are byte-identical (sticky) in both the `Log` and the
concurrent `ErrorLog` — and recur identically across windows — so the cloud's content-derived dedup key
collapses the overlap, exactly as when a real device logs an alarm to both streams. The jittered
operational lines, by contrast, are fresh every cycle → distinct events. So a run exercises **both**
fresh-delta ingestion **and** dedup.

## Scenarios

Each scenario stages **one** fault line, kept in lockstep by hand with the agent's rules
(`goagent/internal/rules/engine.go` and `config/rules.yaml`). Selecting a scenario guarantees its
event is present that run (staged in the always-uploaded `Log` feed); the fixture supplies the rest.

| Scenario | Staged line (module + signature) | Agent classifies it as |
|---|---|---|
| `happy` | `[FILE_TRANS] … curl code=(0)` | `ftp_upload_ok` |
| `ftp-path-reject` | `[FILE_TRANS] … curl code=(25) … /uploads` | `ftp_upload_path_reject` |
| `ftp-auth-fail` | `[FILE_TRANS] … curl code=(67)` | `ftp_auth_fail` |
| `ftp-conn-fail` | `[FILE_TRANS] … curl code=(7)` | `ftp_conn_fail` |
| `ftp-timeout` | `[FILE_TRANS] … curl code=(28)` | `ftp_upload_timeout` |
| `atc-fault` | `[TR69] RPC Unknown received from ACS` | `atc_fault_loop` |
| `reboot` | `[FM] Critical alarm … system reboot …` | `device_reboot` |

(An unknown/empty scenario falls back to the `happy` line.)

## The fixture

`fixtures/real_sample.log` is a **small, redacted, real capture** sampled from the device's periodic
`Log_*.gz` + `ErrorLog_*.gz` feed (captured as OUI `8C1F64`, serial `2205600282`) — hand-picked to
cover the formats the agent must parse. `loggen` **rewrites that captured identity to the configured
device** at generation, so the lines you see carry the run's OUI/serial (default `2205609999`):

- the FTP `curl` codes the periodic feed really emits — `(0)` success, `(7)` can't-connect,
  `(25)` STOR-denied, `(28)` timeout (`(67)` login-denied is not in the routine feed — it is staged
  only by the `ftp-auth-fail` scenario);
- the `[TR69]` ACS-failure lines — `ACS Disconnect with error`, `ACS connect failed`,
  `ACS Connect Status = 2 …`; plus `RPC Unknown received from ACS`;
- the `[FM]` reboot alarm and periodic-upload lines, and `[SON]`/`[SCTP]` S1-setup / SCTP-peer alarms;
- boot-inventory lines (`[SCM]`/`[DMM]`/`[NCM]`) carrying the device identity.

**Why it exists:** it proves the agent parses the *genuine article*, not just tidy synthetic strings.
Because it is replayed in **every** scenario, its common events fire on every run — the scenario-specific
staged line is the differentiator.

**Redaction is mandatory.** Clear-text FTP credentials (`nybsys:<password>`) from the real capture are
replaced with `nybsys:REDACTED` — get the real password from the box or the lead, never commit it into
a fixture or doc. (This feed carries no vendor-ACS public IP — all hosts are the local `192.168.8.100`.)

## Fidelity scope (deliberate)

- `Log_*.gz` + `ErrorLog_*.gz` only — the routine feed, not the reboot-dump `continuouslogging.tgz`
  ring or the hourly variants (the agent's dump-shape intake is parked — see `collector.go`).
- **Operational lines are jittered** (advancing seq + fresh ts) so each cycle is a distinct delta;
  **incident lines are sticky** for dedup. It is still a *fixed corpus* sampled per cycle — the message
  *text* repeats (only seq / ts / filename advance), so it's a realistic delta, not fully novel content.
- **Incident windows are cycle-based** — an approximation of the real rarity/burst, not a reproduction
  of an incident's actual cause. The fast `make e2e` gate passes on the first (baseline) cycle, so the
  incident/`ErrorLog` path is exercised by the unit tests + a longer manual `docker compose up` run
  (watch ~2 min), not by the fast gate.
- Single device.

## Tests

`make test-suite` (or `cd testsuite && go test ./...`) covers the filename shapes, the single-file-gzip
layout, the incident-window schedule (`IsIncidentCycle`), the baseline-clean vs incident split, the
operational-jitter vs sticky-incident dedup anchor, the filename-field touch, and that each scenario
stages the signature its rule expects.
