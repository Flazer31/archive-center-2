// Loopback OpenAI-compatible fake provider for step-1 RisuAI observation tests.
// No real model calls. Behaviour per request is chosen by the "mode" file or
// the model name suffix (e.g. model "fake-sse"), so the RisuAI side needs no change.
//
// modes: json | sse | sse-cut | fail-once | stall-body
// usage: node fake-provider.cjs [port] [logfile]
const http = require("http");
const fs = require("fs");
const path = require("path");

const port = Number(process.argv[2] || 28191);
const logFile = process.argv[3] || path.join(__dirname, "..", "data", "fake-provider.jsonl");
fs.mkdirSync(path.dirname(logFile), { recursive: true });
const modeFile = path.join(path.dirname(logFile), "fake-provider-mode.txt");

let counter = 0;
let failedOnce = new Set();

function currentMode(model) {
  const m = /fake-([a-z-]+)/.exec(model || "");
  if (m) return m[1];
  try { return fs.readFileSync(modeFile, "utf8").trim() || "json"; } catch (_) { return "json"; }
}

function replyText(n) {
  return `가짜 응답 ${n}번입니다. (fake reply ${n})`;
}

function log(entry) {
  fs.appendFileSync(logFile, JSON.stringify({ at: new Date().toISOString(), ...entry }) + "\n");
}

const server = http.createServer((req, res) => {
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.setHeader("Access-Control-Allow-Headers", "*");
  if (req.method === "OPTIONS") { res.writeHead(204); return res.end(); }
  if (req.method === "GET" && req.url.startsWith("/v1/models")) {
    res.writeHead(200, { "Content-Type": "application/json" });
    return res.end(JSON.stringify({ data: ["fake-json", "fake-sse", "fake-sse-cut", "fake-fail-once", "fake-stall-body"].map((id) => ({ id, object: "model" })) }));
  }
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    counter += 1;
    const n = counter;
    let parsed = {};
    try { parsed = JSON.parse(body || "{}"); } catch (_) {}
    const mode = currentMode(parsed.model);
    const stream = parsed.stream === true;
    log({ n, url: req.url, mode, stream, model: parsed.model, message_count: (parsed.messages || []).length, body });
    const text = replyText(n);

    if (mode === "fail-once") {
      const key = JSON.stringify(parsed.messages || []);
      if (!failedOnce.has(key)) {
        failedOnce.add(key);
        res.writeHead(500, { "Content-Type": "application/json" });
        return res.end(JSON.stringify({ error: { message: "fake transient failure" } }));
      }
    }
    if (mode === "stall-body") {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.write('{"id":"stall"');
      return; // never finishes
    }
    if (stream || mode === "sse" || mode === "sse-cut") {
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
      const parts = text.match(/.{1,4}/gu) || [text];
      let i = 0;
      const tick = setInterval(() => {
        if (mode === "sse-cut" && i === Math.floor(parts.length / 2)) {
          clearInterval(tick);
          return res.destroy();
        }
        if (i >= parts.length) {
          clearInterval(tick);
          res.write(`data: ${JSON.stringify({ choices: [{ index: 0, delta: {}, finish_reason: "stop" }] })}\n\n`);
          res.write("data: [DONE]\n\n");
          return res.end();
        }
        res.write(`data: ${JSON.stringify({ choices: [{ index: 0, delta: { content: parts[i] } }] })}\n\n`);
        i += 1;
      }, 250);
      return;
    }
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({
      id: "fake-" + n, object: "chat.completion", model: parsed.model || "fake-json",
      choices: [{ index: 0, message: { role: "assistant", content: text }, finish_reason: "stop" }],
      usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
    }));
  });
});

server.listen(port, "127.0.0.1", () => console.log(`fake provider on 127.0.0.1:${port}, log ${logFile}`));
