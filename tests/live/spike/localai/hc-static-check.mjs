// Re-verifies the machine-checkable rows of docs/security/localai-hardening-checklist.md (section 4) against the pinned
// LocalAI release: does each setting exist in that release's core/cli/run.go (and with which default), and does the EFFECTIVE
// compose configuration (docker compose config, all overrides applied) satisfy the rule?
//
//   node hc-static-check.mjs <run.go[,context.go,...] of the pinned tag> <resolved compose config> [out.json]
//
// The settings are declared in core/cli/run.go and, for the global ones (log level, debug), core/cli/context/context.go.
//
// It is a spike aid for M0-13, a floor like the M5-08 test it previews: it reads the compose file textually and cannot see
// what the running process does. The live probes (surface-probe.json, hardening-live.md) cover that.
import fs from "node:fs";

const [runGoPath, composePath, outPath] = process.argv.slice(2);
if (!runGoPath || !composePath) {
  console.error("usage: node hc-static-check.mjs <run.go> <resolved-compose.yaml> [out.json]");
  process.exit(2);
}
const runGo = runGoPath.split(",").map((f) => fs.readFileSync(f, "utf8")).join("\n");
const compose = fs.readFileSync(composePath, "utf8");

// env: "A,B" tags, with the default tag when there is one (run.go declares them on one line).
const declared = new Map();
for (const line of runGo.split("\n")) {
  const m = line.match(/env:"([^"]+)"/);
  if (!m) continue;
  const def = line.match(/default:"([^"]*)"/);
  for (const name of m[1].split(",")) declared.set(name.trim(), { default: def ? def[1] : null, line: line.trim().slice(0, 120) });
}

// The effective environment block of the localai service.
const env = new Map();
const lines = compose.split("\n");
const start = lines.findIndex((l) => /^ {4}environment:\s*$/.test(l));
if (start < 0) throw new Error("no environment block in the resolved compose file");
for (let i = start + 1; i < lines.length && /^ {6}\S/.test(lines[i]); i++) {
  const m = lines[i].match(/^ {6}([A-Za-z0-9_]+):\s*(.*)$/);
  if (m) env.set(m[1], m[2].replace(/^["']|["']$/g, ""));
}
const has = (k) => env.has(k) && env.get(k) !== "";
const isFalsy = (k) => !has(k) || ["false", "[]", "0"].includes(env.get(k));

const rules = [
  // [variable, rule, expected, checklist row]
  ["LOCALAI_API_KEY", "present", null, "HC-06"],
  ["LOCALAI_SUBTLE_KEY_COMPARISON", "equals", "true", "HC-07"],
  ["LOCALAI_DISABLE_WEBUI", "equals", "true", "HC-12"],
  ["LOCALAI_DISABLE_RUNTIME_SETTINGS", "equals", "true", "HC-13"],
  ["LOCALAI_DISABLE_GALLERY_ENDPOINT", "equals", "true", "HC-14"],
  ["LOCALAI_AUTOLOAD_GALLERIES", "equals", "false", "HC-14"],
  ["LOCALAI_AUTOLOAD_BACKEND_GALLERIES", "equals", "false", "HC-14"],
  ["LOCALAI_GALLERIES", "falsy", null, "HC-14"],
  ["LOCALAI_BACKEND_GALLERIES", "falsy", null, "HC-14"],
  ["LOCALAI_DISABLE_AGENTS", "equals", "true", "HC-15"],
  ["LOCALAI_DISABLE_MCP", "equals", "true", "HC-16"],
  ["LOCALAI_DISABLE_METRICS_ENDPOINT", "equals", "true", "HC-21"],
  ["LOCALAI_MAX_ACTIVE_BACKENDS", "equals", "1", "HC-29"],
  ["LOCALAI_OPAQUE_ERRORS", "equals", "true", "HC-20"],
  ["LOCALAI_DISABLE_API_KEY_REQUIREMENT_FOR_HTTP_GET", "falsy", null, "HC-08"],
  ["LOCALAI_HTTP_GET_EXEMPTED_ENDPOINTS", "falsy", null, "HC-08"],
  ["LOCALAI_AUTH", "falsy", null, "HC-09"],
  ["GITHUB_CLIENT_ID", "falsy", null, "HC-09"],
  ["GITHUB_CLIENT_SECRET", "falsy", null, "HC-09"],
  ["LOCALAI_OIDC_ISSUER", "falsy", null, "HC-09"],
  ["LOCALAI_P2P", "falsy", null, "HC-17"],
  ["LOCALAI_FEDERATED", "falsy", null, "HC-17"],
  ["LOCALAI_P2P_TOKEN", "falsy", null, "HC-17"],
  ["LOCALAI_P2P_NETWORK_ID", "falsy", null, "HC-17"],
  ["LOCALAI_EXTERNAL_BACKENDS", "falsy", null, "HC-18"],
  ["LOCALAI_EXTERNAL_GRPC_BACKENDS", "falsy", null, "HC-18"],
  ["LOCALAI_CORS", "falsy", null, "HC-19"],
  ["LOCALAI_CORS_ALLOW_ORIGINS", "falsy", null, "HC-19"],
  ["LOCALAI_DISABLE_CSRF", "falsy", null, "HC-19"],
  ["LOCALAI_DISABLE_PREDOWNLOAD_SCAN", "falsy", null, "HC-22"],
  ["LOCALAI_MACHINE_TAG", "falsy", null, "HC-23"],
  ["LOCALAI_DEBUG", "falsy", null, "HC-34"],
  ["LOCALAI_LOG_LEVEL", "info", null, "HC-34"],
  ["DEBUG", "falsy", null, "HC-34"],
  ["LOCALAI_WATCHDOG_IDLE", "equals", "true", "HC-30"],
  ["LOCALAI_WATCHDOG_BUSY", "info", null, "HC-30"],
  ["LOCALAI_MAX_CONCURRENT_BACKEND_REQUESTS", "info", null, "HC-31"],
  ["LOCALAI_UPLOAD_LIMIT", "info", null, "HC-32"],
  ["LOCALAI_ADDRESS", "info", null, "HC-01"],
  ["LOCALAI_MODELS_PATH", "info", null, "HC-24"],
  ["LOCALAI_BACKENDS_PATH", "info", null, "HC-24"],
  ["LOCALAI_DATA_PATH", "info", null, "HC-24"],
  ["LOCALAI_CONFIG_DIR", "info", null, "HC-24"],
  ["LOCALAI_REQUIRE_BACKEND_INTEGRITY", "info", null, "HC-27"],
];

const rows = rules.map(([name, rule, expected, row]) => {
  const d = declared.get(name);
  const value = env.has(name) ? (name.includes("API_KEY") || name.includes("SECRET") || name.includes("TOKEN") ? "<set>" : env.get(name)) : null;
  let verdict;
  switch (rule) {
    case "present": verdict = has(name) ? "ok" : "FAIL"; break;
    case "equals": verdict = env.get(name) === expected ? "ok" : "FAIL"; break;
    case "falsy": verdict = isFalsy(name) ? "ok" : "FAIL"; break;
    default: verdict = "info";
  }
  return {
    row, variable: name, rule: rule === "equals" ? `= ${expected}` : rule, exists_in_pinned_release: !!d,
    release_default: d ? d.default : null, effective_value: value, verdict,
  };
});

// A setting the checklist names but the pinned release does not declare is a checklist error or a renamed flag.
const missing = rows.filter((r) => !r.exists_in_pinned_release).map((r) => `${r.variable} (${r.row})`);
// Every env name the effective compose sets must be one the release knows (a typo would silently do nothing).
const unknownSet = [...env.keys()].filter((k) => !declared.has(k));

const out = {
  pinned_sources: "core/cli/run.go and core/cli/context/context.go at v4.10.0 (github.com/mudler/LocalAI)",
  declared_settings: declared.size,
  effective_env_count: env.size,
  rows,
  checklist_settings_missing_from_pinned_release: missing,
  effective_env_names_unknown_to_pinned_release: unknownSet,
  failures: rows.filter((r) => r.verdict === "FAIL").map((r) => `${r.row} ${r.variable}`),
};
if (outPath) fs.writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(`declared in release: ${declared.size}; effective env: ${env.size}`);
console.log("failures:", out.failures.length ? out.failures.join(", ") : "none");
console.log("checklist settings the release does not declare:", missing.length ? missing.join(", ") : "none");
console.log("effective env names unknown to the release:", unknownSet.length ? unknownSet.join(", ") : "none");
for (const r of rows) console.log([r.row, r.variable, r.rule, "release:" + (r.exists_in_pinned_release ? "yes" : "NO"), "default:" + r.release_default, "effective:" + r.effective_value, r.verdict].join(" | "));
