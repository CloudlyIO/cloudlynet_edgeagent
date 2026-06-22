# Agent Context — cloudlynet_edgeagent

## Derived Understanding
- This submodule owns the Go edge agent only. Platform-side `/v1/agent/**` handlers and database persistence are owned by the CloudlyNet AI platform team and are consumed through `design/openapi.yaml`.
- The agent is outbound-only in production: no listener is exposed by the agent binary. It talks to CloudlyNet over REST and to local GenieACS/FTP on the edge device.
- The local `testsuite` is a separate mock microservice for functional testing, not production code. It can run in full mock mode (CloudlyNet + GenieACS + FTP) or `acsftp` mode (only GenieACS + FTP) for live NetAI platform validation.
- Production edge deployment (Ubuntu 22.04) is **native + systemd**, not Docker. `Makefile` + `scripts/install.sh` bootstrap Go, build from source, and install a `cloudlynet-edgeagent` systemd service. Docker/compose is retained only for local/CI functional testing.
- The agent's SQLite is an **embedded local file** via `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`) — not a separate service/container. It only needs a writable data dir (`/var/lib/cloudlynet-agent`); the install toolchain therefore needs no gcc/CGO.

## Assumptions
- Enrollment token payload contains `tenant_id`, `edge_id`, `base_url`, and composite `api_key`.
- Production enrollment tokens should contain `base_url=https://netai.cloudly.io/`; older tokens that still contain `http://localhost:8080` need a temporary `CLOUDLYNET_BASE_URL=https://netai.cloudly.io/` override or, preferably, dashboard key regeneration after the SMO Sim chart is synced with the corrected `PUBLIC_BASE_URL`.
- RadioDevice interaction is TR-069-compatible through GenieACS NBI.
- `design/openapi.yaml` and `design/schemas.sql` already contain the frozen Agent API and NybSys/edge schema; no API/schema behavior change is required in this submodule. `MetricSample.metrics` is open (`additionalProperties`), so the tiered metric keys below are forward-compatible with the existing contract.
- Tiered telemetry keys follow handover §3.4. PM counter paths (`prb_dl_pct`, `sinr_avg_db`, `rrc_success_pct`, etc.) are pinned to the NanoLink `dmcli.new.conf` SampleSet mapping in `collector/metrics.go` (`Device.PeriodicStatistics.SampleSet.1.Parameter.{index}.X_8C1F64_CurrentValue`). T1/T2 live paths and live-hardware T3 paths are concrete.

## Telemetry & Collection
- T1 (live liveness/UE counts), T2 (RF/coverage), T3 (PM + hardware + bounded `Device.FaultMgmt.CurrentAlarm.{i}` alarms). Each tier is read via one GenieACS GPV batch and POSTed at its cadence; devices with no readable metric are skipped.
- `EnsureBaseline` (ATC-policy fix) is applied once per NanoLink, tracked in `Worker.baselined`.
- Registration is retried on heartbeat until CloudlyNet accepts it, allowing a late gateway/port-forward to recover without restarting the agent.
- FTP `seenTGZ` set is pruned to the current directory each scan to stay bounded.

## Impacted Modules
- `goagent`: production edge-agent binary. `collector/metrics.go` holds the tiered metric/alarm catalogue (the place to correct PM paths).
- `testsuite`: mock CloudlyNet and GenieACS service for Docker functional tests; `TESTSUITE_MODE=acsftp` disables the CloudlyNet mock for live-platform validation.
- `Makefile`, `scripts/install.sh`, `scripts/uninstall.sh`, `deploy/`: native Ubuntu 22.04 edge deployment (Go bootstrap, build, systemd service, env-driven config). No platform contract or schema impact.
- Root compose/docs: optional profile wiring and runbook updates only.
