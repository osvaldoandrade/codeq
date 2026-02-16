import { Counter, Trend } from "k6/metrics";
import { sleep } from "k6";

import { claimTask, createTask, submitResult } from "./lib/codeq.js";
import { COMMANDS, envInt, envStr } from "./lib/config.js";

const RATE = envInt("RATE", 1000);
const DURATION = envStr("DURATION", "5m");
const COMMAND = envStr("COMMAND", COMMANDS[0]);

const WORKER_VUS = envInt("WORKER_VUS", 200);
const LEASE_SECONDS = envInt("LEASE_SECONDS", 60);
const WAIT_SECONDS = envInt("WAIT_SECONDS", 0);

const HIGH_PCT = envInt("HIGH_PCT", 50);
const MED_PCT = envInt("MED_PCT", 30);
const LOW_PCT = envInt("LOW_PCT", 20);

const HIGH_PRIORITY = envInt("HIGH_PRIORITY", 10);
const MED_PRIORITY = envInt("MED_PRIORITY", 5);
const LOW_PRIORITY = envInt("LOW_PRIORITY", 0);

const queueWaitMs = new Trend("queue_wait_ms");
const tasksClaimed = new Counter("tasks_claimed_total");

function pickPriority() {
  const total = Math.max(1, HIGH_PCT + MED_PCT + LOW_PCT);
  const r = Math.random() * total;
  if (r < HIGH_PCT) return HIGH_PRIORITY;
  if (r < HIGH_PCT + MED_PCT) return MED_PRIORITY;
  return LOW_PRIORITY;
}

export const options = {
  scenarios: {
    producer: {
      executor: "constant-arrival-rate",
      exec: "producer",
      rate: RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: envInt("PRODUCER_PREALLOC_VUS", Math.max(50, Math.ceil(RATE / 20))),
      maxVUs: envInt("PRODUCER_MAX_VUS", Math.max(100, Math.ceil(RATE / 10))),
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
  const priority = pickPriority();
  createTask({
    command: COMMAND,
    priority,
    payload: { source: "k6", scenario: "mixed_priorities", priority, vu: __VU, iter: __ITER },
    tags: { scenario: "mixed_priorities", priority: String(priority) },
  });
}

export function worker() {
  const task = claimTask({
    commands: [COMMAND],
    leaseSeconds: LEASE_SECONDS,
    waitSeconds: WAIT_SECONDS,
    tags: { scenario: "mixed_priorities" },
  });
  if (!task) {
    sleep(0.05);
    return;
  }

  if (task.createdAt) {
    const created = Date.parse(task.createdAt);
    if (!Number.isNaN(created)) {
      queueWaitMs.add(Date.now() - created, { priority: String(task.priority ?? 0) });
    }
  }
  tasksClaimed.add(1, { priority: String(task.priority ?? 0) });

  submitResult({
    taskId: task.id,
    status: "COMPLETED",
    result: { ok: true, scenario: "mixed_priorities" },
    tags: { scenario: "mixed_priorities", priority: String(task.priority ?? 0) },
  });
}

