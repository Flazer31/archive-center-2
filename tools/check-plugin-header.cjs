// Mirrors RisuAI importPlugin header checks (plugins.svelte.ts @ 02a8369) to catch install errors early.
const fs = require("fs");
const cmp = (a, b) => { const x = a.split(".").map(Number), y = b.split(".").map(Number); for (let i = 0; i < Math.max(x.length, y.length); i++) { const p = x[i] || 0, q = y[i] || 0; if (p > q) return 1; if (p < q) return -1; } return 0; };
for (const f of process.argv.slice(2)) {
  const js = fs.readFileSync(f, "utf8").replace(/^﻿/gm, "");
  const errs = []; let name = "", api = "", ver = "";
  for (const line of js.split("\n")) {
    if (line.startsWith("//@name")) { name = line.slice(7).trim(); if (!name) errs.push("empty name"); }
    if (line.startsWith("//@api")) api = line.split(" ").slice(1).join(" ").trim();
    if (line.startsWith("//@display-name") && !line.slice(15).trim()) errs.push("empty display-name");
    if (line.startsWith("//@arg") || line.startsWith("//@risu-arg")) { const p = line.trim().split(" "); if (p.length < 3) errs.push("bad arg: " + line); else if (p[2] !== "int" && p[2] !== "string") errs.push("bad arg type: " + p[2]); }
    if (line.startsWith("//@link") && !/^https:/.test(line.slice(7).trim())) errs.push("link must be https");
    if (line.startsWith("//@version")) { ver = line.split(" ").slice(1).join(" ").trim(); const at = js.indexOf("//@version"); if (Buffer.byteLength(js.slice(0, at) + line) > 512) errs.push("version beyond 512 bytes"); }
  }
  if (!name) errs.push("name not found");
  if (api !== "3.0") errs.push("api must be 3.0, got " + api);
  if (ver && cmp(ver, "0.0.1") === -1) errs.push("version must be >= 0.0.1, got " + ver);
  console.log(f, errs.length ? "FAIL " + errs.join("; ") : "OK", "name=" + name, "version=" + ver);
}
