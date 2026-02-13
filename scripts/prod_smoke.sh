#!/usr/bin/env bash
set -euo pipefail

CODEQ_BIN="${CODEQ_BIN:-./codeq}"
BASE_URL="${CODEQ_BASE_URL:-https://api.storifly.ai}"
IAM_BASE_URL="${CODEQ_IAM_BASE_URL:-https://api.storifly.ai/v1/accounts}"
IAM_API_KEY="${CODEQ_IAM_API_KEY:-}"
EMAIL="${CODEQ_EMAIL:-}"
PASSWORD="${CODEQ_PASSWORD:-}"
EVENT="${CODEQ_EVENT:-render_video}"
PRIORITY="${CODEQ_PRIORITY:-10}"
PAYLOAD="${CODEQ_PAYLOAD:-{\"jobId\":500}}"
WORKER_ACK="${CODEQ_WORKER_ACK:-complete}"
WORKER_SECS="${CODEQ_WORKER_SECS:-5}"
WORKER_CONCURRENCY="${CODEQ_WORKER_CONCURRENCY:-1}"
QUEUE_INSPECT="${CODEQ_QUEUE_INSPECT:-0}"

if [ ! -x "$CODEQ_BIN" ]; then
  if command -v codeq >/dev/null 2>&1; then
    CODEQ_BIN="codeq"
  else
    echo "[ERROR] codeq binary not found. Set CODEQ_BIN or build ./codeq." >&2
    exit 1
  fi
fi

if [ -z "$IAM_API_KEY" ]; then
  read -r -p "IAM API key: " IAM_API_KEY
fi
if [ -z "$EMAIL" ]; then
  read -r -p "Email: " EMAIL
fi
if [ -z "$PASSWORD" ]; then
  read -r -s -p "Password: " PASSWORD
  echo
fi

echo "[codeq] init"
"$CODEQ_BIN" init --no-prompt \
  --base-url "$BASE_URL" \
  --iam-base-url "$IAM_BASE_URL" \
  --iam-api-key "$IAM_API_KEY"

echo "[codeq] auth login"
"$CODEQ_BIN" auth login --no-prompt --email "$EMAIL" --password "$PASSWORD"

echo "[codeq] task create"
create_out=$("$CODEQ_BIN" task create --event "$EVENT" --priority "$PRIORITY" --payload "$PAYLOAD")
echo "$create_out"
task_id="$(echo "$create_out" | awk '{print $NF}')"

echo "[codeq] worker start (auto-stop after ${WORKER_SECS}s)"
"$CODEQ_BIN" worker start \
  --events "$EVENT" \
  --concurrency "$WORKER_CONCURRENCY" \
  --ack "$WORKER_ACK" \
  --wait-seconds 0 \
  --lease-seconds 60 &
worker_pid=$!

cleanup() {
  if kill -0 "$worker_pid" >/dev/null 2>&1; then
    kill "$worker_pid" >/dev/null 2>&1 || true
    wait "$worker_pid" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

sleep "$WORKER_SECS"

if [ -n "$task_id" ]; then
  echo "[codeq] task result $task_id"
  "$CODEQ_BIN" task result "$task_id"
fi

if [ "$QUEUE_INSPECT" = "1" ]; then
  echo "[codeq] queue inspect $EVENT"
  "$CODEQ_BIN" queue inspect "$EVENT" || true
else
  echo "[codeq] queue inspect skipped (admin only)"
fi
