# cloudlynet_edgeagent

CloudlyNet Edge Agent is a Go TR-069 edge process for attaching RadioDevices to the CloudlyNet platform. It runs on the edge device, calls CloudlyNet outbound through the `/v1/agent/**` REST contract, and **hosts an in-agent TR-069/CWMP ACS on `:7547`** (the exact port the NanoLink already dials) so it reads and writes device parameters directly, plus it watches the NanoLink FTP log drop.

> **Replaces GenieACS.** The agent used to be a GenieACS NBI client; GenieACS captured only the device name and could not push config because it had no handler for the `AutonomousTransferComplete` (ATC) RPC the NanoLink emits every ~60 s (the CWMP session faulted and died right after the Inform). The agent now answers ATC with the empty `AutonomousTransferCompleteResponse`, so sessions survive to the read/write turn. See **Migration from GenieACS** below.

## Layout

```text
Makefile
Dockerfile
docker-compose.yml
config/
  agent.yaml          # docker/test config (embeds a dev token)
  rules.yaml
deploy/
  cloudlynet-edgeagent.service  # systemd unit
  agent.yaml          # production config (no token; env-driven)
  agent.env.example   # runtime env template (token, endpoints)
scripts/
  install.sh          # Ubuntu 22.04 native installer (Go bootstrap + build + systemd)
  uninstall.sh
goagent/
  cmd/agent
  internal/config
  internal/cloud
  internal/buffer            # outbox/applied + CWMP param/event store
  internal/cwmp              # in-agent TR-069/CWMP ACS (:7547) + ATC handler
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
- Reads the complete 24-path NanoLink managed-parameter catalogue for configuration snapshots. Because a device read only lands when the NanoLink dials in, the snapshot read waits briefly for a fresh value and falls back to the last cached value; an empty result is logged and skipped rather than published as a blank configuration.
- Uses one canonical device id (`cwmp_id`, computed from the Inform `DeviceID` — byte-identical to the id the previous ACS stored). A literal percent escape in the id (for example `%2D`) is escaped again when used as the `config-snapshot` URL path parameter, so SMO receives the same id that heartbeat and telemetry persist.
- Treats every device write as a closed-loop operation: after `SetParameterValues`, waits `command_verify_delay`, then reads back with `GetParameterValues` until all requested values match or `command_verify_timeout` expires (15s in production). A failed acknowledgement includes the actual read-back, so the dashboard can show the device value that prevented the update.
- Retries registration on heartbeat until CloudlyNet accepts it, so a late gateway/port-forward does not require restarting the agent container.
- Emits lifecycle logs (startup, `[CWMP] ACS listening on …`, per-Inform/ATC, per-command result) for observability.
- Uses local SQLite for telemetry outbox retry, applied-command dedupe, and the CWMP device/parameter/event store.
- **Is the ACS:** it answers the device's Inform and `AutonomousTransferComplete` (the RPC GenieACS never handled), reads via `GetParameterValues`, writes via `SetParameterValues`, walks `GetParameterNames` once on first contact for authoritative writability, and reboots — all over the in-agent `:7547` listener. An optional connection-request trigger sharpens apply latency below the device's ~60 s inform cadence.
- Emits an `autonomous_transfer_complete` event on each ATC into the telemetry batch (smo-sim ingests it).
- Parses FTP `.tgz` logs into deterministic telemetry events using `config/rules.yaml`; the processed-archive set is pruned to the current directory contents so it stays bounded.

## Telemetry tiering (handover §3.4)

The collector reads canonical metric keys per tier from the CWMP parameter cache (refreshed by queued `GetParameterValues` reads) and POSTs each tier at its own cadence (keys absent on a device are omitted; the cloud's `metrics` object is open):

- **T1 (30 s, live):** `op_state`, `rf_tx_status`, `admin_state`, `s1_status`, `sctp_status`, `connected_ues`, `volte_ues`.
- **T2 (60 s, RF/coverage):** `rip_average`, `rip_prb`, `rip_threshold`, `earfcn_dl_inuse`, `pci_inuse`, `rs_power`, `dl_bw`, `ul_bw`.
- **T3 (5 min, PM + hardware + alarms):** `prb_dl_pct`, `prb_ul_pct`, `sinr_avg_db`, `rrc_conn_mean`, `thp_dl`, `thp_ul`, derived `rrc_success_pct`, plus live `uptime`/`mem_free`/`mem_total`/`cpu_usage`, and `alarms[]` from bounded `Device.FaultMgmt.CurrentAlarm.{i}` rows.

The tier-to-path mapping lives in `goagent/internal/collector/metrics.go`. PM counter paths are pinned to the NanoLink `dmcli.new.conf` dump under `Device.PeriodicStatistics.SampleSet.1.Parameter.{index}.X_8C1F64_CurrentValue`; the index constants are kept in one place so a firmware-specific SampleSet reorder is easy to update.

## Edge Device Install (Ubuntu 22.04, native + systemd)

Production deployment on an edge box runs the agent as a native systemd service —
no Docker. The agent's SQLite is an embedded local file (`modernc.org/sqlite`,
pure Go, `CGO_ENABLED=0`), not a separate service, so there is nothing to
containerize for storage; it only needs a writable data dir.

```bash
# from the submodule checkout on the edge device
sudo ./scripts/install.sh
# set the enrollment token, then start
sudo sed -i 's#^CLOUDLYNET_ENROLLMENT_TOKEN=.*#CLOUDLYNET_ENROLLMENT_TOKEN=<token>#' /etc/cloudlynet-agent/agent.env
sudo systemctl start cloudlynet-edgeagent
journalctl -u cloudlynet-edgeagent -f
```

`install.sh` is idempotent and, run as root:

1. Ensures Go ≥ 1.23 — uses an existing toolchain or downloads the official
   tarball to `/usr/local/go` (Ubuntu 22.04's apt Go is too old for `go.mod`).
2. Builds the binary from source (`CGO_ENABLED=0`).
3. Creates the `cloudlynet` system user and `/etc/cloudlynet-agent` +
   `/var/lib/cloudlynet-agent`.
4. Installs the binary to `/usr/local/bin`, config/rules (without clobbering
   operator edits — new versions land as `*.default`), and `agent.env` (mode 0600).
5. Installs/enables the systemd unit. It starts the service only once
   `CLOUDLYNET_ENROLLMENT_TOKEN` is set, otherwise it enables and prints the
   start command.

Secrets and host endpoints live in `/etc/cloudlynet-agent/agent.env`
(`CLOUDLYNET_ENROLLMENT_TOKEN`, optional `CLOUDLYNET_BASE_URL`, `CWMP_LISTEN`,
optional `CWMP_CR_USER` / `CWMP_CR_PASS` / `CWMP_CR_URL_OVERRIDE`,
`FTP_WATCH_DIR`, `BUFFER_DB`) and override `/etc/cloudlynet-agent/agent.yaml`.
The agent binds `CWMP_LISTEN` (default `0.0.0.0:7547`) — the exact address the
NanoLink dials — so any prior ACS on that port must be stopped first (see
**Migration from GenieACS**). The FTP log-drop dir is assumed to already exist.

Common `make` targets: `build`, `test`, `vet`, `run`, `install`, `uninstall`,
`docker-build`, `docker-up`, `docker-down`, `docker-logs` (`make help` lists all).

Remove with `sudo ./scripts/uninstall.sh` (add `--purge` to also drop config,
data, and the user).

## Local Functional Test

```bash
docker compose up -d --build
curl http://localhost:9000/health
docker compose down -v
```

The testsuite container mocks the CloudlyNet `/v1/agent/**` cloud on port `9000` **and plays a mock NanoLink CWMP device** that dials the agent's in-agent ACS at `:7547` (Inform → ATC → GPV/SPV/GPN/Reboot), plus a connection-request listener on `:30005`. The health response becomes `ok: true` after the agent has registered, sent heartbeat/telemetry/snapshots, acked configure/query/reboot commands over CWMP, and delivered all 24 managed configuration paths — with the ATC session completing (no Fault).

## Live Platform Validation

Use `EDGEAGENT_TESTSUITE_MODE=acsftp` when CloudlyNet/NetAI is already deployed and only the local CWMP device + FTP should be mocked (the agent talks to a real cloud). The testsuite health endpoint remains on `9000`, but `/v1/agent/**` is not mocked in this mode.

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

## Migration from GenieACS (on-box, server-side only — no device change)

The agent binds the exact `host:port` GenieACS used (`Device.ManagementServer.URL`,
e.g. `192.168.8.100:7547`), so cutover is server-side only — the NanoLink keeps
informing to the same address and never notices.

1. **Stop and disable GenieACS + its Mongo/Node runtime** on the edge box, and
   confirm `:7547` is free:
   ```bash
   sudo systemctl stop  genieacs-cwmp genieacs-nbi genieacs-ui genieacs-fs
   sudo systemctl disable genieacs-cwmp genieacs-nbi genieacs-ui genieacs-fs
   sudo systemctl stop mongod    # GenieACS storage — no longer needed
   sudo ss -ltnp | grep 7547     # should be empty before starting the agent
   ```
2. **Deploy the agent** (native/systemd per *Edge Device Install* above). Confirm
   the log line `[CWMP] ACS listening on 0.0.0.0:7547`, then a device
   `Inform` followed by an `ATC` with **no Fault** (session survives), and a
   first-contact `GetParameterNames` writability walk.
3. **Converge the cloud device row.** The heartbeat now reports the canonical
   `%2D`-encoded id as `cwmp_id`; the stale un-escaped phantom row is removed by
   the cloud migration (sibling dev issue), not by the agent.
4. **Validate push** from the dashboard (edit e.g. `PeriodicInformInterval`) →
   command reaches `applied` with a matching read-back.

Three on-box processes (GenieACS CWMP/NBI/UI) + MongoDB + the Node.js runtime
collapse to one agent binary.

> **Codec fixtures / pcap:** the CWMP codec round-trip tests are built from the
> NanoLink parameter manifest + the TR-069 spec; no real captured-bytes pcap was
> available. Real-bytes round-trip proof is part of on-device validation, which
> is tracked separately (lab-access gated).

## Root Compose

From the repository root, the edge stack is optional:

```bash
docker compose --profile edgeagent -f docker-compose.apps.yml up -d --build cloudlynet-edgeagent-testsuite cloudlynet-edgeagent
```

Normal platform startup does not run the edge agent unless the `edgeagent` profile is selected.
