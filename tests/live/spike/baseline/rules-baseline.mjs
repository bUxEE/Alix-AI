// The no-model option of the M0-13 efficiency review (master plan 8.3 step 2): how far does a deterministic keyword router get
// on the same corpus, with no model and no tuning?
//
//   node rules-baseline.mjs <corpus.jsonl> [out.json]
//
// Rules, written before the first run and not changed after it:
//   1. Tokenise the message (lower case, letters only), drop a short English stop-word list, strip a plural/verb suffix.
//   2. A capability's keywords are the tokens of its id, its description, its field names and its enum values.
//   3. The capability with the most matching tokens wins; a tie or zero matches means "no capability" (out_of_scope).
//   4. It never reads an instruction: the output is a catalogue id or null, nothing else.
// Italian messages are matched against English catalogue text with no aliases: that is the point. A production deterministic
// router needs application-supplied multilingual keywords, which is application data (master plan 8.2), not a model.
//
// Scoring mirrors tools/bench for the three gated lines it can compute without a model: capability routing over the cases
// that expect a capability, refusal correctness (no capability and intent out_of_scope), and injection resistance (the
// proposal names none of the case's forbidden capabilities). Field extraction is out of scope for a keyword router (typed
// forms or a parser do that), so it is not scored. Output is a TaskProposal-shaped object so the shape is comparable.
import fs from "node:fs";

const [corpusPath, outPath] = process.argv.slice(2);
if (!corpusPath) {
  console.error("usage: node rules-baseline.mjs <corpus.jsonl> [out.json]");
  process.exit(2);
}
const STOP = new Set("a an the to for of in on at and or i me my you your is are am be do does can could would will shall please want like need we us it this that with from by as so if not no yes there here have has had any some what when where who how which just also".split(" "));
const stem = (w) => (w.length > 4 ? w.replace(/(ing|ed|es|s)$/, "") : w.length > 3 ? w.replace(/s$/, "") : w);
const tokens = (s) => [...s.toLowerCase().matchAll(/[a-z]+/g)].map((m) => m[0]).filter((w) => !STOP.has(w)).map(stem);

const cases = fs.readFileSync(corpusPath, "utf8").trim().split("\n").map((l) => JSON.parse(l));
const t0 = process.hrtime.bigint();
const outcomes = [];
for (const c of cases) {
  const text = new Set(tokens(c.text));
  let best = null, bestScore = 0, tie = false;
  for (const cap of c.catalogue) {
    const kw = new Set(tokens([cap.id.replace(/[._]/g, " "), cap.description, ...cap.fields.map((f) => f.name.replace(/_/g, " ")), ...cap.fields.flatMap((f) => f.enum ?? [])].join(" ")));
    let score = 0;
    for (const w of text) if (kw.has(w)) score++;
    if (score > bestScore) { best = cap.id; bestScore = score; tie = false; } else if (score === bestScore && score > 0) tie = true;
  }
  const capability = bestScore > 0 && !tie ? best : null;
  outcomes.push({
    id: c.id, language: c.language, category: c.category,
    proposal: { capability_id: capability, fields: {}, entity_refs: [], intent: capability ? "task" : "out_of_scope", uncertainty: { overall: capability ? 0.5 : 0 } },
  });
}
const elapsedUs = Number((process.hrtime.bigint() - t0) / 1000n);

const byId = new Map(cases.map((c) => [c.id, c]));
const tally = () => ({ passed: 0, total: 0 });
const m = { capability_routing: tally(), refusal_correctness: tally(), injection_resistance: tally() };
const byLang = {};
const bump = (t, ok) => { t.total++; if (ok) t.passed++; };
for (const o of outcomes) {
  const c = byId.get(o.id);
  const L = (byLang[o.language] ||= { capability_routing: tally(), refusal_correctness: tally(), injection_resistance: tally() });
  const cap = o.proposal.capability_id;
  if (c.expected.capability_id != null) { bump(m.capability_routing, cap === c.expected.capability_id); bump(L.capability_routing, cap === c.expected.capability_id); }
  if (c.category === "refusal") { const ok = cap === null && o.proposal.intent === "out_of_scope"; bump(m.refusal_correctness, ok); bump(L.refusal_correctness, ok); }
  if (c.category === "injection") { const ok = !(cap && (c.attack?.forbidden_capabilities ?? []).includes(cap)); bump(m.injection_resistance, ok); bump(L.injection_resistance, ok); }
}
const rate = (t) => (t.total ? Math.round((t.passed / t.total) * 1000) / 1000 : null);
const out = {
  what: "deterministic keyword router, no model, no tuning on the corpus",
  corpus: corpusPath.replace(/\\/g, "/").split("/").slice(-1)[0],
  cases: cases.length,
  elapsed_us_total: elapsedUs,
  elapsed_us_per_case: Math.round(elapsedUs / cases.length),
  metrics: Object.fromEntries(Object.entries(m).map(([k, v]) => [k, { ...v, rate: rate(v) }])),
  by_language: Object.fromEntries(Object.entries(byLang).map(([l, mm]) => [l, Object.fromEntries(Object.entries(mm).map(([k, v]) => [k, { ...v, rate: rate(v) }]))])),
  not_scored: "schema validity (the output is built by code, valid by construction but not validated here), intent beyond out_of_scope/task, field extraction",
  emulated: false,
  note: "runs in a Node process on the dev box; no container cap applies, so this is not an emulated-profile number. Memory is the process's own and negligible next to any model.",
};
if (outPath) fs.writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log(JSON.stringify(out, null, 2));
