# Agent Context — cloudlynet_edgeagent

## Derived Understanding
- This submodule owns the Go edge agent only. Platform-side `/v1/agent/**` handlers and database persistence are owned by the CloudlyNet AI platform team and are consumed through `design/openapi.yaml`.
- The agent **hosts an in-agent TR-069/CWMP ACS on `:7547`** (the exact port the NanoLink dials, so replacing the old ACS needs no device-side change). It talks to CloudlyNet outbound over REST and watches the local FTP log drop. The device dials the agent; the agent dials the cloud.
- The local `testsuite` is a separate mock microservice (its own Go module — it cannot import `goagent/internal/*`, so it hand-rolls SOAP) for functional testing, not production code. It can run in full mock mode (CloudlyNet cloud + a mock NanoLink CWMP device that dials `:7547` + FTP) or `acsftp` mode (only the CWMP device + FTP) for live NetAI platform validation.
- Production edge deployment (Ubuntu 22.04) is **native + systemd**, not Docker. `Makefile` + `scripts/install.sh` bootstrap Go, build from source, and install a `cloudlynet-edgeagent` systemd service. Docker/compose is retained only for local/CI functional testing.
- The agent's SQLite is an **embedded local file** via `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`) — not a separate service/container. It only needs a writable data dir (`/var/lib/cloudlynet-agent`); the install toolchain therefore needs no gcc/CGO.

## Assumptions
- Enrollment token payload contains `tenant_id`, `edge_id`, `base_url`, and composite `api_key`.
- Production enrollment tokens should contain `base_url=https://netai.cloudly.io/`; older tokens that still contain `http://localhost:8080` need a temporary `CLOUDLYNET_BASE_URL=https://netai.cloudly.io/` override or, preferably, dashboard key regeneration after the SMO Sim chart is synced with the corrected `PUBLIC_BASE_URL`.
- RadioDevice interaction is TR-069/CWMP: the agent is the ACS itself, answering Inform, `AutonomousTransferComplete` (the RPC GenieACS never handled → the crux fix), `GetParameterValues`, `SetParameterValues`, `GetParameterNames`, and `Reboot` on `:7547`.
- `design/openapi.yaml` and `design/schemas.sql` hold the Agent API and NybSys/edge schema. The one contract change here is the coordinated rename of the device-id field `genieacs_id` → `cwmp_id` (agent slice; value byte-identical since the agent computes the same canonical string). `MetricSample.metrics` is open (`additionalProperties`), so the tiered metric keys below are forward-compatible.
- Tiered telemetry keys follow handover §3.4. PM counter paths (`prb_dl_pct`, `sinr_avg_db`, `rrc_success_pct`, etc.) are pinned to the NanoLink `dmcli.new.conf` SampleSet mapping in `collector/metrics.go` (`Device.PeriodicStatistics.SampleSet.1.Parameter.{index}.X_8C1F64_CurrentValue`). T1/T2 live paths and live-hardware T3 paths are concrete.
- Config snapshots are separate from telemetry and must cover the same 24 managed paths as SMO Sim `MANAGED_PARAMS` and frontend `managed-params.ts`. The agent waits up to 10 seconds for the connection-requested GPV cache refresh and skips empty maps; platform-side storage merges non-empty partial reads and command read-backs rather than treating either as a destructive replacement.
- The device id is the canonical `cwmp_id` (computed from the Inform `DeviceID`, byte-identical to the id the previous ACS stored) across inventory, heartbeat, telemetry, and snapshots. If it contains a literal percent sequence such as `%2D`, `SendSnapshot` path-escapes the whole identifier (`%252D` on the wire) so the HTTP router decodes it exactly once and resolves the same cloud NanoLink row.
- Southbound config verification is value-aware: after `SetParameterValues` the worker reads back with `GetParameterValues` until all expected writes match or `command_verify_timeout` expires (15s production). Failure evidence keeps the device's `actual` read-back alongside the requested `expected` value.

## Telemetry & Collection
- T1 (live liveness/UE counts), T2 (RF/coverage), T3 (PM + hardware + bounded `Device.FaultMgmt.CurrentAlarm.{i}` alarms). Each tier is read from the CWMP parameter cache (refreshed by queued `GetParameterValues`) and POSTed at its cadence; devices with no readable metric are skipped.
- `AutonomousTransferComplete` is answered with the empty response so the session survives — this obsoletes the old per-device ATC-policy=None stopgap (`EnsureBaseline` / `Worker.baselined`, both removed). An `autonomous_transfer_complete` event is emitted into the telemetry batch on each ATC.
- Registration is retried on heartbeat until CloudlyNet accepts it, allowing a late gateway/port-forward to recover without restarting the agent.
- FTP `seenTGZ` set is pruned to the current directory each scan to stay bounded.

## Impacted Modules
- `goagent`: production edge-agent binary. `collector/metrics.go` holds the tiered metric/alarm catalogue (the place to correct PM paths).
- `goagent/internal/cwmp`: the in-agent TR-069/CWMP ACS (SOAP codec, canonical device-id, embedded param manifest, session state machine + ATC handler, `:7547` listener, connection-request). `buffer` extends its SQLite store with the CWMP device/param/event tables.
- `testsuite`: mock CloudlyNet cloud + mock NanoLink CWMP device (dials `:7547`) for Docker functional tests; `TESTSUITE_MODE=acsftp` disables the CloudlyNet mock for live-platform validation.
- `Makefile`, `scripts/install.sh`, `scripts/uninstall.sh`, `deploy/`: native Ubuntu 22.04 edge deployment (Go bootstrap, build, systemd service, env-driven config). No platform contract or schema impact.
- Root compose/docs: optional profile wiring and runbook updates only.
