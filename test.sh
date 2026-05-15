#!/usr/bin/env bash
# Smoke + micro-benchmark for a running codeQ server.
#
# Usage:
#   ./test.sh                   # 50 tasks, default settings
#   N=200 ./test.sh             # 200 tasks
#   BASE_URL=http://host:8080 PRODUCER_TOKEN=... WORKER_TOKEN=... ./test.sh
#
# Assumes the codeq install bundle's dev profile is running (static "dev-token"
# for both producer and worker). Override via env vars for other deployments.
#
# Requires: bash, curl, python3 (used as a JSON helper instead of jq).

set -euo pipefail

BASE_URL="${BASE_URL:-${CODEQ_BASE_URL:-http://localhost:8080}}"
PRODUCER_TOKEN="${PRODUCER_TOKEN:-${CODEQ_PRODUCER_TOKEN:-dev-token}}"
WORKER_TOKEN="${WORKER_TOKEN:-${CODEQ_WORKER_TOKEN:-dev-token}}"
COMMAND="${COMMAND:-GENERATE_MASTER}"
N="${N:-50}"
WORKER_ID="${WORKER_ID:-worker-$$}"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
red()  { printf '\033[31m%s\033[0m\n' "$*"; }
grn()  { printf '\033[32m%s\033[0m\n' "$*"; }
ylw()  { printf '\033[33m%s\033[0m\n' "$*"; }

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || { red "missing required command: $1"; exit 127; }
}

require_cmd curl
require_cmd python3

# Tiny JSON helpers (python stdin → stdout) ----------------------------------
json_extract() {
    local field="$1"
    python3 -c "import json,sys;d=json.load(sys.stdin); v=d.get('$field',''); print('' if v is None else v)"
}

bold "==> codeQ smoke test"
echo "  base url        : $BASE_URL"
echo "  producer token  : $(printf '%.6s***' "$PRODUCER_TOKEN")"
echo "  worker token    : $(printf '%.6s***' "$WORKER_TOKEN")"
echo "  command         : $COMMAND"
echo "  tasks           : $N"
echo "  worker id       : $WORKER_ID"
echo

# 1. Liveness ----------------------------------------------------------------
bold "==> health"
if ! curl -fsS "$BASE_URL/metrics" >/dev/null; then
    red "server not reachable at $BASE_URL"
    exit 1
fi
grn "ok"
echo

# 2. Create N tasks ----------------------------------------------------------
bold "==> creating $N tasks"
created_ids=()
start_create=$(date +%s.%N)
for ((i=1; i<=N; i++)); do
    body=$(printf '{"command":"%s","payload":{"i":%d},"priority":0}' "$COMMAND" "$i")
    resp=$(curl -fsS -X POST "$BASE_URL/v1/codeq/tasks" \
        -H "Authorization: Bearer $PRODUCER_TOKEN" \
        -H "Content-Type: application/json" \
        --data "$body")
    id=$(printf '%s' "$resp" | json_extract id)
    if [[ -z "$id" ]]; then
        red "create failed (response: $resp)"
        exit 1
    fi
    created_ids+=("$id")
done
end_create=$(date +%s.%N)
elapsed_create=$(awk -v s="$start_create" -v e="$end_create" 'BEGIN{printf "%.3f", e-s}')
rps_create=$(awk -v n="$N" -v t="$elapsed_create" 'BEGIN{ if (t>0) printf "%.1f", n/t; else print "n/a" }')
grn "created $N tasks in ${elapsed_create}s (~${rps_create} req/s)"
echo

# 3. Claim + complete each task ---------------------------------------------
bold "==> claiming + completing"
claim_body=$(printf '{"commands":["%s"],"leaseSeconds":60,"waitSeconds":0,"workerId":"%s"}' "$COMMAND" "$WORKER_ID")

claimed=0
completed=0
start_claim=$(date +%s.%N)
for ((i=1; i<=N; i++)); do
    resp=$(curl -fsS -X POST "$BASE_URL/v1/codeq/tasks/claim" \
        -H "Authorization: Bearer $WORKER_TOKEN" \
        -H "Content-Type: application/json" \
        --data "$claim_body" || echo '')
    id=$(printf '%s' "$resp" | json_extract id 2>/dev/null || true)
    if [[ -z "$id" ]]; then
        ylw "claim returned empty at iteration $i (queue may have drained)"
        break
    fi
    claimed=$((claimed+1))

    cbody=$(printf '{"workerId":"%s","status":"COMPLETED","result":{"ok":true}}' "$WORKER_ID")
    if curl -fsS -o /dev/null -X POST "$BASE_URL/v1/codeq/tasks/$id/result" \
        -H "Authorization: Bearer $WORKER_TOKEN" \
        -H "Content-Type: application/json" \
        --data "$cbody"; then
        completed=$((completed+1))
    else
        red "complete failed for $id"
    fi
done
end_claim=$(date +%s.%N)
elapsed_claim=$(awk -v s="$start_claim" -v e="$end_claim" 'BEGIN{printf "%.3f", e-s}')
rps_claim=$(awk -v n="$claimed" -v t="$elapsed_claim" 'BEGIN{ if (t>0) printf "%.1f", n/t; else print "n/a" }')
grn "claimed=$claimed completed=$completed in ${elapsed_claim}s (~${rps_claim} req/s)"
echo

# 4. Final summary ----------------------------------------------------------
bold "==> summary"
total=$(awk -v a="$elapsed_create" -v b="$elapsed_claim" 'BEGIN{printf "%.3f", a+b}')
echo "  created     : $N"
echo "  claimed     : $claimed"
echo "  completed   : $completed"
echo "  create time : ${elapsed_create}s (~${rps_create} req/s)"
echo "  drain time  : ${elapsed_claim}s (~${rps_claim} req/s)"
echo "  total time  : ${total}s"

if [[ "$completed" -ne "$N" ]]; then
    ylw "WARN: completed ($completed) != created ($N)"
    exit 2
fi
grn "OK"
