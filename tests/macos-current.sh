#!/bin/bash
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
app="${CODEX_APP_PATH:-}"
if [[ -z "$app" ]]; then
  for candidate in "/Applications/Codex.app" "$HOME/Applications/Codex.app" \
                   "/Applications/ChatGPT.app" "$HOME/Applications/ChatGPT.app"; do
    [[ -d "$candidate" ]] && { app="$candidate"; break; }
  done
fi
[[ -n "$app" && -d "$app" ]] || {
  echo 'Codex.app or ChatGPT.app was not found. Set CODEX_APP_PATH to its path.' >&2
  exit 1
}

asar="$app/Contents/Resources/app.asar"
[[ -f "$asar" ]] || {
  echo "app.asar was not found under $app" >&2
  exit 1
}

bash "$repo/tests/macos-smoke.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

node - "$asar" "$tmp" "$repo/cdp-supervisor.mjs" "$repo/quota-gate.cjs" <<'NODE'
const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const [asarPath, outputDirectory, supervisorPath, gatePath] = process.argv.slice(2);
const gate = require(gatePath);
const buffer = fs.readFileSync(asarPath);
const headerSize = buffer.readUInt32LE(12);
const dataStart = 16 + headerSize;
const header = JSON.parse(buffer.subarray(16, dataStart).toString("utf8"));
const candidates = [];

function walk(node, prefix = "") {
  for (const [name, entry] of Object.entries(node.files || {})) {
    const filePath = `${prefix}/${name}`;
    if (entry.files) {
      walk(entry, filePath);
      continue;
    }
    if (!name.endsWith(".js")) continue;
    const start = dataStart + Number(entry.offset);
    const source = buffer.subarray(start, start + Number(entry.size));
    if (source.indexOf("rate_limit?.allowed!==!1") === -1) continue;
    const text = source.toString("utf8");
    const report = gate.explain(text, `app://-${filePath}`);
    if (report.matched.length === 0) continue;
    const bundlePath = path.join(outputDirectory, path.basename(name));
    fs.writeFileSync(bundlePath, source);
    candidates.push({ filePath, bundlePath, matched: report.matched });
  }
}

walk(header);
if (candidates.length === 0) {
  throw new Error("no current app bundle matched the quota gate");
}

for (const candidate of candidates) {
  const output = execFileSync(process.execPath, [
    supervisorPath,
    "--self-test",
    "--gate", gatePath,
    "--bundle", candidate.bundlePath,
  ], { encoding: "utf8" });
  const result = JSON.parse(output);
  if (result.ok !== true) throw new Error(`self-test failed for ${candidate.filePath}`);
}

console.log(JSON.stringify({
  ok: true,
  bundleCount: candidates.length,
  bundles: candidates.map(({ filePath, matched }) => ({ filePath, matched })),
}, null, 2));
NODE

echo 'Current app quota gate checks passed'
