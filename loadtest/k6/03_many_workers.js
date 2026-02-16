import { sleep } from "k6";

import { claimTask, createTask, submitResult } from "./lib/codeq.js";
import { COMMANDS, envInt, envStr } from "./lib/config.js";

const PRODUCER_RATE = envInt("PRODUCER_RATE", 800);
const DURATION = envStr("DURATION", "5m");
const COMMAND = envStr("COMMAND", COMMANDS[0]);

const WORKER_VUS = envInt("WORKER_VUS", 120);
const LEASE_SECONDS = envInt("LEASE_SECONDS", 60);
const WAIT_SECONDS = envInt("WAIT_SECONDS", 0);

const PRODUCER_PREALLOC_VUS = envInt("PRODUCER_PREALLOC_VUS", Math.max(50, Math.ceil(PRODUCER_RATE / 20)));
const PRODUCER_MAX_VUS = envInt("PRODUCER_MAX_VUS", PRODUCER_PREALLOC_VUS * 2);

export const options = {
  scenarios: {
    producer: {
      executor: "constant-arrival-rate",
      exec: "producer",
      rate: PRODUCER_RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: PRODUCER_PREALLOC_VUS,
      maxVUs: PRODUCER_MAX_VUS,
    },
    many_workers: {
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
  createTask({
    command: COMMAND,
    payload: { source: "k6", scenario: "many_workers", vu: __VU, iter: __ITER },
    tags: { scenario: "many_workers" },
  });
}

export function worker() {
  const task = claimTask({
    commands: [COMMAND],
    leaseSeconds: LEASE_SECONDS,
    waitSeconds: WAIT_SECONDS,
    tags: { scenario: "many_workers" },
  });
  if (!task) {
    sleep(0.05);
    return;
  }
  submitResult({
    taskId: task.id,
    status: "COMPLETED",
    result: { ok: true, scenario: "many_workers" },
    tags: { scenario: "many_workers" },
  });
}

