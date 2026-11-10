# Femto Fleet — Local Bring-up

A 14-device mock NanoLink femtocell fleet runs on a laptop against the real platform path
(gateway → smo_sim → postgres). Fleet mode ships on `main` since PR #4 (2026-09-23).

**It is one command now.** What used to be seven manual steps — minting a token by hand, running a
Python generator into a gitignored conf, authoring a `docker-compose.override.yaml` with two extra
services — is `./scripts/compose.sh up --femtofleet`. The override file is gone: `ftp`, `ftp-init`,
`CWMP_CR_URL_OVERRIDE` and `NANOLINK_CONF_FILE` are all in the root `docker-compose.yaml`.

All paths below are relative to the `cloudlynet_ai` repo root (the parent repo that carries this
submodule), and every command runs from there.

## 1. Prerequisites

- Clone `CloudlyIO/cloudlynet_ai` with submodules (`git submodule update --init --recursive`).
  `submodule/cloudlynet_edgeagent` must be on `main` at or after `39cddf2` (PR #4).
  Check: `submodule/cloudlynet_edgeagent/testsuite/FLEET.md` exists.
- Docker Desktop running, about 8 GB free for images.
- Root `.env`: `cp .env.example .env` (the root `.env` is untracked). Gateway and frontend `.env`
  files are bootstrapped from their `.env.example` by `compose.sh`; both default to local bypass
  mode (`DEV_BYPASS_JWT=true`), so no Cognito login is needed.
- `curl` and `jq` on the host.

## 2. Bring the fleet up

```bash
./scripts/compose.sh up --femtofleet              # add --frontend for the dashboard on :3000
```

That starts infra + apps + the `edgeagent` profile, then runs `scripts/femtofleet.sh`, which waits
for the gateway and smo-sim, resolves the fleet conf, and starts the agent, the testsuite (the 14
mock devices), the FTP server and its one-shot init.

**The testsuite container *is* the fleet**, not a test harness. `acsftp` mode disables only the
mock *cloud*; the mock *devices* are constructed and launched before the mode branch. Without this
container the agent has zero devices.

The conf is committed at `scripts/femtofleet.conf`: 14 devices, synthetic serials `2205610001` to
`2205610014`, PCI 101–114, cell identity 10000001–10000014, RS power −10 dBm. Use your own with
`./scripts/femtofleet.sh --conf path/to/fleet.conf` or `$FEMTOFLEET_CONF`. The serials and cell ids
are placeholders by convention: **never put real device serials or real cell ids in a tracked
file.**

### Enrolment: nothing to mint

The agent enrols with the token baked into `docker-compose.yaml` as the default for
`CLOUDLYNET_ENROLLMENT_TOKEN`, which maps to the seeded local edge. You do not need to create an
edge or copy a token.

> **Do not mint a new edge to "start clean".** A device is pinned to the **first** edge that
> reported it: smo-sim's upsert is `ON CONFLICT (tenant_id, cwmp_id) DO UPDATE SET ...` with
> `edge_id` deliberately absent from the SET list (`ingest_service.py:57`). On a machine where this
> fleet has run before, a new edge therefore stays **empty** while the agent authenticates as it
> and the 14 devices keep reporting through the old edge. `:9100/health` still shows 14/14 and
> commands route to an agent that is not running — every check passes while nothing works.
>
> `./scripts/femtofleet.sh --mint [--edge NAME]` exists for a genuinely fresh tenant. It warns
> before walking into the above.

## 3. Verify

Agent-side — all 14 must have sent an Inform (allow ~2 minutes; devices start 700 ms apart and the
agent onboards them on its heartbeat):

```bash
curl -s localhost:9100/health | jq '.fleet | {expected, informed}'    # -> 14 / 14
```

Platform-side — the rows the agent onboarded:

```bash
docker exec -i postgres sh -c 'psql -U "$POSTGRES_USER" -d maveric' <<'SQL'
select cwmp_id, health, op_state, last_inform_at from nanolink_devices order by cwmp_id;
SQL
```

Expect 14 rows with `health = healthy`, `op_state = t` and `last_inform_at` inside the last minute.

Confirm the log push after two minutes — rows such as `FILE_TRANS / ftp_upload_ok` and
`TR69 / autonomous_transfer_complete` mean the FTP loop and the log parser both work end to end:

```bash
docker exec -i postgres sh -c 'psql -U "$POSTGRES_USER" -d maveric' <<'SQL'
select module, event_type, count(*) from device_events
where created_at > now() - interval '5 minutes'
group by 1, 2 order by 3 desc limit 8;
SQL
```

With `--frontend` the same devices appear on `localhost:3000/devices`.

## 4. What the fleet pushes, with no manual trigger

| Stream | Path | Lands in | Cadence |
| --- | --- | --- | --- |
| Inform + config snapshot | CWMP session to the agent ACS | `nanolink_devices`, `device_config_snapshots` | ~500 ms per device |
| PM SampleSet values | agent telemetry over `/v1/agent/**` | `device_kpis` | continuous |
| `Log_*.gz`, `ErrorLog_*.gz` | FTP upload → agent watch dir → log parser | `device_events` (`FILE_TRANS`, `TR69`, `SON`, …) | 60 s per device |

The gateway's `/v1/agent/**` budget defaults to 120 req/min, sized for one device. `--femtofleet`
raises it to 900 via `AGENT_RATE_LIMIT_PER_MIN`; without that, 14 devices behind one agent IP make
TR-069 command applies time out and roll back as a false FAIL.

## 5. Reset

```bash
./scripts/femtofleet.sh --reset
```

Removes the four fleet containers, drops the agent buffer volume, deletes the minted edge if there
is one, and clears the token cache. It prints exactly what it will remove and asks first (`--yes`
to skip the prompt).

**Dropping the buffer volume is the part that matters.** The agent keeps its CWMP store there and
re-announces every device it remembers on the first heartbeat after a new token, so a
containers-only teardown brings the old devices straight back.

## 6. Gotchas

- **`docker compose down` is stack-wide — `--profile` does NOT scope it.** Earlier revisions of
  this file told you to run `docker compose --profile edgeagent down` to "stop the fleet only".
  That is wrong and it will stop your whole stack: measured with `--dry-run`, it removes 13
  containers including postgres, redis, mongo, kafka, zookeeper, minio and pgadmin (adding
  `--remove-orphans` changes nothing — the count is identical). To stop only the fleet, use
  `./scripts/femtofleet.sh --reset`, or name the services:
  `docker compose --profile edgeagent rm -sf cloudlynet-edgeagent cloudlynet-edgeagent-testsuite ftp ftp-init`.
  `./scripts/compose.sh down` stops the whole stack deliberately; add `-v` to wipe volumes.
- **Stopping the fleet keeps its rows.** The same cwmp_ids re-attach to the same rows on the next
  start, with a stale `last_inform_at` in between. No re-onboarding needed.
- **Fleet conf errors fail startup on purpose.** A missing or duplicate serial, or an unquoted
  number, stops the testsuite container;
  `docker logs maveric_platform_dev-cloudlynet-edgeagent-testsuite-1` names the offending entry.
- **Everything returns after a Docker restart** (`restart: unless-stopped`), fleet included. After
  an unclean shutdown check `docker ps` for `redis` in `Restarting`: `docker logs redis` showing
  `Bad file format reading the append only file` means the AOF is corrupt, which also keeps
  `copilot-backend` and `copilot-mcp-server` from starting. Fix: stop redis, run
  `redis-check-aof --fix appendonly.aof.manifest` inside `/data/appendonlydir` of the redis data
  volume (same image the stack runs; back the `*.incr.aof` file up first), start redis, then
  `./scripts/compose.sh up` again.
- **The FTP watch dir is pruned by the mock devices.** Each mock femtocell deletes its own uploads
  older than `NANOLINK_FTP_RETENTION` (default `6h`; `0` keeps everything) over FTP (`DELE`), so the
  volume holds about six hours of logs instead of growing without bound (it reached ~354,000 files
  / 1.4 GB in ~78 days before this). Files parked in a subdirectory are ignored by both the agent
  and the pruning.
- **An agent restart no longer replays the watch dir.** The agent records what it has ingested in
  `buffer.sqlite` (`ftp_ingested`) and resumes from it; `buffer_max_bytes` caps outbox payload
  rather than file size, so a large CWMP store or ledger can no longer wedge the outbox. Deleting
  `buffer.sqlite` makes the next start re-ingest the watch dir once (the cloud dedups).
- **The first start after upgrading replays the watch dir once.** An agent older than the ingest
  ledger leaves no `ftp_ingested` rows, so the first start of the new binary re-parses every
  `Log_*`/`ErrorLog_*` file in the watch dir. On a watch dir that was never pruned (the
  ~354,000-file case) that replay can overflow `buffer_max_bytes`, and the cap then evicts the
  oldest outbox rows, unsent telemetry included. Before upgrading an agent with a large unpruned
  watch dir, let its outbox drain, then prune the old uploads or move them into a subdirectory of
  the watch dir (the agent skips directories).

## 7. Volume names

Compose prefixes volumes with the **project name**, which is the directory the stack runs from —
`maveric_platform_dev`, not `cloudlynet_ai`. So the agent buffer is
`maveric_platform_dev_edgeagent_buffer`. Earlier revisions of this file named
`cloudlynet_ai_edgeagent_buffer`, which does not exist: `docker volume rm` on it fails, the reset
silently does nothing, and the remembered devices come back. Derive it rather than typing it:

```bash
docker volume ls --filter name=edgeagent_buffer
```

## 8. Manual path (fallback)

Only needed if you are debugging the flag itself. Mint an edge and start the profile by hand:

```bash
TID=00000000-0000-0000-3029-000000000001
curl -s -X POST localhost:8080/v1/tenants/$TID/custom/nybsys/edge-devices \
  -H 'Content-Type: application/json' -d '{"name":"femto-fleet-local"}' | jq .data
# -> { "edge_id": "...", "name": "...", "enrollment_token": "eyJ..." }   token shown ONCE
```

Then export `CLOUDLYNET_ENROLLMENT_TOKEN`, `NANOLINK_CONF_FILE`, `EDGEAGENT_TESTSUITE_MODE=acsftp`,
`CLOUDLYNET_EDGE_BASE_URL=http://maveric_gateway:8080`, an empty `EDGEAGENT_CWMP_CR_URL` and
`AGENT_RATE_LIMIT_PER_MIN=900`, and run
`docker compose --profile apps --profile edgeagent up -d`.

Calling the create endpoint again makes a **second** edge; remove extras with
`DELETE localhost:8080/v1/tenants/$TID/custom/nybsys/edge-devices/{edge_id}` (cascades that edge's
devices, KPIs and events). Re-read the pinning warning in §2 before minting anything.
