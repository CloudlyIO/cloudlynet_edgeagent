# cloudlynet_edgeagent

CloudlyNet Edge Agent is a Go TR-069 edge process for attaching RadioDevices to the CloudlyNet platform. It runs on the edge device, calls CloudlyNet outbound through the `/v1/agent/**` REST contract, and talks to local GenieACS NBI plus the NanoLink FTP log drop.

## Layout

```text
Dockerfile
docker-compose.yml
config/
  agent.yaml
  rules.yaml
goagent/
  cmd/agent
  internal/config
  internal/cloud
  internal/buffer
  internal/genieacs
  internal/rules
  internal/collector
  internal/worker
testsuite/
  main.go
  Dockerfile
```

The prompt used `testsuire`; the implemented directory is the corrected `testsuite/`.

## Runtime Behavior

- Decodes the enrollment token from `agent.yaml` or `CLOUDLYNET_ENROLLMENT_TOKEN`.
- Sends `X-Edge-Key` to the CloudlyNet Agent API.
- Registers, heartbeats inventory, polls commands, posts telemetry, posts config snapshots, and acks commands.
- Retries registration on heartbeat until CloudlyNet accepts it, so a late gateway/port-forward does not require restarting the agent container.
- Emits lifecycle logs (startup, registration, baseline, per-command result) for observability.
- Uses local SQLite for telemetry outbox retry and applied-command dedupe.
- Uses GenieACS NBI for inventory, GPV, SPV with `connection_request`, reboot, and the baseline ATC policy fix (applied **once per NanoLink**).
- Parses FTP `.tgz` logs into deterministic telemetry events using `config/rules.yaml`; the processed-archive set is pruned to the current directory contents so it stays bounded.

## Telemetry tiering (handover §3.4)

The collector reads canonical metric keys per tier from GenieACS and POSTs each tier at its own cadence (keys absent on a device are omitted; the cloud's `metrics` object is open):

- **T1 (30 s, live):** `op_state`, `rf_tx_status`, `admin_state`, `s1_status`, `sctp_status`, `connected_ues`, `volte_ues`.
- **T2 (60 s, RF/coverage):** `rip_average`, `rip_prb`, `rip_threshold`, `earfcn_dl_inuse`, `pci_inuse`, `rs_power`, `dl_bw`, `ul_bw`.
- **T3 (5 min, PM + hardware + alarms):** `prb_dl_pct`, `prb_ul_pct`, `sinr_avg_db`, `rrc_conn_mean`, `thp_dl`, `thp_ul`, derived `rrc_success_pct`, plus live `uptime`/`mem_free`/`mem_total`/`cpu_usage`, and `alarms[]` from bounded `Device.FaultMgmt.CurrentAlarm.{i}` rows.

The tier-to-path mapping lives in `goagent/internal/collector/metrics.go`. PM counter paths are pinned to the NanoLink `dmcli.new.conf` dump under `Device.PeriodicStatistics.SampleSet.1.Parameter.{index}.X_8C1F64_CurrentValue`; the index constants are kept in one place so a firmware-specific SampleSet reorder is easy to update.

## Local Functional Test

```bash
docker compose up -d --build
curl http://localhost:9000/health
docker compose down -v
```

The testsuite container mocks both CloudlyNet `/v1/agent/**` on port `9000` and GenieACS NBI on port `7557`. The health response becomes `ok: true` after the agent has registered, sent heartbeat/telemetry/snapshots, and acked configure/query/reboot commands.

## Live Platform Validation

Use `EDGEAGENT_TESTSUITE_MODE=acsftp` when CloudlyNet/NetAI is already deployed and only local GenieACS + FTP should be mocked. The testsuite health endpoint remains on `9000`, but `/v1/agent/**` is not mocked in this mode.

```bash
EDGEAGENT_TESTSUITE_MODE=acsftp \
CLOUDLYNET_ENROLLMENT_TOKEN='<enrollment-token>' \
docker compose up -d --build cloudlynet-edgeagent-testsuite cloudlynet-edgeagent

curl http://localhost:9000/health
docker compose logs -f cloudlynet-edgeagent
```

Production enrollment tokens should embed `https://netai.cloudly.io/`. If you are validating an
older token that still contains `http://localhost:8080`, temporarily add
`CLOUDLYNET_BASE_URL='https://netai.cloudly.io/'` to the command above; regenerate the edge key in
the dashboard after SMO Sim is deployed with the corrected `PUBLIC_BASE_URL`.

## Root Compose

From the repository root, the edge stack is optional:

```bash
docker compose --profile edgeagent -f docker-compose.apps.yml up -d --build cloudlynet-edgeagent-testsuite cloudlynet-edgeagent
```

Normal platform startup does not run the edge agent unless the `edgeagent` profile is selected.
