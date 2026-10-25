// Reduces a `tcpdump -nn -tttt -i any` capture taken inside the LocalAI container's network namespace to what left it: the
// DNS names queried, and the outbound TCP connection attempts (SYN on eth0) by destination. Loopback traffic and the
// health-check connections to 127.0.0.1:8080 are listed apart.
//
//   node summarize-capture.mjs <capture.txt>...
import fs from "node:fs";
for (const f of process.argv.slice(2)) {
  const names = new Map(), dests = new Map(), local = new Set();
  const first = [];
  for (const l of fs.readFileSync(f, "utf8").split("\n")) {
    const dns = l.match(/ eth0 +Out IP \S+ > (\S+)\.53: \d+\+? (?:\[[^\]]*\] )?(A|AAAA)\? ([^ ]+)\./);
    if (dns) names.set(dns[3], (names.get(dns[3]) || 0) + 1);
    const syn = l.match(/ eth0 +Out IP \S+ > (\d+\.\d+\.\d+\.\d+|[0-9a-f:]+)\.(\d+): Flags \[S\]/);
    if (syn) { const k = `${syn[1]}:${syn[2]}`; dests.set(k, (dests.get(k) || 0) + 1); }
    if (/ lo +In +IP 127\.0\.0\.1\.\d+ > 127\.0\.0\.1\.8080: Flags \[S\]/.test(l)) local.add("127.0.0.1:8080 (health check)");
  }
  console.log(`== ${f}`);
  console.log("DNS names queried:", [...names].map(([n, c]) => `${n} x${c}`).join(", ") || "none");
  console.log("outbound TCP SYN destinations:", [...dests].map(([n, c]) => `${n} x${c}`).join(", ") || "none");
  console.log("loopback:", [...local].join(", ") || "none");
}
