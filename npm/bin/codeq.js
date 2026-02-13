#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");
const { spawnSync } = require("child_process");

const exe = process.platform === "win32" ? ".exe" : "";
const binPath = path.join(__dirname, "..", "native", `codeq${exe}`);

if (!fs.existsSync(binPath)) {
  console.error("[codeq] Native binary not found.");
  console.error("[codeq] Reinstall the package to trigger download:");
  console.error("  npm i -g @osvaldoandrade/codeq@latest");
  process.exit(1);
}

const res = spawnSync(binPath, process.argv.slice(2), { stdio: "inherit" });
if (res.error) {
  console.error(`[codeq] Failed to run native binary: ${res.error.message}`);
  process.exit(1);
}
process.exit(typeof res.status === "number" ? res.status : 1);

