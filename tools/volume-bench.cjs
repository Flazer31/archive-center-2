// Volume bench: replays a real chat (chat_logs.ndjson rows of one session)
// through the real plugin (fake RisuAI host) into the real backend and
// MariaDB, then reports stored observation bytes and backend peak memory.
// usage: node tools/volume-bench.cjs <ac2.exe> <mariadb client exe> <port> <dbport> <chat_logs.ndjson> <session id> [plugin file]
const fs = require("fs"), path = require("path"), vm = require("vm"), os = require("os");
const { spawn, execFileSync } = require("child_process");
const [exe, client, port, dbport, logs, sessionId, pluginFile] = process.argv.slice(2);
const DB = "ac2_volume";
const sql = (q) => execFileSync(client, ["-h127.0.0.1", "-P" + dbport, "-uroot", "-N", "-B", "-e", q], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"], maxBuffer: 1 << 28 }).trim();
const tick = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const rows = fs.readFileSync(logs, "utf8").trim().split("\n").map(JSON.parse).filter((r) => r.chat_session_id === sessionId)
    .sort((a, b) => a.turn_index - b.turn_index || a.id - b.id);
  if (!rows.length) throw new Error("no rows for session");
  sql(`DROP DATABASE IF EXISTS ${DB}; CREATE DATABASE ${DB} CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`);
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "ac2vol-"));
  let token = "";
  const backend = spawn(exe, ["-port", port, "-data", dataDir, "-mariadb-dsn", `root@tcp(127.0.0.1:${dbport})/${DB}?parseTime=true`], { stdio: ["ignore", "ignore", "ignore"] });
  const started = Date.now();
  try {
    let up = false;
    for (let i = 0; i < 50 && !up; i++) {
      // the backend makes its access token on first start; every request carries it
      try { token = fs.readFileSync(path.join(dataDir, "token.txt"), "utf8").trim(); up = (await fetch(`http://127.0.0.1:${port}/health`, { headers: { "X-AC2-Token": token } })).ok; } catch (_) {}
      if (!up) await tick(100);
    }
    if (!up) throw new Error("backend did not start");
    const store = {};
    const chat = { id: "chat-volume", message: [], scriptstate: {} };
    const handlers = { input: [], beforeRequest: [], afterRequest: [], output: [] };
    const clone = (x) => JSON.parse(JSON.stringify(x));
    const api = {
      getArgument: async (k) => ({ ac2_url: `http://127.0.0.1:${port}`, ac2_token: token })[k],
      getLocalPluginStorage: async () => ({ getItem: async (k) => (k in store ? clone(store[k]) : null), setItem: async (k, v) => { store[k] = v; } }),
      addRisuScriptHandler: async (m, f) => handlers[m].push(f), removeRisuScriptHandler: async () => {},
      addRisuReplacer: async (m, f) => handlers[m].push(f), removeRisuReplacer: async () => {},
      addRisuChatListener: async (m, f) => handlers[m].push(f), removeRisuChatListener: async () => {},
      getCurrentCharacterIndex: async () => 0, getCurrentChatIndex: async () => 0,
      getChatFromIndex: async () => clone(chat),
      parseRisuChat: async () => "", getRuntimeInfo: async () => ({ platform: "node" }),
      onUnload: (f) => { api._unload = f; },
      nativeFetch: (url, init) => fetch(url, init),
    };
    const src = fs.readFileSync(pluginFile || path.join(__dirname, "..", "plugin", "archive-center-2.js"), "utf8");
    vm.runInContext(src, vm.createContext({ Risuai: api, console: { warn() {} }, setTimeout, clearTimeout, AbortController, Date, Math, JSON, Promise }));
    const drain = async () => { for (let i = 0; i < 2000; i++) { await tick(10); if (store.ac2_outbox_v1 && store.ac2_outbox_v1.queue.length === 0) return; } throw new Error("outbox did not drain"); };
    await tick(100);
    let n = 0;
    const hookMs = { beforeRequest: [], output: [] };
    const timed = async (name, fn) => { const t0 = process.hrtime.bigint(); await fn(); hookMs[name].push(Number(process.hrtime.bigint() - t0) / 1e6); };
    for (const r of rows) {
      const msg = { role: r.role === "user" ? "user" : "char", chatId: "m" + r.id, data: r.content, time: n++ };
      if (msg.role === "user") {
        await handlers.input[0](msg.data);
        chat.message.push(msg);
        await timed("beforeRequest", () => handlers.beforeRequest[0]([{ role: "user", content: msg.data }], "model"));
      } else {
        msg.generationInfo = { generationId: "g" + r.id };
        chat.message.push(msg);
        await handlers.afterRequest[0](msg.data, "model");
        const arg = { characterIndex: 0, chatIndex: 0, messageIndex: chat.message.length - 1, chat: clone(chat) };
        await timed("output", () => handlers.output[0](arg));
      }
      await drain();
    }
    const q = (s) => sql(`USE ${DB}; ${s}`);
    const peak = execFileSync("powershell", ["-NoProfile", "-Command", `(Get-Process -Id ${backend.pid}).PeakWorkingSet64`], { encoding: "utf8" }).trim();
    const chatBytes = rows.reduce((a, r) => a + Buffer.byteLength(r.content, "utf8"), 0);
    const out = {
      session: sessionId, rows: rows.length, chat_text_bytes: chatBytes,
      raw_events: Number(q(`SELECT COUNT(*) FROM raw_observations`)),
      raw_event_bytes: Number(q(`SELECT SUM(LENGTH(event)) FROM raw_observations`)),
      turns: Number(q(`SELECT COUNT(*) FROM logical_turns WHERE greeting=0`)),
      confirmed: Number(q(`SELECT COUNT(*) FROM logical_turns WHERE greeting=0 AND confirmed=1`)),
      revisions_text_bytes: Number(q(`SELECT COALESCE(SUM(LENGTH(data)),0) FROM revisions`)),
      member_text_bytes: Number(q(`SELECT COALESCE(SUM(LENGTH(data)),0) FROM turn_members`)),
      backend_peak_working_set_mb: Math.round(Number(peak) / 1048576),
      seconds: Math.round((Date.now() - started) / 1000),
      // plugin hook time in this Node process (fake host calls are immediate; a real host adds round trips)
      hook_ms: Object.fromEntries(Object.entries(hookMs).map(([k, v]) => [k, { avg: +(v.reduce((a, b) => a + b, 0) / v.length).toFixed(1), max: +Math.max(...v).toFixed(1), last: +v[v.length - 1].toFixed(1) }])),
    };
    // every source row's current stored text equals the chat text, row by row
    const stored = new Map();
    for (const line of q(`SELECT member_key, data FROM turn_members`).split("\n")) { const i = line.indexOf("\t"); stored.set(line.slice(0, i), line.slice(i + 1)); }
    for (const line of q(`SELECT r.row_key, r.data FROM revisions r JOIN (SELECT row_key, MAX(seq) s FROM revisions GROUP BY row_key) m ON r.row_key=m.row_key AND r.seq=m.s`).split("\n")) { const i = line.indexOf("\t"); stored.set(line.slice(0, i), line.slice(i + 1)); }
    const unescape = (s) => s.replace(/\\(.)/g, (_, c) => ({ n: "\n", t: "\t", "\\": "\\", 0: "\0" })[c] ?? c);
    out.rows_text_equal = rows.filter((r) => unescape(stored.get("c:m" + r.id) ?? "\u0000missing") === r.content).length;
    const diag = await (await fetch(`http://127.0.0.1:${port}/diagnostics`, { headers: { "X-AC2-Token": token } })).json();
    out.process_failures = diag.lifecycle.process_failures;
    console.log(JSON.stringify(out));
    await api._unload();
  } finally {
    backend.kill();
    await tick(300);
    fs.rmSync(dataDir, { recursive: true, force: true });
    try { sql(`DROP DATABASE IF EXISTS ${DB}`); } catch (_) {}
  }
})().catch((e) => { console.error(e); process.exit(2); });
