// Chain test: the real plugin (fake RisuAI host) -> the real ac2 backend over
// HTTP -> MariaDB, through the five permanent-contract cases. It replaces
// only the RisuAI host (scripted chat) and the provider (no model call).
// usage: node tools/e2e-chain.cjs <ac2.exe> <mariadb client exe> <port> <dbport>
const fs = require("fs"), path = require("path"), vm = require("vm"), os = require("os");
const { spawn, execFileSync } = require("child_process");
const [exe, client, port, dbport] = process.argv.slice(2);
const DB = "ac2_e2e";
const sql = (q) => execFileSync(client, ["-h127.0.0.1", "-P" + dbport, "-uroot", "-N", "-B", "-e", q], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
const tick = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const check = (name, ok, detail) => { console.log((ok ? "PASS " : "FAIL ") + name + (ok ? "" : "  " + detail)); if (!ok) failures++; };

(async () => {
  sql(`DROP DATABASE IF EXISTS ${DB}; CREATE DATABASE ${DB} CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`);
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "ac2e2e-"));
  let backend;
  let blog = "";
  const startBackend = async () => {
    backend = spawn(exe, ["-port", port, "-data", dataDir, "-mariadb-dsn", `root@tcp(127.0.0.1:${dbport})/${DB}?parseTime=true`], { stdio: ["ignore", "ignore", "pipe"] });
    backend.stderr.on("data", (d) => (blog += d));
    // the backend makes its access token on first start; every request carries it
    let up = false;
    for (let i = 0; i < 50 && !up; i++) {
      try {
        token = fs.readFileSync(path.join(dataDir, "token.txt"), "utf8").trim();
        up = (await fetch(`http://127.0.0.1:${port}/health`, { headers: { "X-AC2-Token": token } })).ok;
      } catch (_) {}
      if (!up) await tick(100);
    }
    if (!up) throw new Error("backend did not start: " + blog);
  };
  let token = "";
  const stopBackend = async () => {
    const done = new Promise((r) => backend.once("exit", r));
    backend.kill();
    await done;
  };
  try {
    await startBackend();
    const noToken = await fetch(`http://127.0.0.1:${port}/health`);
    const wrongToken = await fetch(`http://127.0.0.1:${port}/observe`, { method: "POST", headers: { "X-AC2-Token": "ac2-wrong" }, body: "{}" });
    check("requests without or with a wrong token are refused", noToken.status === 401 && wrongToken.status === 401, `${noToken.status} ${wrongToken.status}`);

    // fake RisuAI host
    const store = {};
    const chat = { id: "chat-E2E", message: [{ role: "char", chatId: "g0", data: "인사말" }], scriptstate: {} };
    const handlers = { input: [], beforeRequest: [], afterRequest: [], output: [] };
    const clone = (x) => JSON.parse(JSON.stringify(x));
    const api = {
      getArgument: async (k) => ({ ac2_url: `http://127.0.0.1:${port}`, ac2_token: token })[k],
      getLocalPluginStorage: async () => ({ getItem: async (k) => (k in store ? clone(store[k]) : null), setItem: async (k, v) => { store[k] = clone(v); } }),
      addRisuScriptHandler: async (m, f) => handlers[m].push(f), removeRisuScriptHandler: async () => {},
      addRisuReplacer: async (m, f) => handlers[m].push(f), removeRisuReplacer: async () => {},
      addRisuChatListener: async (m, f) => handlers[m].push(f), removeRisuChatListener: async () => {},
      getCurrentCharacterIndex: async () => 0, getCurrentChatIndex: async () => 0,
      getChatFromIndex: async () => clone(chat),
      parseRisuChat: async () => "", getRuntimeInfo: async () => ({ platform: "node" }),
      onUnload: (f) => { api._unload = f; },
      nativeFetch: (url, init) => fetch(url, init),
    };
    vm.runInContext(fs.readFileSync(path.join(__dirname, "..", "plugin", "archive-center-2.js"), "utf8"),
      vm.createContext({ Risuai: api, console: { warn: (...a) => console.log("[plugin]", ...a) }, setTimeout, clearTimeout, AbortController, Date, Math, JSON, Promise }));
    const drain = async () => {
      for (let i = 0; i < 200; i++) { await tick(25); if (store.ac2_outbox_v1 && store.ac2_outbox_v1.queue.length === 0) return; }
      throw new Error("outbox did not drain");
    };
    await tick(100);
    await drain();

    const msgs = chat.message;
    const request = async () => { await handlers.beforeRequest[0]([{ role: "user", content: "x" }], "model"); };
    const reply = async (row) => {
      msgs.push(row);
      await handlers.afterRequest[0](row.data, "model");
      await handlers.output[0]({ characterIndex: 0, chatIndex: 0, messageIndex: msgs.length - 1, chat: clone(chat) });
    };
    const send = async (row) => { await handlers.input[0](row.data); msgs.push(row); await request(); };

    // 1. normal turn
    await send({ role: "user", chatId: "u1", data: "안녕" });
    await reply({ role: "char", chatId: "a1", generationInfo: { generationId: "gen-a1" }, data: "첫 응답" });
    // 2. same-request provider retry (beforeRequest again, nothing new)
    await request();
    await request();
    // 3. reroll of the same user row
    msgs.pop();
    await request();
    await reply({ role: "char", chatId: "a1b", generationInfo: { generationId: "gen-a1b" }, data: "리롤 응답" });
    // backend restart: the next observations continue from MariaDB
    await drain();
    await stopBackend();
    await startBackend();
    // 4. delete the reply, edit the same user row, regenerate
    msgs.pop();
    msgs[1].data = "안녕 (수정)";
    await request();
    await reply({ role: "char", chatId: "a1c", generationInfo: { generationId: "gen-a1c" }, data: "편집 후 응답" });
    // 5. genuinely new user row with identical text
    await send({ role: "user", chatId: "u2", data: "안녕" });
    await reply({ role: "char", chatId: "a2", generationInfo: { generationId: "gen-a2" }, data: "둘째 응답" });
    // 6. normal new turn
    await send({ role: "user", chatId: "u3", data: "다음 이야기" });
    await reply({ role: "char", chatId: "a3", generationInfo: { generationId: "gen-a3" }, data: "셋째 응답" });
    // 7. translated reply (Yumi): the stored text is the original from the record
    const Y = (id, t) => `<!-- yumi-tr:v1:${id}:start -->${t}<!-- yumi-tr:v1:${id}:end -->`;
    await send({ role: "user", chatId: "u4", data: "번역 차례" });
    chat.scriptstate["$__yumi_tr.ab12"] = 'u:{"model":"ORIGINAL ENGLISH REPLY"}';
    await reply({ role: "char", chatId: "a4", generationInfo: { generationId: "gen-a4" }, data: Y("ab12", "번역된 응답") });
    // 8. translated reply whose record is missing: kept unreadable, never the translation
    await send({ role: "user", chatId: "u5", data: "기록 없음" });
    await reply({ role: "char", chatId: "a5", generationInfo: { generationId: "gen-a5" }, data: Y("zz99", "번역만 있음") });
    await drain();
    await api._unload();
    await drain(); // the unload event too, while the backend is up

    const q = (s) => sql(`USE ${DB}; ${s}`);
    const turns = q(`SELECT turn_id, greeting, confirmed, inactive FROM logical_turns WHERE session_key='chat:chat-E2E' ORDER BY turn_id`);
    const members = q(`SELECT member_key, turn_id, state, data FROM turn_members ORDER BY turn_id, ord`);
    const rows = q(`SELECT row_key, turn_id, state FROM response_rows ORDER BY turn_id, ord`);
    console.log("turns (id greeting confirmed inactive):\n" + turns + "\nmembers:\n" + members + "\nresponses:\n" + rows);
    const owner = (k) => q(`SELECT turn_id FROM response_rows WHERE row_key='c:${k}'`);
    const stateOf = (k) => q(`SELECT state FROM response_rows WHERE row_key='c:${k}'`);
    const memberTurn = (k) => q(`SELECT turn_id FROM turn_members WHERE member_key='c:${k}'`);
    check("five non-greeting turns", q(`SELECT COUNT(*) FROM logical_turns WHERE greeting=0`) === "5", turns);
    check("provider retry added no member", q(`SELECT COUNT(*) FROM turn_members WHERE turn_id=${memberTurn("u1")}`) === "1", members);
    check("reroll replaced on the same turn", owner("a1b") === owner("a1") && owner("a1") === memberTurn("u1") && stateOf("a1") === "replaced", rows);
    check("edit+regenerate replaced on the same turn", owner("a1c") === memberTurn("u1") && stateOf("a1c") === "active" && stateOf("a1b") === "replaced", rows);
    check("edited member text stored", q(`SELECT data FROM turn_members WHERE member_key='c:u1'`) === "안녕 (수정)", members);
    check("identical text new row is a new turn", memberTurn("u2") !== memberTurn("u1") && owner("a2") === memberTurn("u2"), members);
    check("normal new turn appended after", Number(memberTurn("u3")) > Number(memberTurn("u2")) && owner("a3") === memberTurn("u3"), rows);
    check("all five turns confirmed by output", q(`SELECT COUNT(*) FROM logical_turns WHERE greeting=0 AND confirmed=1`) === "5", turns);
    check("translated reply stored as the original", q(`SELECT data FROM revisions WHERE row_key='c:a4'`) === "ORIGINAL ENGLISH REPLY", q(`SELECT data FROM revisions WHERE row_key='c:a4'`));
    check("missing original stays unreadable, translation never stored",
      stateOf("a5") === "active" && q(`SELECT COUNT(*) FROM revisions WHERE row_key='c:a5'`) === "0" && q(`SELECT COUNT(*) FROM revisions WHERE data LIKE '%번역%'`) === "0", q(`SELECT row_key, data FROM revisions`));
    check("raw observations in plugin order",
      q(`SELECT COUNT(*) FROM raw_observations`) !== "0" &&
      q(`SELECT COUNT(*) FROM (SELECT seq, LAG(seq) OVER (PARTITION BY boot_id ORDER BY id) prev FROM raw_observations WHERE boot_id IS NOT NULL) x WHERE prev IS NOT NULL AND seq <= prev`) === "0", "");
    check("revision text read back", q(`SELECT data FROM revisions WHERE row_key='c:a1c'`) === "편집 후 응답", "");
    const diag = await (await fetch(`http://127.0.0.1:${port}/diagnostics`, { headers: { "X-AC2-Token": token } })).json();
    check("no processing failures", diag.lifecycle && diag.lifecycle.process_failures === 0, JSON.stringify(diag));
    // every plugin event (including the unload) stored exactly once
    const seqs = q(`SELECT COUNT(*), COUNT(DISTINCT seq), MAX(seq), MIN(seq) FROM raw_observations WHERE boot_id IS NOT NULL`).split("	").map(Number);
    check("every plugin event stored exactly once", seqs[0] === seqs[1] && seqs[1] === seqs[2] && seqs[3] === 1, seqs.join(" "));
    check("unload event delivered", q(`SELECT COUNT(*) FROM raw_observations WHERE hook='unload'`) === "1", "");
  } finally {
    backend.kill();
    await tick(300);
    fs.rmSync(dataDir, { recursive: true, force: true });
    try { sql(`DROP DATABASE IF EXISTS ${DB}`); } catch (_) {}
  }
  console.log(failures ? `${failures} FAILED` : "ALL PASS");
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(2); });
