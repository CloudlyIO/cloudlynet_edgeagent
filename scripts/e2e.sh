#!/usr/bin/env bash
# End-to-end test runner for the NanoLink emulator (issue cloudlynet_ai#346).
#
# Brings the Docker Compose stack up for a scenario, polls the mock cloud's
# /health until "ok":true, prints PASS/FAIL, then tears the stack down.
#
# Usage:
#   scripts/e2e.sh [scenario]   # one scenario (default: happy)
#   scripts/e2e.sh --all        # every scenario; non-zero exit if any fail
set -uo pipefail

# Run against the root compose file regardless of the caller's CWD.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PORT="${EDGEAGENT_TESTSUITE_PORT:-9000}"
MAX_POLLS=60   # x2s => up to ~120s per scenario
SCENARIOS=(happy ftp-path-reject ftp-auth-fail ftp-conn-fail ftp-timeout atc-fault reboot)

run_one() {
  local s="$1" h="" i
  echo "── $s ──────────────────────────────────────────"
  docker compose down -v >/dev/null 2>&1 || true
  if ! EDGEAGENT_TESTSUITE_SCENARIO="$s" docker compose up -d --build >/dev/null 2>&1; then
    echo "FAIL  $s  (docker compose up failed)"
    return 1
  fi
  for ((i = 1; i <= MAX_POLLS; i++)); do
    sleep 2
    h="$(curl -fsS "http://localhost:${PORT}/health" 2>/dev/null || true)"
    printf '%s' "$h" | grep -q '"ok":true' && break
  done
  if printf '%s' "$h" | grep -q '"ok":true'; then
    echo "PASS  $s"
    printf '%s\n' "$h" | (jq -c '{ok,expected_event,saw_expected_event,typed_events}' 2>/dev/null || cat)
    return 0
  fi
  echo "FAIL  $s  (/health never reached ok:true after $((MAX_POLLS * 2))s)"
  printf '%s\n' "${h:-<no /health response>}"
  docker compose logs --tail=20 cloudlynet-edgeagent-testsuite 2>/dev/null || true
  return 1
}

is_valid() {
  local x
  for x in "${SCENARIOS[@]}"; do [[ "$x" == "$1" ]] && return 0; done
  return 1
}

main() {
  local arg="${1:-happy}" rc=0

  if [[ "$arg" == "--all" ]]; then
    local failed=()
    for s in "${SCENARIOS[@]}"; do
      run_one "$s" || failed+=("$s")
    done
    docker compose down -v >/dev/null 2>&1 || true
    echo "═════════════════════════════════════════════════"
    if ((${#failed[@]})); then
      echo "RESULT: FAIL — ${#failed[@]}/${#SCENARIOS[@]} failed: ${failed[*]}"
      rc=1
    else
      echo "RESULT: PASS — all ${#SCENARIOS[@]} scenarios ok"
    fi
    return $rc
  fi

  if ! is_valid "$arg"; then
    echo "unknown scenario: $arg" >&2
    echo "valid: ${SCENARIOS[*]}  (or --all)" >&2
    return 2
  fi
  run_one "$arg" || rc=1
  docker compose down -v >/dev/null 2>&1 || true
  return $rc
}

main "$@"
