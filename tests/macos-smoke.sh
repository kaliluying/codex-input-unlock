#!/bin/bash
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
launcher="$repo/macos/codex-input-unlock-macos.sh"
bash -n "$launcher"
bash -n "$repo/macos/双击启动 Codex.command"
node --check "$repo/cdp-supervisor.mjs"
node --check "$repo/quota-gate.cjs"
node - "$repo/quota-gate.cjs" <<'NODE'
const assert = require("node:assert/strict");
const gate = require(process.argv[2]);
assert.equal(gate.condition({ mode: "codex-local-quota", quotaVariable: "quota" }), "(quota=false,false)");
assert.equal(gate.condition({ mode: "codex-local-quota", quotaVariable: "bad-name" }), null);

const currentCodexBundle = [
  "XP=Xl(K,({get:e})=>{let t=e(Ey),n=e(ly).data;if(t.authMethod!==`chatgpt`||n.rate_limit?.allowed!==!1)return!1;return!0})",
  "vn=X(bt===`local`&&XP),",
  "let Zn=De||gn||vn,Qn;",
  "submitDisabled:Zn",
].join("");
const location = gate.locate(currentCodexBundle, "app://-/assets/app-primary-current.js");
assert.equal(location?.mode, "codex-local-quota");
assert.equal(location?.quotaVariable, "vn");
assert.equal(location?.hostVariable, "bt");
assert.equal(gate.condition(location), "(vn=false,false)");

const previousCodexBundle = currentCodexBundle.replace(
  "vn=X(bt===`local`&&XP),",
  "vn=X(XP)&&bt===`local`,",
);
const previousLocation = gate.locate(previousCodexBundle, "app://-/assets/app-primary-previous.js");
assert.equal(previousLocation?.mode, "codex-local-quota");

const rateLimitStateBundle = [
  "function VF(e,t){let o=WF(e,t),l=o?.blocked_features?.filter(e=>e.name===`send`),d=l?.find(...)??null;return{",
  "sendBlockReason:d?.block_reason??null,sendBlocked:d!=null}}",
].join("");
const stateLocation = gate.locate(rateLimitStateBundle, "app://-/assets/app-initial-next.js");
assert.equal(stateLocation?.mode, "codex-rate-limit-state");
assert.equal(stateLocation?.quotaVariable, "d");
assert.equal(gate.condition(stateLocation), "(d=null,false)");

const derivedBundle = [
  "lV=z(Z,({get:e})=>{let t=e(Xb),n=e(Nb).data;",
  "if(t.authMethod!==`chatgpt`||n.rate_limit?.allowed!==!1)return!1;return!0})",
].join("");
const derivedLocation = gate.locate(derivedBundle, "app://-/assets/app-primary-next.js");
assert.equal(derivedLocation?.mode, "codex-rate-limit-derived");
assert.equal(derivedLocation?.quotaVariable, "t");
assert.equal(gate.condition(derivedLocation), "(t={},false)");

const atomGuardBundle = [
  "XP=Xl(K,({get:e})=>{let t=e(Ey),n=e(ly).data;if(t.authMethod!==`chatgpt`||n.rate_limit?.allowed!==!1)return!1;return!0})",
].join("");
const atomGuard = gate.locateAll(atomGuardBundle, "app://-/assets/app-primary-atom.js")
  .find((entry) => entry.location.mode === "quota-atom-data");
assert.equal(atomGuard?.location.quotaVariable, "n");
assert.equal(atomGuard?.condition, "(__codexExternalQuotaGate.unlock(n),false)");
NODE
echo 'macOS static smoke checks passed'
