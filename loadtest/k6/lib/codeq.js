import http from "k6/http";
import { check } from "k6";

import { ADMIN_TOKEN, BASE_URL, PRODUCER_TOKEN, WORKER_TOKEN } from "./config.js";

function authHeaders(token, extra) {
  const h = {
    Authorization: `Bearer ${token}`,
  };
  if (extra) {
    for (const k in extra) h[k] = extra[k];
  }
  return h;
}

export function createTask({
  command,
  payload,
  priority = 0,
  maxAttempts,
  webhook,
  idempotencyKey,
  runAt,
  delaySeconds,
  token = PRODUCER_TOKEN,
  tags,
} = {}) {
  const body = {
    command,
    payload: payload ?? { k6: { vu: __VU, iter: __ITER, ts: Date.now() } },
    priority,
  };
  if (typeof maxAttempts === "number" && maxAttempts > 0) body.maxAttempts = maxAttempts;
  if (webhook) body.webhook = webhook;
  if (idempotencyKey) body.idempotencyKey = idempotencyKey;
  if (runAt) body.runAt = runAt;
  if (typeof delaySeconds === "number" && delaySeconds >= 0) body.delaySeconds = delaySeconds;

  const res = http.post(`${BASE_URL}/v1/codeq/tasks`, JSON.stringify(body), {
    headers: authHeaders(token, { "Content-Type": "application/json" }),
    tags: { endpoint: "create", command, ...(tags || {}) },
    responseType: "none",
  });

  check(res, { "create: 202": (r) => r.status === 202 });
  return res;
}

export function claimTask({
  commands,
  leaseSeconds,
  waitSeconds,
  token = WORKER_TOKEN,
  tags,
} = {}) {
  const body = {};
  if (Array.isArray(commands) && commands.length > 0) body.commands = commands;
  if (typeof leaseSeconds === "number" && leaseSeconds > 0) body.leaseSeconds = leaseSeconds;
  if (typeof waitSeconds === "number" && waitSeconds >= 0) body.waitSeconds = waitSeconds;

  const res = http.post(`${BASE_URL}/v1/codeq/tasks/claim`, JSON.stringify(body), {
    headers: authHeaders(token, { "Content-Type": "application/json" }),
    tags: { endpoint: "claim", ...(tags || {}) },
  });

  if (res.status === 204) return null;
  check(res, { "claim: 200": (r) => r.status === 200 });
  if (res.status !== 200) return null;
  return res.json();
}

export function submitResult({
  taskId,
  status = "COMPLETED",
  result,
  error,
  token = WORKER_TOKEN,
  tags,
} = {}) {
  const body = { status };
  if (status === "COMPLETED") {
    body.result = result ?? { ok: true };
  } else {
    body.error = error ?? "loadtest error";
  }

  const res = http.post(`${BASE_URL}/v1/codeq/tasks/${taskId}/result`, JSON.stringify(body), {
    headers: authHeaders(token, { "Content-Type": "application/json" }),
    tags: { endpoint: "result", ...(tags || {}) },
    responseType: "none",
  });

  check(res, { "result: 200": (r) => r.status === 200 });
  return res;
}

export function getQueueStats({ command, token = ADMIN_TOKEN, tags } = {}) {
  const res = http.get(`${BASE_URL}/v1/codeq/admin/queues/${command}`, {
    headers: authHeaders(token),
    tags: { endpoint: "queue_stats", command, ...(tags || {}) },
  });
  check(res, { "queue_stats: 200": (r) => r.status === 200 });
  if (res.status !== 200) return null;
  return res.json();
}

