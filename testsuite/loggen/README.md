# loggen — NanoLink log generator

`loggen` builds the log files the mock device uploads over FTP each cycle, shaped exactly like a real
NanoLink's — so the agent's log-ingestion path is exercised against genuine formats, not clean fakes.
It is the *input side* of the testsuite emulator (see [`../README.md`](../README.md)).

## The two artifacts it builds

A real NanoLink uploads two different things; `loggen.Generate()` reproduces both:

| Artifact (filename) | Shape | What it represents |
|---|---|---|
| `<OUI>_<serial>_PowerOn_<ts>_continuouslogging.tgz` | gzipped tar, entries `1`…`10` + `index` + `max` | the device's **rolling operational log** — a ring buffer on flash, every subsystem interleaved |
| `<OUI>_<serial>_PowerOn_<ts>_Devicelog` | a **bare file** (no `.tgz`) | the device's **alarm log** — a smaller, alarm-only stream |

Both carry the same line format:

```
0000000114 2024-06-02 11:26:57.180 [FM] Critical alarm 0x16010400 raised, system reboot ...
└─ seq ───┘ └──── timestamp ─────┘ └mod┘ └───────────────── message ──────────────────┘
```

- **seq** — a monotonic line counter.
- **timestamp** — when the device logged the line.
- **[MODULE]** — the subsystem that emitted it (`FILE_TRANS`, `TR69`, `FM`, `SON`, `SCTP`, `SCM`,
  `NCM`, `DMM`, …). **The agent picks which rules to try from this inline tag** — never from the
  filename (the ring files are just named `1`…`10`).
- **message** — free text; a rule matches a substring of it (e.g. `curl code=(25)`).

## What goes into a generated archive

`Generate(cfg, sampleLines)` gathers three sources, then lays them out into the two artifacts:

1. **The scenario's fault line** — exactly one line, chosen by the selected scenario (below).
2. **The redacted real sample** — `fixtures/real_sample.log` (see [Fixture](#the-fixture)).
3. **Synthetic filler** — a few `[NCM]`/`[SM]`/`[SCM]`/`[SON]` lines so the ring has realistic module
   diversity the sample doesn't cover.

Layout rules:

- The **ring `.tgz`** gets **all** the lines, spread round-robin across entries `1`…`10` (+ `index`/
  `max` bookkeeping) — mirroring how the device spreads a rolling log across its ring files.
- The **`Devicelog`** gets only the **alarm-class** lines — those whose module is `FM`, `TR69`, or
  `SCTP` (`alarmLines`). This matches the real device (its alarm log is a distinct, smaller stream)
  and avoids double-counting the bulk `FILE_TRANS` chatter that lives only in the ring.

## Scenarios

Each scenario stages **one** fault line, kept in lockstep by hand with the agent's rules
(`goagent/internal/rules/engine.go` and `config/rules.yaml`). Selecting a scenario guarantees its
event is present that run; the fixture supplies everything else.

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

`fixtures/real_sample.log` is a **small, redacted, real capture** from an actual NanoLink
(captured as OUI `8C1F64`, serial `2205600282`) — 31 lines hand-picked to cover the formats the agent
must parse. `loggen` **rewrites that captured identity to the configured device** at generation, so the
lines you actually see carry the run's OUI/serial (default `2205609999`), not the capture's:

- all five FTP `curl` codes the device really emits — `(0)` success, `(7)` can't-connect,
  `(25)` STOR-denied, `(28)` timeout, `(67)` login-denied;
- the `[TR69]` ACS-failure lines — `ACS Disconnect with error`, `ACS connect failed`,
  `ACS Connect Status = 2 Connection timed out …`;
- the `[FM]` reboot alarm, and `[SON]`/`[SCTP]` S1-setup / SCTP-peer alarms;
- boot-inventory lines (`[SCM]`/`[DMM]`) carrying the device identity.

**Why it exists:** it proves the agent parses the *genuine article*, not just tidy synthetic strings.
Because it is replayed in **every** scenario, the common events fire on every run — which is why the
`typed_events` list looks the same across scenarios (see the testsuite README's *"Why the runs look
alike"*); the scenario-specific signal is the differentiator.

**Redaction is mandatory.** Clear-text FTP credentials and the vendor ACS IP from the real capture are
replaced with `REDACTED` / `REDACTED-VENDOR-ACS-IP`. Never commit real credentials into a fixture.

## Fidelity scope (deliberate)

- `continuouslogging.tgz` + `Devicelog` only — not the hourly `Log_*` / `ErrorLog_*` archives.
- Ring layout is emitted once; no live rotation/wrap.
- Single device.

## Tests

`make test-suite` (or `cd testsuite && go test ./...`) covers the filename shapes, the numbered-ring
layout, the alarm-subset split, and that each scenario stages the signature its rule expects.
