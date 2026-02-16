import { createTask, getQueueStats } from "./lib/codeq.js";
import { COMMANDS, envInt, envStr } from "./lib/config.js";

const TASKS = envInt("TASKS", 100000);
const VUS = envInt("VUS", 200);
const MAX_DURATION = envStr("MAX_DURATION", "30m");

const COMMAND = envStr("COMMAND", COMMANDS[0]);
const PRIORITY = envInt("PRIORITY", 0);

export const options = {
  scenarios: {
    prefill: {
      executor: "shared-iterations",
      vus: VUS,
      iterations: TASKS,
      maxDuration: MAX_DURATION,
    },
  },
  thresholds: {
    "http_req_failed{endpoint:create}": ["rate<0.01"],
  },
};

export default function () {
  createTask({
    command: COMMAND,
    priority: PRIORITY,
    payload: { source: "k6", scenario: "prefill_queue", vu: __VU, iter: __ITER },
    tags: { scenario: "prefill_queue" },
  });
}

export function teardown() {
  const stats = getQueueStats({ command: COMMAND, tags: { scenario: "prefill_queue" } });
  if (!stats) return;
  console.log(
    `[queue_stats] command=${COMMAND} ready=${stats.ready} delayed=${stats.delayed} inProgress=${stats.inProgress} dlq=${stats.dlq}`
  );
}

