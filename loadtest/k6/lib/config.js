export function envStr(name, defVal) {
  const v = __ENV[name];
  if (v === undefined || v === null) return defVal;
  const s = String(v).trim();
  return s === "" ? defVal : s;
}

export function envInt(name, defVal) {
  const raw = envStr(name, "");
  if (raw === "") return defVal;
  const n = parseInt(raw, 10);
  return Number.isFinite(n) ? n : defVal;
}

export function envFloat(name, defVal) {
  const raw = envStr(name, "");
  if (raw === "") return defVal;
  const n = parseFloat(raw);
  return Number.isFinite(n) ? n : defVal;
}

export function envBool(name, defVal) {
  const raw = envStr(name, "");
  if (raw === "") return defVal;
  const v = raw.toLowerCase();
  return v === "1" || v === "true" || v === "yes" || v === "y" || v === "on";
}

export function envCSV(name, defVals) {
  const raw = envStr(name, "");
  if (raw === "") return defVals;
  const parts = raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
  return parts.length > 0 ? parts : defVals;
}

export function normalizeBaseURL(url) {
  return String(url || "").replace(/\/+$/, "");
}

export const BASE_URL = normalizeBaseURL(envStr("CODEQ_BASE_URL", "http://localhost:8080"));
export const PRODUCER_TOKEN = envStr("CODEQ_PRODUCER_TOKEN", "dev-token");
export const WORKER_TOKEN = envStr("CODEQ_WORKER_TOKEN", "dev-token");
export const ADMIN_TOKEN = envStr("CODEQ_ADMIN_TOKEN", PRODUCER_TOKEN);

export const COMMANDS = envCSV("CODEQ_COMMANDS", ["GENERATE_MASTER"]);

