#!/usr/bin/env bash
# End-to-end test runner for the NanoLink emulator (issue cloudlynet_ai#346).
#
# Brings the Docker Compose stack up for a scenario, polls the mock cloud's
# /health until "ok":true, reports what was verified, then tears the stack down.
#
# Usage:
#   scripts/e2e.sh [scenario]          # one scenario (default: happy)
#   scripts/e2e.sh --all               # every scenario; non-zero exit if any fail
#   scripts/e2e.sh --all --verbose     # per-check breakdown (also: VERBOSE=1)
set -uo pipefail

# Run against the test-harness compose file regardless of the caller's CWD.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"
export COMPOSE_FILE="$REPO_ROOT/docker-compose.test.yml"

PORT="${EDGEAGENT_TESTSUITE_PORT:-9000}"
MAX_POLLS=60   # x2s => up to ~120s per scenario
VERBOSE="${VERBOSE:-0}"
SCENARIOS="happy ftp-path-reject ftp-auth-fail ftp-conn-fail ftp-timeout atc-fault reboot"

# One-line human description of what each scenario verifies.
desc() {
  case "$1" in
    happy)           echo "upload OK (curl 0) -> ftp_upload_ok" ;;
    ftp-path-reject) echo "STOR denied at /uploads (curl 25) -> ftp_upload_path_reject" ;;
    ftp-auth-fail)   echo "bad login (curl 67) -> ftp_auth_fail" ;;
    ftp-conn-fail)   echo "conn failure (curl 7) -> ftp_conn_fail" ;;
    ftp-timeout)     echo "upload timeout (curl 28) -> ftp_upload_timeout" ;;
    atc-fault)       echo "TR-069 RPC-unknown -> atc_fault_loop" ;;
    reboot)          echo "FM reboot alarm -> device_reboot" ;;
    *)               echo "$1" ;;
  esac
}

# curl code the scenario reproduces live over the wire (empty = content-only).
probe_code() {
  case "$1" in
    ftp-path-reject) echo 25 ;;
    ftp-auth-fail)   echo 67 ;;
    *)               echo "" ;;
  esac
}

# jget <json> <key> -> scalar value (bare; true/false/int/string). jq-free.
jget() {
  printf '%s' "$1" | grep -oE "\"$2\":(true|false|-?[0-9]+|\"[^\"]*\")" | head -1 \
    | sed -E 's/^"[^"]*"://; s/^"(.*)"$/\1/'
}
num() { local v; v="$(jget "$1" "$2")"; case "$v" in '' | *[!0-9-]*) echo 0 ;; *) echo "$v" ;; esac; }
acks_count() { printf '%s' "$1" | grep -o '"applied"' | wc -l | tr -d ' '; }
ck() { printf '   %-34s %-3s %s\n' "$1" "$2" "$3"; }
mark() { [ "$1" = 1 ] && echo "ok" || echo "XX"; }

# health_ok <json> -> 0 if "ok":true
health_ok() { printf '%s' "$1" | grep -q '"ok":true'; }

verbose_block() { # idx total scenario json pass
  local idx="$1" tot="$2" s="$3" h="$4" pass="$5" pc reg snp ack fail ev canon expd saw
  reg=$(num "$h" registered); snp=$(num "$h" snapshot_params); ack=$(acks_count "$h")
  fail=$(num "$h" failures); ev=$(num "$h" events)
  canon=$(jget "$h" typed_event_on_canonical); expd=$(jget "$h" expected_event); saw=$(jget "$h" saw_expected_event)
  echo "── [$idx/$tot] $s ─────────────────────────────────────"
  echo " verifies: $(desc "$s")"
  ck "agent registered with cloud"     "$(mark "$([ "$reg" -gt 0 ] && echo 1 || echo 0)")"  "registered=$reg"
  ck "CWMP session reached ACS turn"   "$(mark "$([ "$ack" -gt 0 ] && echo 1 || echo 0)")"  "acks=$ack (ATC answered, no Fault)"
  ck "config snapshot"                 "$(mark "$([ "$snp" -eq 24 ] && echo 1 || echo 0)")" "snapshot_params=$snp"
  ck "commands applied + acked"        "$(mark "$([ "$ack" -ge 3 ] && echo 1 || echo 0)")"  "acks=$ack"
  ck "outbox retry exercised"          "$(mark "$([ "$fail" -gt 0 ] && echo 1 || echo 0)")" "failures=$fail"
  ck "real FTP upload ingested"        "$(mark "$([ "$ev" -gt 0 ] && echo 1 || echo 0)")"   "events=$ev"
  ck "scenario signal classified"      "$(mark "$([ "$saw" = true ] && echo 1 || echo 0)")" "expected_event=$expd"
  ck "event keyed to real device"      "$(mark "$([ "$canon" = true ] && echo 1 || echo 0)")" "typed_event_on_canonical=$canon"
  pc="$(probe_code "$s")"
  if [ -n "$pc" ]; then
    if docker compose logs cloudlynet-edgeagent-testsuite 2>/dev/null | grep -q "curl: ($pc)"; then
      ck "transport fault reproduced" ok "curl: ($pc) in device logs"
    else
      ck "transport fault reproduced" "XX" "curl: ($pc) not observed"
    fi
  fi
  [ "$pass" = 1 ] && echo " => PASS" || { echo " => FAIL"; echo "    raw: ${h:-<no /health>}"; }
}

run_one() { # idx total scenario -> 0 pass / 1 fail
  local idx="$1" tot="$2" s="$3" h="" i pass=0
  # --env-file /dev/null: this IS the self-contained mock-cloud gate — ignore any
  # user .env (e.g. an acsftp/real-cloud one that sets CLOUDLYNET_BASE_URL/token),
  # so the agent uses the compose defaults (mock cloud :9000, full mode, dev token).
  docker compose --env-file /dev/null down -v >/dev/null 2>&1 || true
  if ! EDGEAGENT_TESTSUITE_SCENARIO="$s" docker compose --env-file /dev/null up -d --build >/dev/null 2>&1; then
    printf ' [%s/%s] %-16s FAIL   docker compose up failed\n' "$idx" "$tot" "$s"
    return 1
  fi
  for ((i = 1; i <= MAX_POLLS; i++)); do
    sleep 2
    h="$(curl -fsS "http://localhost:${PORT}/health" 2>/dev/null || true)"
    health_ok "$h" && break
  done
  health_ok "$h" && pass=1
  if [ "$VERBOSE" = 1 ]; then
    verbose_block "$idx" "$tot" "$s" "$h" "$pass"
  elif [ "$pass" = 1 ]; then
    printf ' [%s/%s] %-16s PASS   %s\n' "$idx" "$tot" "$s" "$(desc "$s")"
  else
    printf ' [%s/%s] %-16s FAIL   %s\n' "$idx" "$tot" "$s" "$(desc "$s")"
    printf '        /health: %s\n' "${h:-<no response after $((MAX_POLLS * 2))s>}"
  fi
  return $((1 - pass))
}

is_valid() { case " $SCENARIOS " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

footer() {
  echo
  echo " every run also verifies: CWMP onboard · ATC answered without Fault · 24-param snapshot ·"
  echo "                          3 commands applied+acked · real FTP ingest · event on canonical device"
}

main() {
  local arg="--all" want=""
  for a in "$@"; do
    case "$a" in
      -v | --verbose) VERBOSE=1 ;;
      --all) want="--all" ;;
      -*) echo "unknown flag: $a" >&2; return 2 ;;
      *) want="$a" ;;
    esac
  done
  [ -n "$want" ] && arg="$want"

  if [ "$arg" = "--all" ]; then
    local total failed=0 idx=0 s
    total=$(printf '%s\n' $SCENARIOS | wc -w | tr -d ' ')
    echo "NanoLink emulator · end-to-end ($total scenarios)"
    echo
    for s in $SCENARIOS; do
      idx=$((idx + 1))
      run_one "$idx" "$total" "$s" || failed=$((failed + 1))
    done
    docker compose --env-file /dev/null down -v >/dev/null 2>&1 || true
    footer
    echo " ─────────────────────────────────────────────────"
    if [ "$failed" -eq 0 ]; then
      echo " RESULT: $total/$total PASS"
      return 0
    fi
    echo " RESULT: $failed/$total FAILED"
    return 1
  fi

  if ! is_valid "$arg"; then
    echo "unknown scenario: $arg" >&2
    echo "valid: $SCENARIOS  (or --all)" >&2
    return 2
  fi
  local rc=0
  run_one 1 1 "$arg" || rc=1
  docker compose --env-file /dev/null down -v >/dev/null 2>&1 || true
  return $rc
}

main "$@"
