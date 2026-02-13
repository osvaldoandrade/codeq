#!/usr/bin/env node
"use strict";

const fs = require("fs");
const https = require("https");
const path = require("path");

const pkg = require("../package.json");

function goosFromPlatform(platform) {
  switch (platform) {
    case "darwin":
      return "darwin";
    case "linux":
      return "linux";
    case "win32":
      return "windows";
    default:
      return null;
  }
}

function goarchFromArch(arch) {
  switch (arch) {
    case "x64":
      return "amd64";
    case "arm64":
      return "arm64";
    default:
      return null;
  }
}

function downloadToFile(url, filePath, redirectsLeft) {
  return new Promise((resolve, reject) => {
    const req = https.get(
      url,
      {
        headers: {
          "User-Agent": "codeq-npm-installer",
          Accept: "application/octet-stream",
        },
      },
      (res) => {
        const code = res.statusCode || 0;
        if (code >= 300 && code < 400 && res.headers.location) {
          if (redirectsLeft <= 0) {
            reject(new Error(`too many redirects while downloading ${url}`));
            res.resume();
            return;
          }
          const next = res.headers.location.startsWith("http")
            ? res.headers.location
            : new URL(res.headers.location, url).toString();
          res.resume();
          downloadToFile(next, filePath, redirectsLeft - 1).then(resolve, reject);
          return;
        }
        if (code !== 200) {
          let body = "";
          res.setEncoding("utf8");
          res.on("data", (d) => {
            body += d;
          });
          res.on("end", () => {
            const msg = body.trim() ? `: ${body.trim()}` : "";
            reject(new Error(`HTTP ${code} downloading ${url}${msg}`));
          });
          return;
        }

        fs.mkdirSync(path.dirname(filePath), { recursive: true });
        const f = fs.createWriteStream(filePath, { mode: 0o755 });
        res.pipe(f);
        f.on("finish", () => f.close(resolve));
        f.on("error", (err) => {
          try {
            fs.unlinkSync(filePath);
          } catch (_) {}
          reject(err);
        });
      }
    );
    req.on("error", reject);
  });
}

async function main() {
  const repo = process.env.CODEQ_GITHUB_REPO || "osvaldoandrade/codeq";
  const tag = process.env.CODEQ_RELEASE_TAG || `v${pkg.version}`;

  const goos = goosFromPlatform(process.platform);
  const goarch = goarchFromArch(process.arch);
  if (!goos || !goarch) {
    console.error(`[codeq] Unsupported platform for prebuilt binaries: ${process.platform}/${process.arch}`);
    console.error("[codeq] Fallback: install from source using install.sh:");
    console.error("  curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh");
    process.exit(1);
  }

  const exe = goos === "windows" ? ".exe" : "";
  const assetName = `codeq-${goos}-${goarch}${exe}`;
  const url = `https://github.com/${repo}/releases/download/${tag}/${assetName}`;

  const outPath = path.join(__dirname, "..", "native", `codeq${exe}`);
  const tmpPath = `${outPath}.tmp`;

  console.log(`[codeq] Downloading ${assetName} from ${tag}`);
  await downloadToFile(url, tmpPath, 5);

  try {
    fs.renameSync(tmpPath, outPath);
  } catch (err) {
    // Windows can fail to replace an existing file in use; try to remove and retry.
    try {
      fs.unlinkSync(outPath);
    } catch (_) {}
    fs.renameSync(tmpPath, outPath);
  }

  if (goos !== "windows") {
    try {
      fs.chmodSync(outPath, 0o755);
    } catch (_) {}
  }

  console.log("[codeq] Installed native binary");
}

main().catch((err) => {
  console.error(`[codeq] Install failed: ${err && err.message ? err.message : String(err)}`);
  process.exit(1);
});

