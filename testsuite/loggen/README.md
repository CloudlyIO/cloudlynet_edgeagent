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
> emit them; it now emits the **routine feed** a real box actually pushes every minute. The agent still
> ingests all shapes (`.tgz`, bare `Devicelog`, and `Log_*.gz`/`ErrorLog_*.gz`); only the emulator's
> routine emission changed. The reboot-dump shapes stay covered by the agent's unit tests.

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

## What goes into a generated slice

`Generate(cfg, sampleLines)` gathers three sources, then lays them into the two artifacts:

1. **The scenario's fault line** — exactly one line, chosen by the selected scenario (below).
2. **The redacted real sample** — `fixtures/real_sample.log` (see [Fixture](#the-fixture)).
3. **Synthetic filler** — a few `[NCM]`/`[SM]`/`[SCM]`/`[SON]` lines for module diversity the sample
   doesn't cover.

Layout rules:

- The **`Log_*.gz`** gets **all** the lines — the full operational slice.
- The **`ErrorLog_*.gz`** gets only the **error/alarm** lines — those whose text carries an
  `alarm`/`fault`/`fail`/`error` marker (`errorLogLines`, the same predicate the agent's `rules.alarmy`
  fallback uses). A `curl code=(0)` **success** is not an error, so the happy-path line stays out of
  the ErrorLog; real background alarms in the corpus keep it non-empty anyway.

Because the ErrorLog's lines are a subset of the Log's, the same alarm arrives via **both** files —
exactly as on the real device — and the cloud's content-derived dedup key collapses the duplicate.

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
  ring or the hourly variants.
- A **fixed corpus replayed each cycle** — no live per-minute deltas or ring rotation. (Real
  consecutive `Log_*.gz` are disjoint deltas; the emulator repeats the same slice, and content-dedup
  collapses the repeats.)
- Single device.

## Tests

`make test-suite` (or `cd testsuite && go test ./...`) covers the filename shapes, the single-file-gzip
layout, the error-subset split, and that each scenario stages the signature its rule expects.
