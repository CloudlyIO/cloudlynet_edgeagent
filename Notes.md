# Femto Fleet — Local Bring-up

A 14-device mock NanoLink femtocell fleet runs on a laptop against the real platform path
(gateway → smo_sim → postgres) in roughly 15 minutes. Fleet mode ships on `main` since PR #4
(2026-09-23); nothing needs a feature branch.

All paths below are relative to the `cloudlynet_ai` repo root (the parent repo that carries this
submodule), and every command runs from there.

## 1. Prerequisites

- Clone `CloudlyIO/cloudlynet_ai` with submodules (`git submodule update --init --recursive`).
  `submodule/cloudlynet_edgeagent` must be on `main` at or after `39cddf2` (PR #4).
  Check: `submodule/cloudlynet_edgeagent/testsuite/FLEET.md` exists.
- Docker Desktop running, about 8 GB free for images.
- Root `.env`: `cp .env.example .env` (the root `.env` is untracked). Gateway and frontend `.env`
  files are bootstrapped from their `.env.example` by `compose.sh`; both default to local bypass
  mode (`DEV_BYPASS_JWT=true`, tenant `00000000-0000-0000-3029-000000000001`), so no Cognito
  login is needed.
- `curl` and `jq` on the host for the checks below.

## 2. Start the platform stack

`up` gives you infra plus all app services; use `e2e` instead if you also want the dashboard on
`localhost:3000`.

```bash
./scripts/kafka/compose.sh up            # or: ./scripts/kafka/compose.sh e2e
curl -s localhost:8080/v1/health | jq .data.status   # gateway -> "ok"
curl -s localhost:8002/health   | jq .data.status   # smo_sim -> "ok"
```

The `edgeagent` profile is deliberately not part of `up` or `e2e`: it needs an enrollment token
and a fleet conf first (steps 3 to 5) and starts in step 6.

## 3. Create an edge and mint its enrollment token

One edge stands for the box the 14 femtocells sit behind. The token is returned exactly once
(only its hash is stored): copy it, you paste it into the override in step 5.

```bash
TID=00000000-0000-0000-3029-000000000001
curl -s -X POST localhost:8080/v1/tenants/$TID/custom/nybsys/edge-devices \
  -H 'Content-Type: application/json' \
  -d '{"name":"femto-fleet-local"}' | jq .data
# -> { "edge_id": "...", "name": "femto-fleet-local", "enrollment_token": "eyJ..." }
```

Keep the token out of git. Running the call again creates a second edge; remove extras with
`DELETE localhost:8080/v1/tenants/$TID/custom/nybsys/edge-devices/{edge_id}` (cascades that
edge's devices, KPIs and events).

## 4. Write the 14-device fleet conf (synthetic identities)

Fleet mode is switched on by a `devices:` list in the NanoLink conf; the full schema and rules
are in `submodule/cloudlynet_edgeagent/testsuite/FLEET.md`. Every serial must be unique and every
param value must be a quoted YAML string. This generator writes 14 synthetic devices (PCI 101 to
114, cell identity 10000001 to 10000014, RS power -10 dBm) into the gitignored `docs/task_docs/`
folder:

```bash
mkdir -p docs/task_docs && python3 - <<'EOF'
P  = "Device.Services.FAPService.1.CellConfig.LTE.RAN"
SS = "Device.PeriodicStatistics.SampleSet.1.Parameter"   # 316 = PRB DL %, 412 = SINR dB
out = ['identity:', '  oui: "8C1F64"', '  product_class: "ENB-N03002-B3"', '',
       'ftp:', '  host: "ftp"', '  user: "nybsys"', '  pass: ""', '  upload_interval: 60s', '',
       'scenario: "happy"', '', 'devices:']
for i in range(1, 15):
    out += [f'  - serial: "22056100{i:02d}"', '    params:',
            f'      {P}.RF.PhyCellID: "{100 + i}"',
            f'      {P}.Common.CellIdentity: "{10000000 + i}"',
            f'      {P}.RF.EARFCNDL: "1850"',
            f'      {P}.RF.ReferenceSignalPower: "-10"',
            f'      {SS}.316.X_8C1F64_CurrentValue: "{35 + (i % 5) * 9}"',
            f'      {SS}.412.X_8C1F64_CurrentValue: "{12.5 - (i % 5) * 0.7:.1f}"']
open("docs/task_docs/fleet.conf", "w").write("\n".join(out) + "\n")
print("wrote docs/task_docs/fleet.conf (14 devices)")
EOF
```

The `22056100xx` serials and `1000000x` cell identities are placeholders by convention: never put
real device serials or real cell ids in a tracked file.

## 5. Compose override: FTP server + fleet wiring

Create `docker-compose.override.yaml` at the repo root (gitignored; `docker compose` merges it
automatically). It points the agent at the real local gateway, switches the testsuite into fleet
mode, and adds the vsftpd server the fleet uploads its logs to (the root compose has none). The
FTP volume is the same one the agent watches, so uploads land in its ingest directory directly.

```yaml
services:
  cloudlynet-edgeagent:
    environment:
      CLOUDLYNET_ENROLLMENT_TOKEN: <token from step 3>
      CLOUDLYNET_BASE_URL: http://maveric_gateway:8080
      CWMP_CR_URL_OVERRIDE: ""        # fleet mode: each device advertises its own CR URL

  cloudlynet-edgeagent-testsuite:
    environment:
      TESTSUITE_MODE: acsftp          # talk to the real platform, mock cloud off
      NANOLINK_CONF: /conf/fleet.conf
      FTP_PASS: nybsys-local-test
    volumes:
      - ./docs/task_docs/fleet.conf:/conf/fleet.conf:ro
    depends_on:
      ftp:
        condition: service_healthy

  ftp:
    image: delfer/alpine-ftp-server
    container_name: nanolink_local_ftp
    restart: unless-stopped
    profiles: ["edgeagent"]
    environment:
      USERS: nybsys|nybsys-local-test|/home/vsftpd/nybsys
      ADDRESS: ftp
      MIN_PORT: "21100"
      MAX_PORT: "21110"
    volumes:
      - edgeagent_ftp:/home/vsftpd/nybsys
    depends_on:
      ftp-init:
        condition: service_completed_successfully
    healthcheck:
      test: ["CMD-SHELL", "nc -z 127.0.0.1 21"]
      interval: 5s
      timeout: 3s
      retries: 12
    networks:
      - maveric

  ftp-init:
    image: alpine:3.20
    profiles: ["edgeagent"]
    command: ["sh", "-c", "mkdir -p /data/uploads && chown 0:0 /data/uploads && chmod 555 /data/uploads"]
    volumes:
      - edgeagent_ftp:/data
    networks:
      - maveric
```

Rate limit note: the gateway's `/v1/agent/**` per-IP budget is a fixed 120 requests/min on
`main`, sized for one device. Fourteen devices behind one agent IP still onboard and push
telemetry fine, but TR-069 command applies can time out under the resulting 429s. The env knob
`AGENT_RATE_LIMIT_PER_MIN` exists only on the local gateway branch `feat/agent-rate-limit-env`
(not merged); if you need command traffic, check that branch out and add
`gateway: environment: AGENT_RATE_LIMIT_PER_MIN: "900"` to this override.

## 6. Bring the fleet up and verify it is pushing telemetry and logs

```bash
docker compose --profile edgeagent up -d --build
```

Start order is `ftp-init` → `ftp` (healthy) → testsuite (the 14 mock devices, health on host
port 9100) → agent (in-agent TR-069 ACS on 7547). Devices start 700 ms apart and the agent
onboards them on its heartbeat, so allow about two minutes.

Agent-side view, all 14 must have sent an Inform:

```bash
curl -s localhost:9100/health | jq '.fleet | {expected, informed}'    # -> 14 / 14
```

Platform-side view, the rows the agent onboarded:

```bash
docker exec -i postgres sh -c 'psql -U "$POSTGRES_USER" -d maveric' <<'SQL'
select cwmp_id, health, op_state, last_inform_at from nanolink_devices order by cwmp_id;
SQL
```

Expect 14 rows with `health = healthy`, `op_state = t` and `last_inform_at` inside the last
minute.

What the fleet pushes once it is up, with no manual trigger:

| Stream | Path | Lands in | Cadence |
| --- | --- | --- | --- |
| Inform + config snapshot | CWMP session to the agent ACS | `nanolink_devices`, `device_config_snapshots` | ~500 ms per device |
| PM SampleSet values | agent telemetry over `/v1/agent/**` | `device_kpis` | continuous |
| `Log_*.gz`, `ErrorLog_*.gz` | FTP upload → agent watch dir → log parser | `device_events` (modules `FILE_TRANS`, `TR69`, `SON`, ...) | 60 s per device |

Confirm the log push after two minutes:

```bash
docker exec -i postgres sh -c 'psql -U "$POSTGRES_USER" -d maveric' <<'SQL'
select module, event_type, count(*) from device_events
where created_at > now() - interval '5 minutes'
group by 1, 2 order by 3 desc limit 8;
SQL
```

Rows such as `FILE_TRANS / ftp_upload_ok` and `TR69 / autonomous_transfer_complete` mean both the
FTP loop and the log parser work end to end. With the `e2e` stack the same devices appear on
`localhost:3000/devices`.

## 7. Gotchas, reset and re-enrolment

- **Re-enrolling under a new edge resurrects old devices.** The agent keeps its CWMP store in the
  `cloudlynet_ai_edgeagent_buffer` volume and re-announces every device it remembers on the first
  heartbeat after a new token. Before switching edges: `docker compose --profile edgeagent down`,
  `docker volume rm cloudlynet_ai_edgeagent_buffer`, delete the old edge (step 3), then bring the
  profile up again.
- **Stopping the fleet keeps its rows.** `docker compose --profile edgeagent down` leaves
  `nanolink_devices` in place with a stale `last_inform_at`; the same cwmp_ids re-attach to the
  same rows on the next start, so no re-onboarding is needed.
- **Fleet conf errors fail startup on purpose.** A missing or duplicate serial, or an unquoted
  number, stops the testsuite container; `docker logs cloudlynet_ai-cloudlynet-edgeagent-testsuite-1`
  names the offending entry.
- **Everything returns after a Docker restart** (`restart: unless-stopped`), fleet included. After
  an unclean shutdown check `docker ps` for `redis` in `Restarting`: `docker logs redis` showing
  `Bad file format reading the append only file` means the AOF is corrupt, which also keeps
  `copilot-backend` and `copilot-mcp-server` from starting. Fix: stop redis, run
  `redis-check-aof --fix appendonly.aof.manifest` inside `/data/appendonlydir` of the
  `cloudlynet_ai_redis_data` volume (same image the stack runs; back the `*.incr.aof` file up
  first), start redis, then `./scripts/kafka/compose.sh up` again.
- **Teardown.** `docker compose --profile edgeagent down` stops the fleet only;
  `./scripts/kafka/compose.sh down` stops the whole stack. Volumes persist either way; add `-v`
  to wipe them.
