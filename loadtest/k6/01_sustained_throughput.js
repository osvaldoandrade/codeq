import { sleep } from "k6";

import { claimTask, createTask, submitResult } from "./lib/codeq.js";
import { COMMANDS, envInt, envStr } from "./lib/config.js";

const RATE = envInt("RATE", 500); // tasks/sec
const DURATION = envStr("DURATION", "5m");
const COMMAND = envStr("COMMAND", COMMANDS[0]);

const WORKER_VUS = envInt("WORKER_VUS", 100);
const LEASE_SECONDS = envInt("LEASE_SECONDS", 60);
const WAIT_SECONDS = envInt("WAIT_SECONDS", 0);

const PRODUCER_PREALLOC_VUS = envInt("PRODUCER_PREALLOC_VUS", Math.max(20, Math.ceil(RATE / 20)));
const PRODUCER_MAX_VUS = envInt("PRODUCER_MAX_VUS", PRODUCER_PREALLOC_VUS * 2);

const CLAIM_P99_MS = envInt("CLAIM_P99_MS", 100);

export const options = {
  scenarios: {
    producer: {
      executor: "constant-arrival-rate",
      exec: "producer",
      rate: RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: PRODUCER_PREALLOC_VUS,
      maxVUs: PRODUCER_MAX_VUS,
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
    "http_req_duration{endpoint:claim}": [`p(99)<${CLAIM_P99_MS}`],
  },
};

export function producer() {
  createTask({
    command: COMMAND,
    payload: { source: "k6", scenario: "sustained", vu: __VU, iter: __ITER },
    tags: { scenario: "sustained" },
  });
}

export function worker() {
  const task = claimTask({
    commands: [COMMAND],
    leaseSeconds: LEASE_SECONDS,
    waitSeconds: WAIT_SECONDS,
    tags: { scenario: "sustained" },
  });
  if (!task) {
    sleep(0.05);
    return;
  }
  submitResult({
    taskId: task.id,
    status: "COMPLETED",
    result: { ok: true, scenario: "sustained" },
    tags: { scenario: "sustained" },
  });
}

