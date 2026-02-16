import { Counter, Trend } from "k6/metrics";
import { sleep } from "k6";

import { claimTask, createTask, submitResult } from "./lib/codeq.js";
import { COMMANDS, envInt, envStr } from "./lib/config.js";

const RATE = envInt("RATE", 500);
const DURATION = envStr("DURATION", "5m");
const COMMAND = envStr("COMMAND", COMMANDS[0]);

const WORKER_VUS = envInt("WORKER_VUS", 200);
const LEASE_SECONDS = envInt("LEASE_SECONDS", 60);
const WAIT_SECONDS = envInt("WAIT_SECONDS", 0);

const DELAY_PCT = envInt("DELAY_PCT", 50);
const MIN_DELAY_SECONDS = envInt("MIN_DELAY_SECONDS", 1);
const MAX_DELAY_SECONDS = envInt("MAX_DELAY_SECONDS", 30);

const delayedCreated = new Counter("delayed_tasks_created_total");
const immediateCreated = new Counter("immediate_tasks_created_total");
const queueWaitMs = new Trend("queue_wait_ms");

function pickDelaySeconds() {
  const min = Math.max(0, MIN_DELAY_SECONDS);
  const max = Math.max(min, MAX_DELAY_SECONDS);
  return min + Math.floor(Math.random() * (max - min + 1));
}

export const options = {
  scenarios: {
    producer: {
      executor: "constant-arrival-rate",
      exec: "producer",
      rate: RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: envInt("PRODUCER_PREALLOC_VUS", Math.max(50, Math.ceil(RATE / 10))),
      maxVUs: envInt("PRODUCER_MAX_VUS", Math.max(100, Math.ceil(RATE / 5))),
    },
    worker: {
      executor: "constant-vus",
      exec: "worker",
      vus: WORKER_VUS,
      duration: DURATION,
    },
  },
  thresholds: {
    "http_req_failed{endpoint:create}": ["rate<0.01"],
    "http_req_failed{endpoint:claim}": ["rate<0.01"],
    "http_req_failed{endpoint:result}": ["rate<0.01"],
  },
};

export function producer() {
  const r = Math.random() * 100;
  const isDelayed = r < DELAY_PCT;
  const delaySeconds = isDelayed ? pickDelaySeconds() : undefined;
  if (isDelayed) delayedCreated.add(1);
  else immediateCreated.add(1);

  createTask({
    command: COMMAND,
    delaySeconds,
    payload: { source: "k6", scenario: "delayed_tasks", delayed: isDelayed, delaySeconds, vu: __VU, iter: __ITER },
    tags: { scenario: "delayed_tasks", delayed: String(isDelayed) },
  });
}

export function worker() {
  const task = claimTask({
    commands: [COMMAND],
    leaseSeconds: LEASE_SECONDS,
    waitSeconds: WAIT_SECONDS,
    tags: { scenario: "delayed_tasks" },
  });
  if (!task) {
    sleep(0.1);
    return;
  }
  let isDelayed = false;
  if (task.payload) {
    try {
      const p = JSON.parse(task.payload);
      isDelayed = !!p.delayed;
    } catch (_) {
      // ignore malformed payloads
    }
  }
  if (task.createdAt) {
    const created = Date.parse(task.createdAt);
    if (!Number.isNaN(created)) {
      queueWaitMs.add(Date.now() - created, { delayed: String(isDelayed) });
    }
  }
  submitResult({
    taskId: task.id,
    status: "COMPLETED",
    result: { ok: true, scenario: "delayed_tasks" },
    tags: { scenario: "delayed_tasks" },
  });
}
