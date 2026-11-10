// Records real plugin events (fake RisuAI API) as the Go ingest test fixture.
// usage: node tools/make-ingest-fixture.cjs > go-service/internal/ingest/testdata/plugin-events.json
const fs = require("fs");
const path = require("path");
const vm = require("vm");
const SRC = fs.readFileSync(path.join(__dirname, "..", "plugin", "archive-center-2.js"), "utf8");
const Y = (id, t) => `<!-- yumi-tr:v1:${id}:start -->${t}<!-- yumi-tr:v1:${id}:end -->`;
const chat = (reply) => ({
  id: "chat-A",
  message: [
    { role: "char", chatId: "g0", data: "greeting" },
    { role: "user", chatId: "u1", data: "첫 입력", time: 100 },
    { role: "user", chatId: "u2", data: "둘째 입력 😀🇰🇷", time: 101 },
    { role: "char", chatId: "a1", generationInfo: { generationId: "gen-1" }, data: Y("ab12", reply) },
    { role: "char", chatId: "a2", data: Y("zz99", "기록 없는 번역") },
    { role: "char", isComment: true, disabled: true, data: "branch" },
    { role: "user", chatId: "u3", disabled: "allBefore", data: "x" },
    { role: "user", chatId: "u4", data: "lone \ud800 surrogate" },
  ],
  scriptstate: { "$__yumi_tr.ab12": 'u:{"model":"ORIGINAL REPLY"}', mood: "kept private" },
});
const events = [];
let current = chat("이벤트 번역");
const handlers = { input: [], beforeRequest: [], afterRequest: [], output: [] };
const api = {
  getArgument: async (k) => ({ ac2_url: "http://backend" })[k],
  getLocalPluginStorage: async () => ({ getItem: async () => null, setItem: async () => {} }),
  addRisuScriptHandler: async (m, f) => handlers[m].push(f), removeRisuScriptHandler: async () => {},
  addRisuReplacer: async (m, f) => handlers[m].push(f), removeRisuReplacer: async () => {},
  addRisuChatListener: async (m, f) => handlers[m].push(f), removeRisuChatListener: async () => {},
  getCurrentCharacterIndex: async () => 2, getCurrentChatIndex: async () => 5,
  getChatFromIndex: async () => current,
  parseRisuChat: async () => "", getRuntimeInfo: async () => ({ platform: "node" }),
  onUnload: (f) => { api._unload = f; },
  nativeFetch: async (url, init) => {
    if (init && init.body) events.push(...JSON.parse(init.body).events);
    return { ok: true, status: 200, text: async () => "{}" };
  },
};
(async () => {
  vm.runInContext(SRC, vm.createContext({ Risuai: api, console: { warn() {} }, setTimeout, clearTimeout, AbortController, Date, Math, JSON, Promise }));
  await new Promise((r) => setTimeout(r, 30));
  await handlers.beforeRequest[0]([{ role: "user", content: "p" }], "model");
  const atEvent = chat("이벤트 번역");
  current = chat("나중 번역"); // the re-read sees a later edit
  current.id = "chat-A";
  await handlers.output[0]({ characterIndex: 2, chatIndex: 5, messageIndex: 3, chat: atEvent });
  const noId = chat("x"); delete noId.id;
  current = noId;
  await handlers.beforeRequest[0]([], "model");
  await new Promise((r) => setTimeout(r, 50));
  await api._unload();
  process.stdout.write(JSON.stringify(events.filter((e) => ["load", "beforeRequest", "output"].includes(e.hook)), null, 1));
})();
