import { sleep } from "k6";

import { claimTask, createTask, submitResult } from "./lib/codeq.js";
import { COMMANDS, envInt, envStr } from "./lib/config.js";

// 10k in 10s => 1000 tasks/sec (default).
const RATE = envInt("RATE", 1000);
const BURST_DURATION = envStr("BURST_DURATION", "10s");
const DRAIN_DURATION = envStr("DRAIN_DURATION", "2m");
const COMMAND = envStr("COMMAND", COMMANDS[0]);

const WORKER_VUS = envInt("WORKER_VUS", 200);
const LEASE_SECONDS = envInt("LEASE_SECONDS", 60);
const WAIT_SECONDS = envInt("WAIT_SECONDS", 0);

const PRODUCER_PREALLOC_VUS = envInt("PRODUCER_PREALLOC_VUS", Math.max(50, Math.ceil(RATE / 20)));
const PRODUCER_MAX_VUS = envInt("PRODUCER_MAX_VUS", PRODUCER_PREALLOC_VUS * 2);

export const options = {
  scenarios: {
    burst_producer: {
      executor: "constant-arrival-rate",
      exec: "producer",
      rate: RATE,
      timeUnit: "1s",
      duration: BURST_DURATION,
      preAllocatedVUs: PRODUCER_PREALLOC_VUS,
      maxVUs: PRODUCER_MAX_VUS,
    },
    drain_workers: {
      executor: "constant-vus",
      exec: "worker",
      vus: WORKER_VUS,
      duration: DRAIN_DURATION,
    },
  },
  thresholds: {
    "http_req_failed{endpoint:create}": ["rate<0.01"],
    "http_req_failed{endpoint:claim}": ["rate<0.01"],
    "http_req_failed{endpoint:result}": ["rate<0.01"],
  },
};

export function producer() {
  createTask({
    command: COMMAND,
    payload: { source: "k6", scenario: "burst_10k_10s", vu: __VU, iter: __ITER },
    tags: { scenario: "burst_10k_10s" },
  });
}

export function worker() {
  const task = claimTask({
    commands: [COMMAND],
    leaseSeconds: LEASE_SECONDS,
    waitSeconds: WAIT_SECONDS,
    tags: { scenario: "burst_10k_10s" },
  });
  if (!task) {
    sleep(0.05);
    return;
  }
  submitResult({
    taskId: task.id,
    status: "COMPLETED",
    result: { ok: true, scenario: "burst_10k_10s" },
    tags: { scenario: "burst_10k_10s" },
  });
}

