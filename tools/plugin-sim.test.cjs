// Runs the real plugin file against a fake RisuAI v3 API (node:test).
// It checks host-side duties only: ordered durable sending, never changing
// the request/response, and keeping snapshot sources apart.
// usage: node --test tools/plugin-sim.test.cjs
const test = require("node:test");
const assert = require("node:assert");
const fs = require("fs");
const path = require("path");
const vm = require("vm");

const SRC = fs.readFileSync(path.join(__dirname, "..", "plugin", "archive-center-2.js"), "utf8");
const tick = (ms = 0) => new Promise((r) => setTimeout(r, ms));
// as RisuAI and PocketRisu do, a declared argument the user never changed
// is 0 (int) or "" (string), never the default its description names
const ARG_START = Object.fromEntries([...SRC.matchAll(/^\/\/@arg (\S+) (int|string)/gm)].map((m) => [m[1], m[2] === "int" ? 0 : ""]));

function makeHost(state) {
  const handlers = { input: [], beforeRequest: [], afterRequest: [], output: [] };
  const storeData = state.storeData;
  const api = {
    getArgument: async (k) => ({ ...ARG_START, ac2_url: "http://backend", ...(state.captureOff ? { ac2_hash_only: 1 } : {}) })[k],
    getLocalPluginStorage: async () => ({
      getItem: async (k) => (k in storeData ? JSON.parse(JSON.stringify(storeData[k])) : null),
      setItem: async (k, v) => {
        if (state.hangStorage) await new Promise(() => {});
        if (state.deferStorage) {
          state.pendingSaves = (state.pendingSaves || 0) + 1;
          state.maxPendingSaves = Math.max(state.maxPendingSaves || 0, state.pendingSaves);
          await new Promise((r) => state.releaseSaves.push(r));
          state.pendingSaves--;
        }
        storeData[k] = JSON.parse(JSON.stringify(v));
      },
    }),
    addRisuScriptHandler: async (m, fn) => { handlers[m].push(fn); },
    // safeLocalStorage is the browser of one device (state.local); without
    // it the host has none
    ...(state.local ? { safeLocalStorage: {
      getItem: async (k) => (k in state.local ? state.local[k] : null),
      setItem: async (k, v) => { state.local[k] = String(v); },
    } } : {}),
    removeRisuScriptHandler: async () => {},
    addRisuReplacer: async (m, fn) => { handlers[m].push(fn); },
    removeRisuReplacer: async () => {},
    ...(state.pdfHook ? {
      registerBodyIntercepter: async (fn) => { state.pdfBody = fn; return { id: "pdf-hook" }; },
      unregisterBodyIntercepter: async (id) => { assert.strictEqual(id, "pdf-hook"); state.pdfUnregistered = true; },
    } : {}),
    addRisuChatListener: async (m, fn) => { handlers[m].push(fn); },
    removeRisuChatListener: async () => {},
    getCurrentChatIndex: async () => 0,
    getChatFromIndex: async () => state.rereadChat,
    parseRisuChat: async (e) => (e === "{{user}}" ? "Tester" : e === "{{char}}" ? "Lina" : ""),
    getCurrentCharacterIndex: async () => state.charIndex ?? 0,
    getRuntimeInfo: async () => ({ apiVersion: "3.0", platform: "node" }),
    onUnload: (fn) => { state.unload = fn; },
    registerButton: async (arg, cb) => { (state.buttons = state.buttons || []).push({ ...arg, cb }); return { id: arg.id }; },
    nativeFetch: async (url, init) => {
      // memory delivery is answered on its own (state.injects); observation
      // sending below is what the gates and acknowledgements act on
      if (url.endsWith("/memory/inject")) {
        (state.injects = state.injects || []).push(JSON.parse(init.body));
        if (!state.backendUp) throw new Error("connect refused");
        return { ok: true, status: 200, text: async () => JSON.stringify(state.injectReply || { block: "" }) };
      }
      if (url.includes("/status")) {
        (state.statusAsks = state.statusAsks || []).push(url);
        if (!state.backendUp) throw new Error("connect refused");
        // the state as the backend had it when asked; statusGate holds the answer
        const chip = state.chip || "done";
        if (state.statusGate) await state.statusGate;
        return { ok: true, status: 200, text: async () => JSON.stringify({ chip }) };
      }
      state.calls.push({ url, body: init && init.body ? JSON.parse(init.body) : null, token: init && init.headers ? init.headers["X-AC2-Token"] : undefined });
      if (state.gate) await state.gate;
      if (!state.backendUp) throw new Error("connect refused");
      const reply = state.resendNext ? '{"status":"ok","resend_text":true}' : '{"status":"ok"}';
      state.resendNext = false;
      return { ok: true, status: 200, text: async () => reply };
    },
  };
  return { api, handlers };
}

async function loadPlugin(state) {
  const host = makeHost(state);
  // Long timers (the 10 s retry) are captured when the test asks, so it can
  // fire them; short ones (fetch timeout) run normally.
  // With fetchTimers, the 8 s request timeout is captured as well.
  const st = (fn, ms) => {
    // the status chip's poll (15 s) is held for the test to fire
    if (ms === 15000) { const h = { captured: true, fn }; (state.chipTimers = state.chipTimers || []).push(h); return h; }
    if (state.fetchTimers && ms >= 8000 && ms < 10000) { const h = { captured: true, fn }; state.fetchTimers.push(h); return h; }
    if (state.longTimers && ms >= 10000) { state.longTimers.push(fn); return { captured: true }; }
    return setTimeout(fn, ms);
  };
  const ct = (h) => { if (h && h.captured) { h.cleared = true; return; } clearTimeout(h); };
  // Maps the plugin creates are tracked so a test can see what they hold.
  state.maps = [];
  class TrackedMap extends Map { constructor(...a) { super(...a); state.maps.push(this); } }
  // browsers have TextEncoder; withoutTextEncoder exercises the fallback
  const ctx = vm.createContext({ Risuai: host.api, console: { warn() {}, log() {}, info: (...args) => { (state.diagnostics = state.diagnostics || []).push(args); } }, setTimeout: st, clearTimeout: ct, AbortController, Date, Math, JSON, Promise, Map: TrackedMap, ...(state.withoutTextEncoder ? {} : { TextEncoder }) });
  await vm.runInContext(SRC, ctx);
  await tick(20);
  return host.handlers;
}

// A memory text as the backend writes it: its title with the fingerprint
// of the lines after it (ingest.Fingerprint: djb2 and FNV-1a over UTF-16).
function memoryBlock(body) {
  let x = 5381, y = 0x811c9dc5;
  for (let i = 0; i < body.length; i++) { const c = body.charCodeAt(i); x = (Math.imul(x, 33) + c) >>> 0; y = Math.imul(y ^ c, 0x01000193) >>> 0; }
  return "[Archive Center: records of earlier turns of this story | " + x.toString(16).padStart(8, "0") + y.toString(16).padStart(8, "0") + "]\n" + body;
}

const chat = (rows) => ({ id: "chat-1", message: rows, scriptstate: { "$__yumi_tr.ab": '{"model":"orig"}', other: "secret-ish" } });

test("E11 OpenAI-compatible PDF (llm_gateway_pdf): the block leaves the messages and the PDF goes on the last user message as a file part; other routes keep the text", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), pdfHook: true };
  const h = await loadPlugin(state);
  const block = memoryBlock("선택된 기억\n리나는 약사다");
  const request = [{ role: "system", content: "keep instructions" }, { role: "user", content: "now" }];
  const prepare = async (data = "PDF-gw") => {
    state.injectReply = { block, transport: { mode: "llm_gateway_pdf", status: "ready", text: block, pdf_text: block, retained_text: "", mime_type: "application/pdf", pdf_base64: data } };
    return h.beforeRequest[0](request, "model");
  };
  const openai = (messages, parts = false) => ({ model: "m", messages: messages.map((m) => m.role === "user" && parts ? { role: "user", content: [{ type: "text", text: m.content }, { type: "image_url", image_url: { url: "pic" } }] } : { role: m.role, content: m.content }) });
  const out = await prepare();
  assert.ok(out.some((m) => typeof m.content === "string" && m.content.includes("ac2-pdf:")), "marked block in the request");
  const raw = openai(out);
  const saved = JSON.stringify(raw);
  const done = JSON.parse(await state.pdfBody(saved, "openai_basic"));
  assert.strictEqual(JSON.stringify(raw), saved, "original body untouched");
  assert.ok(!JSON.stringify(done).includes("선택된 기억"), "the block text left the messages");
  const user = done.messages.at(-1);
  assert.deepStrictEqual(user.content, [{ type: "text", text: "now" }, { type: "file", file: { filename: "archive-center-memory.pdf", file_data: "data:application/pdf;base64,PDF-gw" } }]);
  assert.strictEqual(done.messages[0].content, "keep instructions");
  assert.strictEqual(await state.pdfBody(JSON.stringify(done), "openai_streaming"), JSON.stringify(done), "already transformed retry is unchanged");
  // parts kept, the file added once
  const withParts = await prepare("PDF-2");
  const pd = await state.pdfBody(openai(withParts, true), "openai_streaming");
  assert.deepStrictEqual(pd.messages.at(-1).content.map((p) => p.type), ["text", "image_url", "file"]);
  assert.strictEqual(pd.messages.at(-1).content[2].file.file_data, "data:application/pdf;base64,PDF-2");
  // a Google route for this mode keeps the text, as an OpenAI route does for google_pdf
  const other = await prepare("PDF-3");
  const gem = { contents: [{ role: "user", parts: [{ text: other.find((m) => m.content.includes("Archive Center:")).content + "\nnow" }] }] };
  const kept = await state.pdfBody(gem, "gemini_base");
  assert.ok(kept.contents[0].parts[0].text.includes("리나는 약사다") && !kept.contents[0].parts[0].text.includes("ac2-pdf:"), "text retained without the marker");
  assert.strictEqual(kept.contents[0].parts.length, 1);
});

test("PDF transport uses request markers, preserves text failures, and delegates Yumi", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), pdfHook: true };
  const h = await loadPlugin(state);
  const block = memoryBlock("선택된 기억\n비밀: 본인만 아는 조건");
  const request = [{ role: "system", content: "keep instructions" }, { role: "user", content: "now" }];
  const prepare = async (text = block, mode = "google_pdf", data = "PDF-one") => {
    state.injectReply = { block: text, transport: { mode, status: mode === "provider_manager_pdf" ? "delegated" : "ready", text, pdf_text: text, retained_text: "", mime_type: "application/pdf", pdf_base64: data } };
    return h.beforeRequest[0](request, "model");
  };
  const body = (messages, user = true, stringParts = false) => ({ systemInstruction: { parts: [{ text: "keep instructions\n" + messages.find((m) => m.content.includes("Archive Center:")).content }] }, contents: user ? [{ role: "user", parts: stringParts ? "now" : [{ text: "now" }, { inlineData: { mimeType: "image/png", data: "picture" } }] }] : [] });
  try {
    const out = await prepare();
    const raw = body(out);
    const saved = JSON.stringify(raw);
    const transformed = JSON.parse(await state.pdfBody(saved, "gemini_base"));
    assert.ok(state.diagnostics.some((entry) => entry[1].status === "applied"));
    assert.ok(!JSON.stringify(state.diagnostics).includes("PDF-one"), "diagnostics exclude PDF bytes");
    assert.ok(!JSON.stringify(state.diagnostics).includes(block), "diagnostics exclude selected memory");
    assert.strictEqual(JSON.stringify(raw), saved, "original body untouched");
    assert.strictEqual(transformed.systemInstruction.parts[0].text, "keep instructions\n");
    assert.strictEqual(transformed.contents[0].parts[0].text, "now");
    assert.strictEqual(transformed.contents[0].parts[1].inlineData.data, "picture");
    assert.strictEqual(transformed.contents[0].parts[2].inlineData.data, "PDF-one");
    assert.strictEqual(await state.pdfBody(JSON.stringify(transformed), "gemini_base"), JSON.stringify(transformed), "already transformed retry is unchanged");
    const rawRetry = JSON.parse(await state.pdfBody(saved, "gemini_base"));
    assert.strictEqual(rawRetry.systemInstruction.parts[0].text, "keep instructions\n", "latest raw-original retry reuses its PDF");
    assert.strictEqual(rawRetry.contents[0].parts[2].inlineData.data, "PDF-one");
    const retry = await h.beforeRequest[0](out, "model");
    assert.strictEqual(retry.filter((m) => m.content.includes("Archive Center:")).length, 1, "authentic marked block replaced after consumption");
    const retryPDF = JSON.parse(await state.pdfBody(JSON.stringify(body(retry, true, true)), "gemini_base_stream"));
    assert.deepStrictEqual(retryPDF.contents[0].parts, [{ text: "now" }, { inlineData: { mimeType: "application/pdf", data: "PDF-one" } }]);
    const absent = await prepare();
    const noUser = await state.pdfBody(body(absent, false), "gemini_base");
    assert.strictEqual(noUser.systemInstruction.parts[0].text, "keep instructions\n" + block);
    const unsupported = await prepare();
    const other = { messages: unsupported };
    const otherResult = await state.pdfBody(other, "openai_basic");
    assert.strictEqual(otherResult.messages[1].content, block);
    assert.ok(other.messages[1].content.includes("ac2-pdf:"), "unsupported original untouched");
    const a = await prepare(block, "google_pdf", "A");
    const bBlock = memoryBlock("다른 요청의 기억");
    const b = await prepare(bBlock, "google_pdf", "B");
    const [br, ar] = await Promise.all([state.pdfBody(body(b), "gemini_base"), state.pdfBody(body(a), "gemini_base")]);
    assert.strictEqual(br.contents[0].parts.at(-1).inlineData.data, "B");
    assert.strictEqual(ar.contents[0].parts.at(-1).inlineData.data, "A");
    const olderReplay = await state.pdfBody(body(b), "gemini_base");
    assert.strictEqual(olderReplay.systemInstruction.parts[0].text, "keep instructions\n" + bBlock, "only latest completed envelope retained; old raw replay keeps text");
    assert.strictEqual(olderReplay.contents[0].parts.length, 2);
    const sameA = await prepare(block, "google_pdf", "same-A");
    const sameB = await prepare(block, "google_pdf", "same-B");
    assert.strictEqual((await state.pdfBody(body(sameB), "gemini_base")).contents[0].parts.at(-1).inlineData.data, "same-B");
    assert.strictEqual((await state.pdfBody(body(sameA), "gemini_base")).contents[0].parts.at(-1).inlineData.data, "same-A");
    const merged = await prepare();
    const metadata = { usageMetadata: { totalTokenCount: 4 } };
    assert.strictEqual(await state.pdfBody(metadata, "meta_gemini"), metadata);
    const mergedBody = { contents: [{ role: "user", parts: [{ text: "surrounding\nsystem: " + merged[1].content + "\nnow" }] }] };
    const mergedResult = await state.pdfBody(mergedBody, "gemini_base");
    assert.strictEqual(mergedResult.contents[0].parts[0].text, "surrounding\nsystem: \nnow");
    assert.strictEqual(mergedResult.contents[0].parts[1].inlineData.data, "PDF-one");
    const quoted = await prepare();
    const markerProse = "ordinary | ac2-pdf:unrelated]\nprose";
    const quotedBody = body(quoted);
    quotedBody.contents[0].parts[0].text = "quotation:\n" + block + "\n" + markerProse;
    quotedBody.metadata = { note: markerProse };
    const quotedResult = await state.pdfBody(quotedBody, "gemini_base");
    assert.strictEqual(quotedResult.contents[0].parts[0].text, quotedBody.contents[0].parts[0].text, "unmarked canonical quotation and marker-like prose preserved");
    assert.strictEqual(quotedResult.metadata.note, markerProse);
    assert.strictEqual(quotedResult.systemInstruction.parts[0].text, "keep instructions\n", "only exact marked block removed");
    const bodyWithMarkerProse = memoryBlock("body | ac2-pdf:unrelated]\nretained");
    const markedProse = await prepare(bodyWithMarkerProse);
    const repeatedProse = await h.beforeRequest[0](markedProse, "model");
    assert.strictEqual(repeatedProse.length, markedProse.length, "authentic header recognition never rewrites memory body before fingerprinting");
    const proseResult = await state.pdfBody(body(repeatedProse), "unsupported");
    assert.strictEqual(proseResult.systemInstruction.parts[0].text, "keep instructions\n" + bodyWithMarkerProse);
    const text = await prepare(block, "text");
    const textBody = body(text);
    assert.strictEqual(await state.pdfBody(textBody, "gemini_base"), textBody, "ordinary text unchanged");
    const pendingAcrossMode = await prepare(block, "google_pdf", "pending-mode");
    await prepare(block, "provider_manager_pdf");
    assert.strictEqual((await state.pdfBody(body(pendingAcrossMode), "gemini_base")).contents[0].parts.at(-1).inlineData.data, "pending-mode", "mode switch does not retarget an already pending request");
    const pm = await prepare(block, "provider_manager_pdf");
    assert.strictEqual(pm[1].content, "<pm-pdf>\n" + block + "\n</pm-pdf>");
    const pmRetry = await h.beforeRequest[0](pm, "model");
    assert.strictEqual(pmRetry.length, pm.length, "Yumi authentic wrapper remains single");
    const emptyPM = await prepare("", "provider_manager_pdf");
    assert.strictEqual(emptyPM[1].content, "<pm-pdf>\n\n</pm-pdf>", "empty selected span prevents Yumi automatic whole-prompt conversion");
    const emptyRetry = await h.beforeRequest[0](emptyPM, "model");
    assert.strictEqual(emptyRetry.length, emptyPM.length, "empty transport wrapper remains single on retry");
    const spoof = [{ role: "system", content: "<pm-pdf>\nordinary text\n</pm-pdf>" }, ...request];
    assert.ok((await h.beforeRequest[0](spoof, "model")).some((m) => m.content === spoof[0].content));
    const aborted = await prepare(block, "google_pdf", "aborted");
    const abandoned = body(aborted);
    await state.unload();
    const afterUnload = await state.pdfBody(abandoned, "gemini_base");
    assert.strictEqual(afterUnload.systemInstruction.parts[0].text, "keep instructions\n" + block, "abandoned context cleared on unload");
    assert.strictEqual(afterUnload.contents[0].parts.length, 2);
  } finally { if (!state.pdfUnregistered) await state.unload(); }
  assert.strictEqual(state.pdfUnregistered, true);
});

test("PDF transport missing hook or backend failure keeps canonical memory text", async () => {
  for (const pdfHook of [false, true]) {
    const block = memoryBlock("텍스트 기준본");
    const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), pdfHook,
      injectReply: { block, transport: { mode: "google_pdf", status: pdfHook ? "failed" : "ready", text: block, error: "pdf unavailable", pdf_base64: "unused" } } };
    const h = await loadPlugin(state);
    try {
      const result = await h.beforeRequest[0]([{ role: "user", content: "now" }], "model");
      assert.strictEqual(result[0].content, block);
    } finally { await state.unload(); }
  }
});

test("PDF lanes keep protected selected memory outside Google and Yumi PDFs", async () => {
  const normal = memoryBlock("ordinary selected fact");
  const retained = memoryBlock("secret: only Lina knows; hide from Doyun");
  const block = memoryBlock("ordinary selected fact\nsecret: only Lina knows; hide from Doyun");
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), pdfHook: true };
  const h = await loadPlugin(state);
  const request = [{ role: "system", content: "keep instructions" }, { role: "user", content: "now" }];
  try {
    for (const mode of ["google_pdf", "provider_manager_pdf"]) {
      state.injectReply = { block, transport: { mode, status: mode === "google_pdf" ? "ready" : "delegated", text: block, pdf_text: normal, retained_text: retained, pdf_base64: "ordinary-pdf" } };
      const out = await h.beforeRequest[0](request, "model");
      assert.strictEqual(out[2].content, retained);
      assert.strictEqual(out[0].content, request[0].content);
      assert.strictEqual(out[3].content, request[1].content);
      if (mode === "provider_manager_pdf") {
        assert.strictEqual(out[1].content, "<pm-pdf>\n" + normal + "\n</pm-pdf>");
      } else {
        const body = { systemInstruction: { parts: [{ text: out[1].content + "\n" + out[2].content }] }, contents: [{ role: "user", parts: [{ text: "now" }] }] };
        const result = await state.pdfBody(body, "gemini_base");
        assert.strictEqual(result.systemInstruction.parts[0].text, "\n" + retained);
        assert.strictEqual(result.contents[0].parts[1].inlineData.data, "ordinary-pdf");
        const unsupported = await state.pdfBody(body, "unsupported");
        assert.strictEqual(unsupported.systemInstruction.parts[0].text, normal + "\n" + retained);
      }
      const retried = await h.beforeRequest[0](out, "model");
      assert.strictEqual(retried.length, out.length, "each authentic lane replaced once on retry");
    }
    state.injectReply = { block: retained, transport: { mode: "provider_manager_pdf", status: "delegated", text: retained, pdf_text: "", retained_text: retained } };
    const secretOnly = await h.beforeRequest[0](request, "model");
    assert.strictEqual(secretOnly[1].content, "<pm-pdf>\n\n</pm-pdf>");
    assert.strictEqual(secretOnly[2].content, retained);
  } finally { await state.unload(); }
});

test("unsent observations survive backend outage and reload, then go out in order", async () => {
  const state = { storeData: {}, calls: [], backendUp: false, rereadChat: chat([]) };
  let h = await loadPlugin(state); // boot 1, backend down
  await h.input[0]("first input");
  await state.unload(); // page reload
  const saved = state.storeData.ac2_outbox_v1.queue.map((e) => e.hook);
  assert.ok(saved.includes("input") && saved.includes("load"), "outbox must hold unsent events: " + saved);

  h = await loadPlugin(state); // reload, still down
  state.backendUp = true;
  state.calls = [];
  await h.input[0]("after reload");
  await tick(50);
  const sent = state.calls.filter((c) => c.url.endsWith("/observe")).flatMap((c) => c.body.events);
  const hooks = sent.map((e) => e.hook + ":" + e.boot_id.slice(-6));
  const boots = [...new Set(sent.map((e) => e.boot_id))];
  assert.strictEqual(boots.length, 2, "events from both boots must be sent: " + hooks);
  const firstBootIdx = sent.findIndex((e) => e.boot_id === boots[1]);
  assert.ok(sent.slice(0, firstBootIdx).every((e) => e.boot_id === boots[0]), "older boot events must come first");
  assert.ok(sent.some((e) => e.hook === "input" && e.content.text === "first input"), "first input lost");
  assert.ok(sent.some((e) => e.hook === "unload"), "unload event lost");
  assert.deepStrictEqual(state.storeData.ac2_outbox_v1.queue, [], "outbox must be empty after success");
  await state.unload();
});

test("request and response are returned unchanged even when the backend is down", async () => {
  const state = { storeData: {}, calls: [], backendUp: false, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  const msgs = [{ role: "user", content: "hi" }];
  const out = await h.beforeRequest[0](msgs, "model");
  assert.strictEqual(out, msgs, "beforeRequest must return the same array");
  assert.deepStrictEqual(out, [{ role: "user", content: "hi" }]);
  const reply = await h.afterRequest[0]("raw reply <thinking>x</thinking>", "model");
  assert.strictEqual(reply, "raw reply <thinking>x</thinking>");
  await state.unload();
});

test("the memory the backend chooses goes in before the latest user message; nothing chosen leaves the request as it is", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  const msgs = [{ role: "system", content: "card" }, { role: "user", content: "old" }, { role: "assistant", content: "reply" }, { role: "user", content: "now" }];
  let out = await h.beforeRequest[0](msgs, "model");
  assert.strictEqual(out, msgs, "an empty block must leave the same array");
  const ask = state.injects[state.injects.length - 1];
  assert.strictEqual(ask.chat_id, "chat-1");
  assert.strictEqual(ask.input, "now", "the latest user message is the input");
  assert.strictEqual(ask.payload.length, 4);
  assert.deepStrictEqual(ask.payload[3], { len: 3, h: ask.payload[3].h, role: "user", text: "now" }, "text capture on sends text beside fingerprints and roles");
  state.injectReply = { block: "[memory]\nM1 fact" };
  out = await h.beforeRequest[0](msgs, "model");
  assert.notStrictEqual(out, msgs);
  assert.deepStrictEqual(out.map((m) => m.role + ":" + m.content), ["system:card", "user:old", "assistant:reply", "system:[memory]\nM1 fact", "user:now"]);
  assert.strictEqual(msgs.length, 4, "the host's array is not changed in place");
  await state.unload();
});

test("delivery carries the request's observation and is not held by observation sending", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([{ role: "user", chatId: "u1", data: "now" }]) };
  const h = await loadPlugin(state);
  await tick(30);
  state.gate = new Promise(() => {}); // /observe never answers
  state.injects = [];
  const out = await Promise.race([h.beforeRequest[0]([{ role: "user", content: "now" }], "model"), tick(500).then(() => "held")]);
  assert.notStrictEqual(out, "held", "delivery waited for observation sending");
  assert.strictEqual(state.injects.length, 1);
  const obs = state.injects[0].observation;
  assert.ok(obs && obs.contract === "observation.v1" && obs.events.length === 1, "observation missing");
  const ev = obs.events[0];
  assert.strictEqual(ev.hook, "beforeRequest");
  assert.strictEqual(ev.stored_chat.chat_id, "chat-1");
  assert.ok(ev.seq > 0 && ev.boot_id, "the event keeps its (boot_id, seq) so the outbox copy is the same event");
  state.gate = null;
  await state.unload();
});

test("with older observations still unsent, delivery carries them before this one, in plugin order", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), gate: new Promise(() => {}) };
  const h = await loadPlugin(state); // the load observation is held unsent
  state.injects = [];
  await h.beforeRequest[0]([{ role: "user", content: "now" }], "model");
  assert.strictEqual(state.injects.length, 1);
  const evs = state.injects[0].observation.events;
  assert.deepStrictEqual(evs.map((e) => e.hook).slice(-1), ["beforeRequest"]);
  assert.ok(evs.some((e) => e.hook === "load"), "the older unsent load observation is carried");
  for (let i = 1; i < evs.length; i++) assert.ok(evs[i].seq > evs[i - 1].seq, "events out of plugin order");
  assert.deepStrictEqual(state.injects[0].payload.map((m) => m.role), ["user"], "message roles are sent with the fingerprints");
  state.gate = null;
  await state.unload();
});

test("a delivery answer asking for texts makes the next observation send them again", async () => {
  const row = { role: "user", chatId: "u1", data: "hello" };
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([row]) };
  const h = await loadPlugin(state);
  await h.beforeRequest[0]([], "m");
  await tick(30);
  state.injectReply = { block: "", resend_text: true };
  await h.beforeRequest[0]([], "m");
  state.injectReply = null;
  await tick(30);
  state.calls = [];
  await h.beforeRequest[0]([], "m");
  await tick(30);
  const sent = state.calls.filter((c) => c.url.endsWith("/observe")).flatMap((c) => c.body.events).filter((e) => e.hook === "beforeRequest");
  assert.ok(sent.length && sent[sent.length - 1].stored_chat.rows[0].data.text === "hello", "texts not sent again after resend_text");
  await state.unload();
});

test("a request handed back to the hook keeps one memory message, also as a copy (the host copies it across the sandbox)", async () => {
  const block = memoryBlock("- M1\n");
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), injectReply: { block } };
  const h = await loadPlugin(state);
  const first = await h.beforeRequest[0]([{ role: "system", content: "card" }, { role: "user", content: "now" }], "model");
  const again = await h.beforeRequest[0](JSON.parse(JSON.stringify(first)), "model");
  assert.strictEqual(again.filter((m) => m.content === block).length, 1, "memory delivered twice");
  assert.deepStrictEqual(again.map((m) => m.role), ["system", "system", "user"]);
  assert.strictEqual(state.injects[state.injects.length - 1].payload.length, 2, "the plugin's own message is not part of the observed request");
  await state.unload();
});

// A narrative guide message as the backend writes it (store.GuideMessage).
function guideMsg(body) {
  return memoryBlock(body).replace("[Archive Center: records of earlier turns of this story | ", "[Archive Center: narrative guide | ");
}

test("narrative support (E03): the guide goes after the memory and, strong, last; the improved input replaces the user's text in this request only; handed back, one of each", async () => {
  const block = memoryBlock("- M1\n");
  const after = guideMsg("[서사 참고]\n이어지는 점:\n- 약속\n"), before = guideMsg("[이번 응답 전개 계획]\n- 시작: 시계\n");
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]),
    injectReply: { block, guide: { after_memory: after, before_reply: before }, input_rewrite: { from: "리나에게 묻는다", to: "리나에게 약속을 묻는다" } } };
  const h = await loadPlugin(state);
  const msgs = [{ role: "system", content: "card" }, { role: "user", content: "old" }, { role: "user", content: "리나에게 묻는다\n(프리셋 뒤 글)" }];
  const out = await h.beforeRequest[0](msgs, "model");
  assert.deepStrictEqual(out.map((m) => m.role + ":" + m.content), ["system:card", "user:old", "system:" + block, "system:" + after, "user:리나에게 약속을 묻는다\n(프리셋 뒤 글)", "system:" + before]);
  assert.strictEqual(msgs[2].content, "리나에게 묻는다\n(프리셋 뒤 글)", "the host's message is not changed in place");
  const again = await h.beforeRequest[0](JSON.parse(JSON.stringify(out)), "model");
  assert.strictEqual(again.filter((m) => m.content === after).length, 1, "guide after memory twice");
  assert.strictEqual(again.filter((m) => m.content === before).length, 1, "guide before reply twice");
  assert.strictEqual(again.filter((m) => m.content.includes("약속을 묻는다")).length, 1, "the input improved twice or lost");
  assert.ok(!state.injects[state.injects.length - 1].payload.some((p) => p.text && p.text.startsWith("[Archive Center: narrative guide")), "the guide is not part of the observed request");
  // a text the request does not hold is not touched; no memory still takes the guide's tail and the rewrite
  state.injectReply = { block: "", guide: { before_reply: before }, input_rewrite: { from: "없는 글", to: "바꾼 글" } };
  const none = await h.beforeRequest[0](msgs, "model");
  assert.deepStrictEqual(none.map((m) => m.content), ["card", "old", "리나에게 묻는다\n(프리셋 뒤 글)", before]);
  // a guide-titled message whose fingerprint does not match is not the plugin's
  const foreign = "[Archive Center: narrative guide | 0000000000000000]\n남의 글\n";
  state.injectReply = { block: "" };
  const kept = await h.beforeRequest[0]([{ role: "system", content: foreign }, { role: "user", content: "x" }], "model");
  assert.strictEqual(kept[0].content, foreign);
  await state.unload();
});

test("narrative support without a memory block: after_memory follows backend placement, before_reply stays last", async () => {
  const after = guideMsg("[서사 참고]\n- 대화에서 이어지는 점\n"), before = guideMsg("[이번 응답 전개 계획]\n- 시작\n");
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  const msgs = [{ role: "system", content: "card" }, { role: "user", content: "old" }, { role: "user", content: "current" }];
  for (const index of [0, 1, 2, 3, undefined]) {
    state.injectReply = { block: "", guide: { after_memory: after, before_reply: before }, ...(index === undefined ? {} : { placement: { index } }) };
    const out = await h.beforeRequest[0](msgs, "model");
    const expected = msgs.map((m) => m.content);
    expected.splice(index === undefined ? 2 : index, 0, after);
    expected.push(before);
    assert.deepStrictEqual(out.map((m) => m.content), expected, "placement " + index);
    assert.deepStrictEqual(out.filter((m) => m.content === after || m.content === before).map((m) => m.role), ["system", "system"]);
    const again = await h.beforeRequest[0](JSON.parse(JSON.stringify(out)), "model");
    assert.deepStrictEqual(again.map((m) => m.content), expected, "handed back, exactly once");
  }
  assert.deepStrictEqual(msgs.map((m) => m.content), ["card", "old", "current"]);
  await state.unload();
});

test("narrative support (review 20 B-01, B-02): the improved input goes into this turn's input only, never a preset after it or an older row, and a request handed back gets it once", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), injectReply: { block: "", input_rewrite: { from: "묻는다", to: "약속을 묻는다" } } };
  const h = await loadPlugin(state);
  // the input itself, then a user-role preset part holding the same words
  const msgs = [{ role: "user", content: "옛날에 묻는다" }, { role: "assistant", content: "답" }, { role: "user", content: "묻는다" }, { role: "user", content: "프리셋 예문: 묻는다" }];
  const out = await h.beforeRequest[0](msgs, "model");
  assert.deepStrictEqual(out.map((m) => m.content), ["옛날에 묻는다", "답", "약속을 묻는다", "프리셋 예문: 묻는다"]);
  const again = await h.beforeRequest[0](JSON.parse(JSON.stringify(out)), "model");
  assert.deepStrictEqual(again.map((m) => m.content), ["옛날에 묻는다", "답", "약속을 묻는다", "프리셋 예문: 묻는다"], "applied twice");
  // the input's text is not in this turn: an older row is never touched
  const gone = await h.beforeRequest[0]([{ role: "user", content: "옛날에 묻는다" }, { role: "assistant", content: "답" }, { role: "user", content: "다른 글" }], "model");
  assert.deepStrictEqual(gone.map((m) => m.content), ["옛날에 묻는다", "답", "다른 글"]);
  await state.unload();
});

test("narrative support: an improved input that is a substring of the original (words removed) still replaces it; one that contains the original is applied once", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), injectReply: { block: "", input_rewrite: { from: "오늘 오늘 갔어", to: "오늘 갔어" } } };
  const h = await loadPlugin(state);
  const out = await h.beforeRequest[0]([{ role: "user", content: "오늘 오늘 갔어" }], "model");
  assert.deepStrictEqual(out.map((m) => m.content), ["오늘 갔어"], "the original went out unchanged");
  const again = await h.beforeRequest[0](JSON.parse(JSON.stringify(out)), "model");
  assert.deepStrictEqual(again.map((m) => m.content), ["오늘 갔어"], "applied twice");
  // the improved text holds the original (a phrase added): handed back, still once
  state.injectReply = { block: "", input_rewrite: { from: "묻는다", to: "(기억) 묻는다 (끝)" } };
  const o2 = await h.beforeRequest[0]([{ role: "user", content: "묻는다" }], "model");
  assert.deepStrictEqual(o2.map((m) => m.content), ["(기억) 묻는다 (끝)"]);
  const a2 = await h.beforeRequest[0](JSON.parse(JSON.stringify(o2)), "model");
  assert.deepStrictEqual(a2.map((m) => m.content), ["(기억) 묻는다 (끝)"], "replaced twice");
  // to is a part of from and another message of the turn also holds from: only the input is changed, also when handed back
  state.injectReply = { block: "", input_rewrite: { from: "오늘 오늘 갔어", to: "오늘 갔어" } };
  const two = [{ role: "user", content: "오늘 오늘 갔어" }, { role: "user", content: "참고: 오늘 오늘 갔어" }];
  const t1 = await h.beforeRequest[0](two, "model");
  assert.deepStrictEqual(t1.map((m) => m.content), ["오늘 갔어", "참고: 오늘 오늘 갔어"]);
  const t2 = await h.beforeRequest[0](JSON.parse(JSON.stringify(t1)), "model");
  assert.deepStrictEqual(t2.map((m) => m.content), ["오늘 갔어", "참고: 오늘 오늘 갔어"], "the other message was rewritten on the second call");
  // one message holds from twice: the last is changed, and a request handed back (twice) keeps that result
  state.injectReply = { block: "", input_rewrite: { from: "오늘 오늘 갔어", to: "오늘 갔어" } };
  const one = [{ role: "user", content: "참고: 오늘 오늘 갔어 / 오늘 오늘 갔어" }];
  const r1 = await h.beforeRequest[0](one, "model");
  assert.deepStrictEqual(r1.map((m) => m.content), ["참고: 오늘 오늘 갔어 / 오늘 갔어"]);
  const r2 = await h.beforeRequest[0](JSON.parse(JSON.stringify(r1)), "model");
  const r3 = await h.beforeRequest[0](JSON.parse(JSON.stringify(r2)), "model");
  assert.deepStrictEqual([r2, r3].map((r) => r[0].content), ["참고: 오늘 오늘 갔어 / 오늘 갔어", "참고: 오늘 오늘 갔어 / 오늘 갔어"], "rewritten again when handed back");
  // a new request with the same original input is rewritten again
  const r4 = await h.beforeRequest[0](one, "model");
  assert.deepStrictEqual(r4.map((m) => m.content), ["참고: 오늘 오늘 갔어 / 오늘 갔어"]);
  // an empty improved text never replaces
  state.injectReply = { block: "", input_rewrite: { from: "묻는다", to: "" } };
  const e1 = await h.beforeRequest[0]([{ role: "user", content: "묻는다" }], "model");
  assert.deepStrictEqual(e1.map((m) => m.content), ["묻는다"]);
  await state.unload();
});

test("with text capture off the input text is not sent for delivery", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), captureOff: true };
  const h = await loadPlugin(state);
  state.injects = [];
  await h.beforeRequest[0]([{ role: "user", content: "private words" }], "model");
  assert.strictEqual(state.injects.length, 1);
  assert.strictEqual(state.injects[0].input, "", "input text sent with capture off");
  assert.ok(!JSON.stringify(state.injects[0]).includes("private words"));
  await state.unload();
});

test("output sends the event snapshot separately from the re-read, with variable values sent once", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { id: "chat-1", message: [], scriptstate: {} } };
  const h = await loadPlugin(state);
  state.rereadChat = chat([{ role: "user", chatId: "u1", data: "hi" }, { role: "char", chatId: "a1", data: "LATER EDIT" }]);
  state.calls = [];
  await h.output[0]({ characterIndex: 0, chatIndex: 0, messageIndex: 1, chat: chat([{ role: "user", chatId: "u1", data: "hi" }, { role: "char", chatId: "a1", generationInfo: { generationId: "g1" }, data: "AT EVENT" }]) });
  await tick(30);
  const ev = state.calls.flatMap((c) => (c.body ? c.body.events : [])).find((e) => e.hook === "output");
  assert.ok(ev, "output event not sent");
  assert.strictEqual(ev.event_chat.rows[1].data.text, "AT EVENT");
  assert.strictEqual(ev.event_chat.rows[1].generation_id, "g1");
  assert.strictEqual(ev.reread_chat.rows[1].data.text, "LATER EDIT");
  assert.strictEqual(ev.event_chat.scriptstate["$__yumi_tr.ab"].text, '{"model":"orig"}');
  assert.strictEqual(ev.event_chat.scriptstate.other.text, "secret-ish", "chat variable values go to the backend (story date)");
  assert.strictEqual(ev.event_chat.scriptstate.other.translation_record, undefined);
  assert.strictEqual(ev.reread_chat.scriptstate.other.text, undefined, "the re-read of the same event carries a value already sent only as its fingerprint");
  assert.strictEqual(ev.reread_chat.scriptstate.other.h, ev.event_chat.scriptstate.other.h);
  // acknowledged and unchanged: the next observation sends only its fingerprint
  state.calls = [];
  await h.output[0]({ characterIndex: 0, chatIndex: 0, messageIndex: 1, chat: chat([{ role: "user", chatId: "u1", data: "hi" }, { role: "char", chatId: "a1", generationInfo: { generationId: "g1" }, data: "AT EVENT" }]) });
  await tick(30);
  const again = state.calls.flatMap((c) => (c.body ? c.body.events : [])).find((e) => e.hook === "output");
  assert.ok(again, "second output not sent");
  assert.strictEqual(again.event_chat.scriptstate.other.text, undefined, "an unchanged variable value is sent once");
  assert.ok(again.event_chat.scriptstate.other.h, "its fingerprint still goes");
  await state.unload();
});

const sentEvents = (state) => state.calls.filter((c) => c.url.endsWith("/observe") && c.ok !== false).flatMap((c) => c.body.events);

test("seq increases by one per observation and is never reused", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  for (let i = 0; i < 5; i++) await h.input[0]("m" + i);
  await tick(50);
  const seqs = sentEvents(state).map((e) => e.seq);
  assert.deepStrictEqual(seqs, seqs.map((_, i) => i + 1), "seq: " + seqs);
  await state.unload();
});

test("a failed send is retried by the timer without any new host event", async () => {
  const state = { storeData: {}, calls: [], backendUp: false, rereadChat: chat([]), longTimers: [] };
  const h = await loadPlugin(state);
  await h.input[0]("waiting");
  await tick(30);
  assert.ok(state.longTimers.length >= 1, "no retry scheduled");
  state.backendUp = true;
  state.calls = [];
  state.longTimers.shift()();
  await tick(50);
  assert.ok(sentEvents(state).some((e) => e.hook === "input" && e.content.text === "waiting"), "retry did not send");
  await state.unload();
});

test("no retry is scheduled after unload", async () => {
  const state = { storeData: {}, calls: [], backendUp: false, rereadChat: chat([]), longTimers: [] };
  await loadPlugin(state);
  await tick(30);
  await state.unload();
  state.longTimers.length = 0;
  await tick(50);
  assert.strictEqual(state.longTimers.length, 0, "retry scheduled after unload");
});

test("overflow while a batch is in flight never removes unsent events", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  await tick(30);
  let release;
  state.gate = new Promise((r) => (release = r));
  state.calls = [];
  await h.input[0]("first");          // goes out and is held
  for (let i = 1; i <= 2001; i++) await h.input[0]("later-" + i);
  state.gate = null;
  release();
  await tick(400);
  const sent = new Set(sentEvents(state).filter((e) => e.hook === "input").map((e) => e.content.text));
  const reported = state.calls.reduce((n, c) => n + (c.body ? c.body.dropped_before || 0 : 0), 0);
  const all = ["first"].concat(Array.from({ length: 2001 }, (_, i) => "later-" + (i + 1)));
  const missing = all.filter((t) => !sent.has(t));
  assert.ok(sent.has("first"), "in-flight event lost");
  assert.strictEqual(missing.length, reported, "missing " + missing.length + " but reported " + reported + ": " + missing.slice(0, 5));
  assert.deepStrictEqual(state.storeData.ac2_outbox_v1.queue, []);
  await state.unload();
});

test("hung plugin storage does not hold beforeRequest", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  state.hangStorage = true;
  const msgs = [{ role: "user", content: "hi" }];
  const out = await Promise.race([h.beforeRequest[0](msgs, "model"), tick(500).then(() => "timeout")]);
  assert.strictEqual(out, msgs, "beforeRequest was held by storage");
});

test("load sends the stored chat for the backend to restore from", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([{ role: "user", chatId: "u1", data: "hi" }]) };
  await loadPlugin(state);
  await tick(30);
  const load = sentEvents(state).find((e) => e.hook === "load");
  assert.ok(load && load.stored_chat && load.stored_chat.rows[0].data.text === "hi", "load without chat");
  await state.unload();
});

test("snapshots name the chat and, for the open character, the character", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { ...chat([{ role: "user", chatId: "u1", data: "hi" }]), name: "첫 채팅" } };
  const h = await loadPlugin(state);
  await tick(30);
  const load = sentEvents(state).find((e) => e.hook === "load");
  assert.deepStrictEqual([load.stored_chat.name, load.stored_chat.char_name], ["첫 채팅", "Lina"]);
  // an output of another character's chat (the user moved on) carries no character name
  state.charIndex = 3;
  await h.output[0]({ characterIndex: 0, chatIndex: 0, messageIndex: 0, chat: state.rereadChat });
  await tick(30);
  const out = sentEvents(state).find((e) => e.hook === "output");
  assert.deepStrictEqual([out.event_chat.name, out.event_chat.char_name], ["첫 채팅", null]);
  await state.unload();
});

const rowsOf = (state, hook) => sentEvents(state).filter((e) => e.hook === hook).map((e) => e.stored_chat.rows);

test("a row's text is sent once; unchanged rows go as fingerprints, changed or reverted rows with text", async () => {
  const msgs = [{ role: "user", chatId: "u1", data: "same" }, { role: "char", chatId: "a1", data: "v1" }];
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { id: "chat-1", message: msgs, scriptstate: {} } };
  const h = await loadPlugin(state);
  await tick(30);
  state.calls = [];
  await h.beforeRequest[0]([], "m");
  await tick(30);
  msgs[1].data = "v2";
  await h.beforeRequest[0]([], "m");
  await tick(30);
  msgs[1].data = "v1"; // earlier candidate chosen again
  await h.beforeRequest[0]([], "m");
  await tick(30);
  const [r1, r2, r3] = rowsOf(state, "beforeRequest");
  assert.strictEqual(r1[0].data.text, undefined, "unchanged row resent with text after the load ack");
  assert.ok(r1[0].data.h && r1[0].data.len === 4, "fingerprint missing");
  assert.strictEqual(r2[1].data.text, "v2");
  assert.strictEqual(r3[1].data.text, "v1", "reverting to an earlier text must send it again");
  await state.unload();
});

test("texts are not marked sent until acknowledged, and resend_text sends everything again", async () => {
  const msgs = [{ role: "user", chatId: "u1", data: "hello" }];
  const state = { storeData: {}, calls: [], backendUp: false, rereadChat: { id: "chat-1", message: msgs, scriptstate: {} } };
  const h = await loadPlugin(state);
  await tick(30);
  await h.beforeRequest[0]([], "m"); // load + this both unacknowledged
  state.backendUp = true;
  state.calls = [];
  await h.beforeRequest[0]([], "m");
  await tick(50);
  const all = rowsOf(state, "beforeRequest");
  assert.ok(all.length >= 2 && all.every((r) => r[0].data.text === "hello"), "text dropped before acknowledgement");
  state.resendNext = true;
  await h.beforeRequest[0]([], "m"); // its ack says resend
  await tick(30);
  state.calls = [];
  await h.beforeRequest[0]([], "m");
  await tick(30);
  assert.strictEqual(rowsOf(state, "beforeRequest")[0][0].data.text, "hello", "resend_text ignored");
  await state.unload();
});

test("the outgoing prompt is sent as fingerprints only", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  state.calls = [];
  await h.beforeRequest[0]([{ role: "system", content: "very long prompt" }], "m");
  await tick(30);
  const ev = sentEvents(state).find((e) => e.hook === "beforeRequest");
  assert.strictEqual(ev.payload.messages[0].content.text, undefined);
  assert.strictEqual(ev.payload.messages[0].content.len, 16);
  await state.unload();
});

test("outbox saves run one at a time, so an older save never lands last", async () => {
  const state = { storeData: {}, calls: [], backendUp: false, rereadChat: chat([]), releaseSaves: [] };
  const h = await loadPlugin(state);
  await tick(30);
  state.deferStorage = true;
  await h.input[0]("a");
  await h.input[0]("b");
  await tick(20);
  for (let i = 0; i < 20 && state.releaseSaves.length; i++) { state.releaseSaves.shift()(); await tick(10); }
  assert.strictEqual(state.maxPendingSaves, 1, "saves overlapped: " + state.maxPendingSaves);
  const texts = state.storeData.ac2_outbox_v1.queue.filter((e) => e.hook === "input").map((e) => e.content.text);
  assert.deepStrictEqual(texts, ["a", "b"], "last save is not the newest outbox");
  state.deferStorage = false;
  await state.unload();
});

test("events dropped by overflow leave no pending text records behind", async () => {
  const msgs = [{ role: "user", chatId: "u1", data: "t0" }];
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { id: "chat-1", message: msgs, scriptstate: {} } };
  const h = await loadPlugin(state);
  await tick(30);
  let release;
  state.gate = new Promise((r) => (release = r));
  for (let i = 1; i <= 2100; i++) { msgs[0].data = "t" + i; await h.beforeRequest[0]([], "m"); }
  state.gate = null;
  release();
  await tick(800);
  assert.deepStrictEqual(state.storeData.ac2_outbox_v1.queue, []);
  const pendingLike = state.maps.filter((m) => [...m.keys()].some((k) => /^[a-z0-9]+-[a-z0-9]+:\d+$/.test(k)));
  assert.strictEqual(pendingLike.reduce((n, m) => n + m.size, 0), 0, "pending text records left after overflow");
  await state.unload();
});

test("a lone surrogate is sent as U+FFFD, as the backend will read it", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { id: "chat-1", message: [{ role: "user", chatId: "u1", data: "a\ud800b" }], scriptstate: {} } };
  await loadPlugin(state);
  await tick(30);
  const load = sentEvents(state).find((e) => e.hook === "load");
  assert.strictEqual(load.stored_chat.rows[0].data.text, "a�b");
  await state.unload();
});

test("an event whose request timed out is not dropped by overflow, and goes out before later ones", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), longTimers: [], fetchTimers: [] };
  const h = await loadPlugin(state);
  await tick(30);
  let release;
  state.gate = new Promise((r) => (release = r));
  state.calls = [];
  await h.input[0]("first");
  await tick(10);
  // the outbox fills while "first" is in flight
  for (let i = 1; i <= 1999; i++) await h.input[0]("later-" + i);
  const timer = state.fetchTimers.find((t) => !t.cleared);
  assert.ok(timer, "request timeout not armed");
  timer.fn(); // the request times out; its fate at the backend is unknown
  await tick(10);
  // the next observations overflow the outbox before any new send
  for (let i = 2000; i <= 2001; i++) await h.input[0]("later-" + i);
  assert.ok(state.storeData.ac2_outbox_v1.queue.some((e) => e.content && e.content.text === "first"), "timed-out event dropped by overflow");
  state.gate = null;
  release();
  for (let i = 0; i < 20 && state.longTimers.length; i++) { state.longTimers.shift()(); await tick(100); }
  await tick(300);
  const sentAll = state.calls.filter((c) => c.url.endsWith("/observe")).flatMap((c) => c.body.events).filter((e) => e.hook === "input").map((e) => e.content.text);
  const firstAfterTimeout = sentAll.indexOf("first", 1);
  assert.ok(firstAfterTimeout >= 0, "timed-out event never resent");
  assert.ok(sentAll.slice(1, firstAfterTimeout).every((t) => t === "first"), "a later event went out before the timed-out one was resent");
  const reported = state.calls.reduce((n, c) => n + (c.body ? c.body.dropped_before || 0 : 0), 0);
  const all = ["first"].concat(Array.from({ length: 2001 }, (_, i) => "later-" + (i + 1)));
  assert.strictEqual(all.filter((t) => !sentAll.includes(t)).length, reported);
  await state.unload();
});

test("large events are sent in requests of at most 8 MiB, so none is stuck", async () => {
  const msgs = [{ role: "user", chatId: "u1", data: "x" }];
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { id: "chat-1", message: msgs, scriptstate: {} } };
  const h = await loadPlugin(state);
  await tick(30);
  let release;
  state.gate = new Promise((r) => (release = r));
  for (let i = 0; i < 60; i++) { msgs[0].data = String(i).padEnd(180000, "y"); await h.beforeRequest[0]([], "m"); }
  state.gate = null;
  release();
  await tick(1500);
  const sizes = state.calls.filter((c) => c.url.endsWith("/observe")).map((c) => JSON.stringify(c.body).length);
  assert.deepStrictEqual(state.storeData.ac2_outbox_v1.queue, [], "events left unsent");
  assert.ok(sizes.slice(1).every((n) => n <= (8 << 20) + 4096), "request over 8 MiB: " + Math.max(...sizes));
  assert.ok(sizes.length > 2, "expected several requests");
  await state.unload();
});

test("acknowledged texts of older chats are dropped; a chat visited again sends its texts once more", async () => {
  const chats = { A: [{ role: "user", chatId: "a1", data: "in A" }], B: [{ role: "user", chatId: "b1", data: "in B" }], C: [{ role: "user", chatId: "c1", data: "in C" }] };
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { id: "A", message: chats.A, scriptstate: {} } };
  const h = await loadPlugin(state);
  const visit = async (id) => { state.rereadChat = { id, message: chats[id], scriptstate: {} }; await h.beforeRequest[0]([], "m"); await tick(30); };
  await visit("A");
  await visit("B");
  await visit("C");
  const lastSentMap = state.maps.find((m) => [...m.keys()].some((k) => /^[ABC]\|c:/.test(k)));
  assert.ok(lastSentMap && ![...lastSentMap.keys()].some((k) => k.startsWith("A|")), "chat A texts still kept");
  state.calls = [];
  await visit("A");
  const ev = sentEvents(state).find((e) => e.hook === "beforeRequest");
  assert.strictEqual(ev.stored_chat.rows[0].data.text, "in A", "revisited chat sent without text");
  await state.unload();
});

// ---- outbox counterexamples, kept as regressions ----

test("an event sent before a reload stays protected and goes out first after a full outbox reload", async () => {
  const rows = [{ role: "user", chatId: "u", data: "question" }, { role: "char", chatId: "a", generationInfo: { generationId: "g" }, data: "answer" }];
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { id: "reload-case", message: rows, scriptstate: {} }, longTimers: [], fetchTimers: [] };
  const h = await loadPlugin(state);
  await tick(30);
  state.calls = [];
  state.gate = new Promise(() => {}); // the request never answers
  await h.output[0]({ characterIndex: 0, chatIndex: 0, messageIndex: 1, chat: state.rereadChat });
  await tick(10);
  await h.beforeRequest[0]([], "m");
  for (let i = 0; i < 1998; i++) await h.input[0]("filler-" + i);
  const lost = state.storeData.ac2_outbox_v1.queue.find((e) => e.hook === "output");
  state.fetchTimers.find((t) => !t.cleared).fn(); // times out
  await tick(30);
  // hard reload: a new plugin instance from the saved outbox
  const next = { storeData: JSON.parse(JSON.stringify(state.storeData)), calls: [], backendUp: true, rereadChat: state.rereadChat, longTimers: [], fetchTimers: [] };
  await loadPlugin(next);
  await tick(300);
  const sent = next.calls.filter((c) => c.url.endsWith("/observe")).flatMap((c) => c.body.events);
  assert.ok(sent.length && sent[0].boot_id === lost.boot_id && sent[0].seq === lost.seq, "attempted output not sent first after reload");
  await next.unload();
});

for (const withoutTextEncoder of [false, true]) {
  test("multi-event requests stay within 8 MiB of UTF-8 for Korean text" + (withoutTextEncoder ? " (no TextEncoder)" : ""), async () => {
    const row = { role: "user", chatId: "u", data: "initial" };
    const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { id: "bytes", message: [row], scriptstate: {} }, withoutTextEncoder };
    const h = await loadPlugin(state);
    await tick(30);
    state.calls = [];
    let release;
    state.gate = new Promise((r) => (release = r));
    for (let i = 0; i < 10; i++) { row.data = i + "한".repeat(800000); await h.beforeRequest[0]([], "m"); }
    state.gate = null;
    release();
    await tick(1500);
    const sizes = state.calls.filter((c) => c.body).map((c) => ({ events: c.body.events.length, utf8: Buffer.byteLength(JSON.stringify(c.body), "utf8") }));
    assert.deepStrictEqual(state.storeData.ac2_outbox_v1.queue, []);
    assert.ok(sizes.filter((s) => s.events > 1).every((s) => s.utf8 <= 8 << 20), "over 8 MiB: " + JSON.stringify(sizes));
    await state.unload();
  });
}

test("a late acknowledgement does not bring back texts of chats already dropped", async () => {
  const mk = (id) => ({ id, message: [{ role: "user", chatId: "u", data: "text-" + id }], scriptstate: {} });
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: mk("A") };
  const h = await loadPlugin(state);
  await tick(30);
  let release;
  state.gate = new Promise((r) => (release = r));
  for (const id of ["A", "B", "C", "D"]) { state.rereadChat = mk(id); await h.beforeRequest[0]([], "m"); }
  state.gate = null;
  release();
  await tick(200);
  const keys = state.maps.flatMap((m) => [...m.keys()]).filter((k) => /^[ABCD]\|c:/.test(k));
  assert.ok(keys.every((k) => k.startsWith("C|") || k.startsWith("D|")), "kept: " + keys);
  await state.unload();
});

// ---- slow storage counterexamples, kept as regressions ----

test("with slow device storage no batch leaves before its sent-mark is saved, and an output survives a full reload", async () => {
  const rows = [{ role: "user", chatId: "u", data: "question" }, { role: "char", chatId: "a", generationInfo: { generationId: "g" }, data: "answer" }];
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: { id: "reload-case", message: rows, scriptstate: {} } };
  const h = await loadPlugin(state);
  await tick(30);
  state.calls = [];
  let release;
  state.gate = new Promise((r) => (release = r));
  await h.input[0]("blocker"); // its request is held
  await h.output[0]({ characterIndex: 0, chatIndex: 0, messageIndex: 1, chat: state.rereadChat });
  await h.beforeRequest[0]([], "m");
  for (let i = 0; i < 1997; i++) await h.input[0]("filler-" + i);
  await tick(30);
  const output = state.storeData.ac2_outbox_v1.queue.find((e) => e.hook === "output");
  // device storage stalls; the held request is then answered
  state.hangStorage = true;
  state.gate = new Promise(() => {});
  release();
  await tick(50);
  const sentNow = state.calls.filter((c) => c.url.endsWith("/observe")).flatMap((c) => c.body.events);
  assert.ok(!sentNow.some((e) => e.seq === output.seq && e.boot_id === output.boot_id), "a batch left before its sent-mark was saved");
  // hard reload from the last completed save
  const next = { storeData: JSON.parse(JSON.stringify(state.storeData)), calls: [], backendUp: true, rereadChat: state.rereadChat };
  await loadPlugin(next);
  await tick(300);
  const wire = next.calls.filter((c) => c.url.endsWith("/observe")).flatMap((c) => c.body.events);
  assert.ok(wire.some((e) => e.seq === output.seq && e.boot_id === output.boot_id), "the output was dropped on reload");
  await next.unload();
});

test("overflow drops other observations before an output", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  await tick(30);
  let release;
  state.gate = new Promise((r) => (release = r));
  await h.input[0]("blocker");
  await h.output[0]({ characterIndex: 0, chatIndex: 0, messageIndex: 0, chat: chat([]) });
  for (let i = 0; i < 2100; i++) await h.input[0]("filler-" + i);
  assert.ok(state.storeData.ac2_outbox_v1.queue.some((e) => e.hook === "output"), "output dropped while inputs remained");
  state.gate = null;
  release();
  await tick(500);
  await state.unload();
});

test("a multi-event request is at most 8 MiB including the request around the events", async () => {
  const s = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(s);
  await tick(30);
  s.calls = [];
  let release;
  s.gate = new Promise((r) => (release = r));
  await h.input[0]("block");
  await tick(10);
  const probe = JSON.parse(JSON.stringify(s.calls.at(-1).body.events.at(-1)));
  const estimate = (seq, n) => { const e = JSON.parse(JSON.stringify(probe)); e.seq = seq; e.content = { len: n, h: "0".repeat(16), text: "x".repeat(n) }; return Buffer.byteLength(JSON.stringify(e), "utf8") + 1; };
  const n1 = 4194000;
  const n2 = (8 << 20) - estimate(probe.seq + 1, n1) - estimate(probe.seq + 2, n1) + n1; // the two events alone fill 8 MiB
  await h.input[0]("x".repeat(n1));
  await h.input[0]("x".repeat(n2));
  s.gate = null;
  release();
  await tick(400);
  const sizes = s.calls.filter((c) => c.body).map((c) => ({ events: c.body.events.length, utf8: Buffer.byteLength(JSON.stringify(c.body), "utf8") }));
  assert.ok(sizes.every((x) => x.events === 1 || x.utf8 <= 8 << 20), "over 8 MiB: " + JSON.stringify(sizes));
  await s.unload();
});

test("two devices on one server storage keep their own outboxes", async () => {
  // PocketRisu: plugin storage is on the server, one for every device
  const server = {};
  const a = { storeData: server, local: {}, calls: [], backendUp: false, rereadChat: chat([]) };
  const b = { storeData: server, local: {}, calls: [], backendUp: false, rereadChat: chat([]) };
  let ha = await loadPlugin(a);
  const hb = await loadPlugin(b);
  await ha.input[0]("from device A");
  await hb.input[0]("from device B");
  await tick(20);
  await b.unload();
  await a.unload();
  const keys = Object.keys(server).filter((k) => k.startsWith("ac2_outbox_v1"));
  assert.strictEqual(keys.length, 2, "one outbox per device: " + keys);
  ha = await loadPlugin(a); // device A reloads and the backend is back
  a.backendUp = true;
  a.calls = [];
  await ha.input[0]("A again");
  await tick(50);
  const sent = a.calls.filter((c) => c.url.endsWith("/observe")).flatMap((c) => c.body.events);
  assert.ok(sent.some((e) => e.hook === "input" && e.content.text === "from device A"), "device A's unsent input lost");
  assert.ok(!sent.some((e) => e.content && e.content.text === "from device B"), "device A sent device B's outbox");
  const bKey = keys.find((k) => k.endsWith(":" + b.local.ac2_device_id));
  assert.ok(server[bKey].queue.some((e) => e.hook === "input" && e.content.text === "from device B"), "device B's outbox was overwritten");
  await a.unload();
});

test("a phone on the same PocketRisu server uses the token another device saved; the address stays each device's own", async () => {
  const server = {};
  // device A saved its address and token before the token was shared
  const a = { storeData: server, local: { ac2_conn_v1: JSON.stringify({ url: "http://127.0.0.1:28190", token: "TOK-A" }) }, calls: [], backendUp: true, rereadChat: chat([]) };
  const ha = await loadPlugin(a);
  await ha.input[0]("from the PC");
  await tick(50);
  assert.ok(a.calls.some((c) => c.url.startsWith("http://127.0.0.1:28190/") && c.token === "TOK-A"), "device A: its own address and token");
  // the phone has nothing of its own
  const b = { storeData: server, local: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const hb = await loadPlugin(b);
  await hb.input[0]("from the phone");
  await tick(50);
  const sent = b.calls.filter((c) => c.url.endsWith("/observe"));
  assert.ok(sent.length > 0 && sent.every((c) => c.token === "TOK-A"), "the phone sent without the shared token: " + JSON.stringify(sent.map((c) => c.token)));
  assert.ok(sent.every((c) => c.url.startsWith("http://backend/")), "the phone keeps its own address (its argument), not device A's");
  await b.unload();
  await a.unload();
});

test("a system message the plugin did not put in stays, even with the same opening words", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  const own = { role: "system", content: "[Archive Center: user installation instructions]" };
  const out = await h.beforeRequest[0]([own, { role: "user", content: "now" }], "model");
  assert.deepStrictEqual(out.map((m) => m.content), [own.content, "now"]);
  await state.unload();
});

test("the carried observations, with the batch around them, stay within 8 MiB", async () => {
  const LIMIT = 8 << 20;
  const row = { role: "user", chatId: "u1", data: "x" };
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([row]), gate: new Promise(() => {}) };
  const h = await loadPlugin(state); // nothing is acknowledged
  state.injects = [];
  await h.beforeRequest[0]([], "m");
  const first = state.injects[0].observation;
  const bytes = (v) => Buffer.byteLength(JSON.stringify(v));
  const around = bytes({ ...first, events: [] });
  const known = first.events.reduce((n, e, i) => n + bytes(e) + (i ? 1 : 0), 0);
  // the next event is the last one with the row text grown: sized so the
  // events alone fit and the batch around them does not
  const last = first.events[first.events.length - 1];
  const target = LIMIT - known - 1 - Math.floor(around / 2);
  row.data = "x".repeat(target - bytes(last) + 1);
  await h.beforeRequest[0]([], "m");
  const ask = state.injects[1];
  if (ask.observation) assert.ok(bytes(ask.observation) <= LIMIT, "observation batch over 8 MiB: " + bytes(ask.observation));
  else assert.ok(known + 1 + (target) <= LIMIT && known + 1 + target + around > LIMIT, "fixture does not sit on the boundary");
  state.gate = null;
  await state.unload();
});

test("the plugin's own memory text handed back after many others is still taken out once (no list to expire)", async () => {
  const first = memoryBlock("- A\n");
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), injectReply: { block: first } };
  const h = await loadPlugin(state);
  const kept = JSON.parse(JSON.stringify(await h.beforeRequest[0]([{ role: "user", content: "now" }], "model")));
  for (let i = 0; i < 25; i++) {
    state.injectReply = { block: memoryBlock("- other " + i + "\n") };
    await h.beforeRequest[0]([{ role: "user", content: "x" + i }], "model");
  }
  state.injectReply = { block: first };
  const again = await h.beforeRequest[0](kept, "model");
  assert.strictEqual(again.filter((m) => m.content === first).length, 1, "own memory text put in twice");
  await state.unload();
});

test("a message with the memory title but a wrong fingerprint is not taken out", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  const h = await loadPlugin(state);
  const fake = { role: "system", content: "[Archive Center: records of earlier turns of this story | 0000000000000000]\n- M1\n" };
  const out = await h.beforeRequest[0]([fake, { role: "user", content: "now" }], "model");
  assert.deepStrictEqual(out.map((m) => m.content), [fake.content, "now"]);
  await state.unload();
});

// The chat chip shows the backend's state: one floating button, replaced in
// place by its id when the state changes; asked again by itself only while
// the state changes by itself, and after each memory request.
test("the status chip follows the backend's state", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), chip: "generating" };
  const h = await loadPlugin(state);
  await tick(20);
  const last = () => state.buttons[state.buttons.length - 1];
  assert.strictEqual(state.buttons.length, 1);
  assert.ok(state.statusAsks[0].endsWith("/status?chat_id=chat-1"), "asked without the open chat: " + state.statusAsks[0]);
  assert.strictEqual(last().id, "ac2-chip");
  assert.strictEqual(last().location, "chat"); // the chat menu, not over other screens' close button
  assert.ok(last().name.includes("기억 만드는 중"), last().name);
  assert.strictEqual(state.chipTimers.length, 1, "a changing state is asked again");
  state.chip = "done";
  state.chipTimers.shift().fn();
  await tick(20);
  assert.strictEqual(state.buttons.length, 2);
  assert.strictEqual(last().id, "ac2-chip");
  assert.ok(last().name.includes("기억 정리 완료"), last().name);
  assert.ok(decodeURIComponent(last().icon).includes("#2F8F5B"), "done colour");
  assert.strictEqual(state.chipTimers.length, 0, "a settled state is not polled");
  // the same state again does not replace the button
  const asks = state.statusAsks.length;
  await h.beforeRequest[0]([{ role: "user", content: "x" }], "model");
  await tick(20);
  assert.strictEqual(state.statusAsks.length, asks + 1, "not asked after a memory request");
  assert.strictEqual(state.buttons.length, 2, "same state replaced the button");
  // the backend gone: offline, and asked again until it is back
  state.backendUp = false;
  await h.beforeRequest[0]([{ role: "user", content: "y" }], "model");
  await tick(20);
  assert.ok(last().name.includes("연결할 수 없어요"), last().name);
  assert.strictEqual(state.chipTimers.length, 1);
  await state.unload();
  assert.ok(state.chipTimers[0].cleared, "poll left after unload");
});

test("the action icon keeps its archive outline and uses only its corner dot for every backend state", async () => {
  const colors = { done: "#2F8F5B", generating: "#0E6B63", retrying: "#B7791F", embedding_failed: "#C53030", given_up: "#C53030", no_critic: "#7A776E", offline: "#7A776E" };
  let outline;
  for (const [chip, color] of Object.entries(colors)) {
    const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), chip };
    try {
      await loadPlugin(state);
      const button = state.buttons.at(-1);
      assert.strictEqual(button.id, "ac2-chip");
      assert.strictEqual(button.location, "chat");
      assert.strictEqual(button.iconType, "img");
      assert.ok(button.name.startsWith("Archive Center 2 · "));
      const svg = decodeURIComponent(button.icon.slice("data:image/svg+xml,".length));
      assert.match(svg, /viewBox="0 0 20 20"/);
      assert.ok(!svg.includes("currentColor"));
      const groups = [...svg.matchAll(/<g ([^>]+)>(.*?)<\/g>/g)];
      assert.strictEqual(groups.length, 2, "white underlay then dark archive outline");
      assert.match(groups[0][1], /stroke="#fff"/);
      assert.match(groups[0][1], /stroke-opacity="\.6"/);
      assert.match(groups[0][1], /stroke-width="3"/);
      assert.match(groups[1][1], /stroke="#3A3A3A"/);
      assert.match(groups[1][1], /stroke-width="1\.6"/);
      for (const group of groups) {
        assert.match(group[1], /fill="none"/);
        assert.match(group[2], /<rect [^>]*rx="4"/);
        assert.match(group[2], /<path d="M2 7h14M6 11h6"/);
      }
      assert.strictEqual(groups[0][2], groups[1][2], "the underlay follows the same outline");
      const dots = svg.match(/<circle [^>]+\/>/g) || [];
      assert.strictEqual(dots.length, 1);
      assert.match(dots[0], /cx="15\.5" cy="15\.5" r="3\.5"/);
      assert.ok(dots[0].includes('fill="' + color + '"'));
      assert.match(dots[0], /stroke="#fff" stroke-width="1\.5"/);
      const fixed = svg.replace(dots[0], "");
      assert.ok(!fixed.includes(color), "state color belongs only to the dot");
      if (outline) assert.strictEqual(fixed, outline, "state must not change the archive outline");
      outline = fixed;
    } finally { if (state.unload) await state.unload(); }
  }
});

test("a reply observed asks for the chip again once the backend has it", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), chip: "done" };
  const h = await loadPlugin(state);
  await tick(20);
  const last = () => state.buttons[state.buttons.length - 1];
  assert.ok(last().name.includes("기억 정리 완료"));
  assert.strictEqual((state.chipTimers || []).length, 0, "a settled state is not polled");
  const asks = state.statusAsks.length;
  state.chip = "generating";
  let open;
  state.gate = new Promise((r) => { open = r; });
  await h.output[0]({ characterIndex: 0, chatIndex: 0, messageIndex: 1, chat: chat([{ role: "user", chatId: "u1", data: "hi" }, { role: "char", chatId: "a1", data: "reply" }]) });
  await tick(20);
  assert.strictEqual(state.statusAsks.length, asks, "asked before the backend had the reply");
  open();
  state.gate = null;
  await tick(30);
  assert.ok(sentEvents(state).some((e) => e.hook === "output"), "output not sent");
  assert.strictEqual(state.statusAsks.length, asks + 1, "not asked after the reply was sent");
  assert.ok(last().name.includes("기억 만드는 중"), last().name);
  await state.unload();
});

// The models screen with a stand-in document: the plugin's own screen code
// (from the error texts to the status section), the backend answered here.
test("a key typed for one provider is not sent with another", async () => {
  class E {
    constructor(tag) { this.tag = tag; this.children = []; this.listeners = {}; this._value = ""; this.textContent = ""; }
    setAttribute(k, v) { if (k === "value") this.value = v; else this[k] = String(v); }
    addEventListener(k, v) { this.listeners[k] = v; }
    append(...v) { this.children.push(...v); }
    replaceChildren(...v) { this.children = v; }
    get value() { return this.tag === "select" ? ((this.children.find((c) => c.selected) || this.children[0] || {}).value || "") : this._value; }
    set value(v) { if (this.tag === "select") for (const c of this.children) c.selected = c.value === v; else this._value = String(v); }
    querySelector() { return all(this).find((e) => e.role === "status"); }
  }
  const all = (e) => [e, ...e.children.filter((c) => c instanceof E).flatMap(all)];
  // a provider is picked in its drop-down by the label shown
  const choose = (select, label) => { select.value = select.children.find((o) => text(o) === label).value; return select.listeners.change(); };
  const text = (e) => (typeof e === "string" ? e : (e.textContent || "") + e.children.map(text).join(""));
  const view = { critic: { provider: "openai", model: "a", url: "", reasoning: "", retries: 3, key_set: true, key_last4: "0000", source: "saved" },
    embedding: { provider: "voyageai", model: "voyage-context-4", url: "", key_source: "none", suggestions: { voyageai: ["voyage-context-4"], ollama: ["nomic-embed-text", "bge-m3"] } },
    embed_providers: [{ id: "openai", label: "OpenAI", needs_key: true, default_model: "text-embedding-3-small" }, { id: "voyageai", label: "VoyageAI", needs_key: true, default_model: "voyage-context-4" },
      { id: "ollama", label: "Ollama", default_model: "nomic-embed-text" }, { id: "neuralwatt", label: "Neuralwatt", needs_key: true }],
    providers: [{ id: "openai", label: "OpenAI", needs_key: true }, { id: "vertex", label: "Vertex", needs_key: true }, { id: "custom", label: "Custom", needs_key: true, needs_url: true }] };
  const posts = [];
  const code = SRC.slice(SRC.indexOf("  const ERR_TEXT"), SRC.indexOf("  // Status (9-3)"));
  const ctx = vm.createContext({ document: { createElement: (t) => new E(t), body: new E("body") }, refreshChip() {},
    bridge: async (u, i) => { if (i.method === "POST") posts.push(JSON.parse(i.body)); return { ok: true, json: JSON.parse(JSON.stringify(view)) }; } });
  vm.runInContext(code + "\nthis.run = modelsTab;", ctx);
  const save = async (typed, field, to, again) => {
    const body = new E("div");
    await ctx.run(body);
    const f = (id) => all(body).find((e) => e.id === id);
    f(field).value = typed;
    const pick = (label) => choose(f("ac2-cprov"), label);
    for (const p of to) await pick(p);
    if (again) f(field).value = again;
    await all(body).find((e) => e.tag === "button" && text(e) === "평론가·임베딩 저장").listeners.click();
    return posts[posts.length - 1].critic;
  };
  let c = await save("KEY-FOR-OPENAI", "ac2-ckey", ["Custom"]);
  assert.strictEqual(c.provider, "custom");
  assert.strictEqual(c.key, undefined, "the key typed for OpenAI went with Custom");
  // (the service account JSON field is sent only with Vertex)
  c = await save("KEY-FOR-OPENAI", "ac2-ckey", ["OpenAI"]);
  assert.strictEqual(c.key, "KEY-FOR-OPENAI", "a click on the same provider keeps the key");
  c = await save("", "ac2-ckey", ["Custom"], "KEY-FOR-CUSTOM");
  assert.strictEqual(c.key, "KEY-FOR-CUSTOM", "a key typed after the switch is sent");

  // the embedding section: its provider goes with the save, and its keys
  // follow the same rule
  const embedSave = async (typed, to, again) => {
    const body = new E("div");
    await ctx.run(body);
    const f = (id) => all(body).find((e) => e.id === id);
    f("ac2-ekey").value = typed;
    for (const label of to) await choose(f("ac2-eprov"), label);
    if (again) f("ac2-ekey").value = again;
    await all(body).find((e) => e.tag === "button" && text(e) === "평론가·임베딩 저장").listeners.click();
    return { emb: posts[posts.length - 1].embedding, model: f("ac2-emodel").value };
  };
  let e = await embedSave("PA-VOYAGE", ["OpenAI"]);
  assert.strictEqual(e.emb.provider, "openai");
  assert.strictEqual(e.emb.model, "text-embedding-3-small", "the provider's default model is suggested");
  assert.strictEqual(e.emb.key, undefined, "the Voyage key went with OpenAI");
  e = await embedSave("PA-VOYAGE", []);
  assert.deepStrictEqual([e.emb.provider, e.emb.key], ["voyageai", "PA-VOYAGE"]);
  e = await embedSave("", ["Ollama"], "OLLAMA-KEY");
  assert.deepStrictEqual([e.emb.provider, e.emb.key, e.emb.model], ["ollama", "OLLAMA-KEY", "nomic-embed-text"]);

  // the model is picked from the provider's list, or typed after "직접 입력";
  // a provider without a list takes a typed name
  const body = new E("div");
  await ctx.run(body);
  const f = (id) => all(body).find((x) => x.id === id);
  const saveNow = async () => { await all(body).find((x) => x.tag === "button" && text(x) === "평론가·임베딩 저장").listeners.click(); return posts[posts.length - 1].embedding; };
  await choose(f("ac2-eprov"), "Ollama");
  assert.ok(!f("ac2-emodelpick").hidden && f("ac2-emodel").hidden, "the list, not the text field");
  await choose(f("ac2-emodelpick"), "bge-m3");
  assert.strictEqual((await saveNow()).model, "bge-m3");
  // (a save draws the screen again from the backend's view)
  await choose(f("ac2-eprov"), "Ollama");
  await choose(f("ac2-emodelpick"), "직접 입력");
  assert.ok(!f("ac2-emodel").hidden, "직접 입력 opens the text field");
  f("ac2-emodel").value = "my-embed";
  assert.deepStrictEqual(Object.values(await saveNow()).slice(0, 2), ["ollama", "my-embed"]);
  await choose(f("ac2-eprov"), "Neuralwatt");
  assert.ok(f("ac2-emodelpick").hidden && !f("ac2-emodel").hidden, "no list: the text field");
  f("ac2-emodel").value = "nw-embed";
  assert.deepStrictEqual(Object.values(await saveNow()).slice(0, 2), ["neuralwatt", "nw-embed"]);
});

// The worldline screen with a stand-in document: the plugin's own map code,
// the backend answered here (04, 05).
test("the worldline map opens one fold at a time, keeps branch boxes apart and pages a turn's memories", async () => {
  class E {
    constructor(tag, attrs = {}, ...kids) {
      this.tag = tag; this.attrs = {}; this.kids = []; this.events = {}; this.dataset = {}; this.classList = { toggle: () => {} }; this.hidden = false;
      for (const [k, v] of Object.entries(attrs || {})) { if (k === "on") for (const [ev, fn] of Object.entries(v)) this.addEventListener(ev, fn); else this.setAttribute(k, v); }
      this.append(...kids);
    }
    setAttribute(k, v) { this.attrs[k] = String(v); if (k.startsWith("data-")) this.dataset[k.slice(5)] = String(v); }
    append(...kids) { this.kids.push(...kids.filter((k) => k != null)); }
    replaceChildren(...kids) { this.kids = []; this.append(...kids); }
    addEventListener(ev, fn) { (this.events[ev] = this.events[ev] || []).push(fn); }
    async fire(ev, v = {}) { for (const fn of this.events[ev] || []) await fn({ preventDefault() {}, stopPropagation() {}, ...v }); }
    getBoundingClientRect() { return { width: 900, height: 500, left: 0, top: 0 }; }
    getBBox() { return { x: -300, y: -1000, width: 800, height: 1100 }; }
    setPointerCapture() {}
    querySelectorAll(sel) {
      const out = [];
      for (const k of this.kids) if (k instanceof E) { if ((k.attrs.class || "").split(" ").includes(sel.slice(1))) out.push(k); out.push(...k.querySelectorAll(sel)); }
      return out;
    }
  }
  const asks = [];
  let cards = 0;
  const map = { chat: "A", turns: 200, parent: null, start_branches: [], nodes: [
    { turn: 1, user: "q1", branches: [{ chat: "B", turns: 3 }, { chat: "C", turns: 3 }, { chat: "D", turns: 3 }] },
    { turn: 2, user: "q2", branches: [{ chat: "E", turns: 3 }] },
    { fold: [3, 150] }, { fold: [151, 180] }, { turn: 181, user: "q181" }] };
  const code = SRC.slice(SRC.indexOf("  const WL_ROW ="), SRC.indexOf("  async function openSettings()")) + "\nglobalThis.worldlineTab = worldlineTab;";
  const ctx = vm.createContext({ document: { createElementNS: (ns, t) => new E(t) }, el: (...a) => new E(...a), msgBox: () => ({ node: new E("msg"), show() {}, clear() {} }),
    currentChatId: async () => "A", chatInView: async () => ({ id: "A", name: "", open: "A", isOpen: true }), encodeURIComponent, ERR_TEXT: {}, MEM_PAGE: 30, memoryCard: () => { cards++; return new E("card"); }, errText: () => "error", console,
    bridge: async (url) => {
      asks.push(url);
      if (url.startsWith("/worldline?")) return { ok: true, json: map };
      if (url.startsWith("/worldline/turn?")) return { ok: true, json: { user: "q", reply: "a", versions: [] } };
      if (url.startsWith("/memory/list?")) {
        const off = Number(new URL("http://x" + url).searchParams.get("offset"));
        return { ok: true, json: { total: 65, memories: Array.from({ length: Math.min(30, 65 - off) }, (_, i) => ({ id: "M" + (off + i) })) } };
      }
      throw new Error("unexpected " + url);
    } });
  vm.runInContext(code, ctx);
  const body = new E("body");
  await ctx.worldlineTab(body);

  // branch boxes of neighbouring turns do not overlap (boxes are 30 high)
  const ys = body.querySelectorAll(".wl-branch").map((g) => Number(g.attrs.transform.match(/,([^)]*)/)[1])).sort((a, b) => a - b);
  for (let i = 1; i < ys.length; i++) assert.ok(ys[i] - ys[i - 1] >= 30, "branch boxes overlap: " + ys.join(" "));

  // opening a fold asks for that range only; opening another drops the first
  const folds = () => body.querySelectorAll(".wl-fold");
  await folds()[0].fire("click");
  await folds()[1].fire("click");
  const opens = asks.filter((u) => u.startsWith("/worldline?")).map((u) => new URL("http://x" + u).searchParams.get("open"));
  assert.deepStrictEqual(opens, [null, "3-150", "151-180"]);

  // a press that moved is not a click; Enter opens the turn; more memories page in
  const m = body.querySelectorAll(".ac2-map")[0];
  const node = body.querySelectorAll(".wl-node")[0];
  await m.fire("pointerdown", { pointerId: 1, clientX: 100, clientY: 100 });
  await m.fire("pointermove", { pointerId: 1, clientX: 125, clientY: 100 });
  await m.fire("pointerup", { pointerId: 1 });
  await node.fire("click");
  assert.ok(!asks.some((u) => u.startsWith("/worldline/turn?")), "a drag opened the turn");
  await node.fire("keydown", { key: "Enter" });
  await tick(0);
  assert.strictEqual(cards, 30);
  const more = (function find(e) { if (e instanceof E) { if (e.tag === "button" && e.kids.includes("더 보기")) return e; for (const k of e.kids) { const f = find(k); if (f) return f; } } return null; })(body);
  assert.ok(more && !more.hidden, "no more button");
  // a second click while a page is on its way asks for no page twice
  await Promise.all([more.fire("click"), more.fire("click")]);
  assert.strictEqual(cards, 60, "a page was shown twice");
  await more.fire("click");
  assert.strictEqual(cards, 65, "the turn's other memories are not reachable");
  assert.ok(more.hidden, "more stays after the last page");
  assert.deepStrictEqual(asks.filter((u) => u.startsWith("/memory/list?")).map((u) => new URL("http://x" + u).searchParams.get("offset")), ["0", "30", "60"]);
});

test("a reply sent while a status is on its way asks for the chip again after it (SOL-REVIEW-STEP9-08 R08-01)", async () => {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), chip: "done" };
  const h = await loadPlugin(state);
  await tick(20);
  const last = () => state.buttons[state.buttons.length - 1];
  assert.ok(last().name.includes("기억 정리 완료"));
  // a request asks for the status; its answer (done) is held
  let open;
  state.statusGate = new Promise((r) => { open = r; });
  await h.beforeRequest[0]([{ role: "user", content: "x" }], "model");
  await tick(20);
  const asks = state.statusAsks.length;
  // meanwhile the reply is observed and the backend starts making memory
  state.chip = "generating";
  await h.output[0]({ characterIndex: 0, chatIndex: 0, messageIndex: 1, chat: chat([{ role: "user", chatId: "u1", data: "hi" }, { role: "char", chatId: "a1", data: "reply" }]) });
  await tick(30);
  assert.ok(sentEvents(state).some((e) => e.hook === "output"), "output not sent");
  state.statusGate = null;
  open();
  await tick(30);
  assert.strictEqual(state.statusAsks.length, asks + 1, "not asked again after the held answer");
  assert.ok(last().name.includes("기억 만드는 중"), last().name);
  await state.unload();
});

test("the connection tab connects by itself, on opening and after typing stops; a refused token is not kept", async () => {
  class E {
    constructor(tag) { this.tag = tag; this.children = []; this.listeners = {}; this._value = ""; this.textContent = ""; this.className = ""; }
    setAttribute(k, v) { if (k === "value") this.value = v; else this[k] = String(v); }
    addEventListener(k, v) { this.listeners[k] = v; }
    append(...v) { this.children.push(...v); }
    replaceChildren(...v) { this.children = v; }
    get value() { return this._value; }
    set value(v) { this._value = String(v); }
  }
  const all = (e) => [e, ...e.children.filter((c) => c instanceof E).flatMap(all)];
  const text = (e) => (typeof e === "string" ? e : (e.textContent || "") + e.children.map(text).join(""));
  const GOOD = "ac2-good";
  let checked = true;
  const asks = [];
  const saved = [];
  let stored = { url: "http://b", token: GOOD };
  const timers = [];
  const code = SRC.slice(SRC.indexOf("  const ERR_TEXT"), SRC.indexOf("  // Status (9-3)"));
  const ctx = vm.createContext({ document: { createElement: (t) => new E(t), body: new E("body") },
    setTimeout: (fn, ms) => { const h = { fn, ms }; timers.push(h); return h; }, clearTimeout: (h) => { if (h) h.cleared = true; },
    conn: async () => ({ ...stored }), saveConn: async (n) => { saved.push(n); stored = { ...n }; return true; },
    bridge: async (u, i, c) => {
      const t = (c || stored).token;
      asks.push([u, t]);
      if (checked && t !== GOOD) return { ok: false, status: 401, code: t ? "token_invalid" : "token_required" };
      if (u === "/settings/token/check") { checked = JSON.parse(i.body).checked; return { ok: true, json: { status: "ok", checked, ...(checked ? { token: GOOD } : {}) } }; }
      return { ok: true, json: u === "/version" ? { version: "0.1.0" } : { last4: "good", created_at: "2026-10-28T00:00:00Z", checked } };
    } });
  vm.runInContext(code + "\nthis.run = connTab;", ctx);
  const body = new E("div");
  await ctx.run(body);
  await tick(10);
  const f = (id) => all(body).find((e) => e.id === id);
  const status = () => all(body).find((e) => e.role === "status");
  assert.ok(!all(body).some((e) => e.tag === "button" && text(e).includes("연결 확인")), "no separate check button");
  assert.deepStrictEqual(asks[0], ["/version", GOOD], "checked on opening");
  assert.ok(text(status()).includes("연결됨") && !status().hidden, text(status()));
  assert.strictEqual(saved.length, 0, "the same values are not written again");
  // a wrong token typed: checked after the pause, refused, not kept
  f("ac2-token").value = "ac2-wrong";
  f("ac2-token").listeners.input();
  const pause = timers.filter((h) => h.ms === 800 && !h.cleared);
  assert.strictEqual(pause.length, 1, "one check after typing stops");
  await pause[0].fn();
  assert.ok(status().className.includes("ac2-err"));
  assert.strictEqual(saved.length, 0, "a refused token is not kept");
  // typing on cancels the earlier pause; the right token connects and is kept
  f("ac2-token").value = "ac2-g";
  f("ac2-token").listeners.input();
  f("ac2-token").value = GOOD + "x";
  f("ac2-token").listeners.input();
  f("ac2-token").value = GOOD;
  stored = { url: "http://b", token: "old" };
  f("ac2-token").listeners.input();
  const live = timers.filter((h) => h.ms === 800 && !h.cleared);
  assert.strictEqual(live.length, 1, "only the last pause runs");
  await live[0].fn();
  assert.strictEqual(JSON.stringify(saved), JSON.stringify([{ url: "http://b", token: GOOD }]));
  assert.ok(text(status()).includes("연결됨"), text(status()));

  // the token check (on by default) turned off: an empty token connects;
  // turned on again, this device keeps the token handed back
  await tick(10);
  const sw = f("ac2-tokcheck");
  assert.strictEqual(sw.checked, true, "on as the backend says");
  sw.checked = false;
  await sw.listeners.change();
  assert.strictEqual(checked, false);
  f("ac2-token").value = "";
  stored = { url: "http://b", token: "" };
  f("ac2-token").listeners.input();
  await timers.filter((h) => h.ms === 800 && !h.cleared).pop().fn();
  assert.ok(text(status()).includes("연결됨"), "no token, check off: " + text(status()));
  saved.length = 0;
  sw.checked = true;
  await sw.listeners.change();
  assert.strictEqual(checked, true);
  assert.strictEqual(JSON.stringify(saved), JSON.stringify([{ url: "http://b", token: GOOD }]), "the token handed back is kept");
  assert.strictEqual(f("ac2-token").value, GOOD);
});

// Run the real connection tab, bridge, conn and saveConn. Only DOM and
// RisuAI storage/nativeFetch are external boundaries here.
async function firstConnectionUI({ closed, checked = true, savedToken = "", shared = {}, local = {}, databaseMessage, connection } = {}) {
  class E {
    constructor(tag) { this.tag = tag; this.children = []; this.listeners = {}; this._value = ""; this.textContent = ""; this.className = ""; }
    setAttribute(k, v) { if (k === "value") this.value = v; else this[k] = String(v); }
    addEventListener(k, v) { this.listeners[k] = v; }
    append(...v) { this.children.push(...v); }
    replaceChildren(...v) { this.children = v; }
    get value() { return this._value; }
    set value(v) { this._value = String(v); }
  }
  const all = (e) => [e, ...e.children.filter((c) => c instanceof E).flatMap(all)];
  const text = (e) => typeof e === "string" ? e : (e.textContent || "") + e.children.map(text).join("");
  const url = "http://fresh-backend";
  const value = "synthetic-" + Math.random().toString(36).slice(2);
  const asks = [], writes = [];
  const reply = (status, json) => ({ ok: status < 400, status, text: async () => JSON.stringify(json) });
  const api = {
    getArgument: async (k) => k === "ac2_url" ? url : savedToken,
    safeLocalStorage: {
      getItem: async (k) => local[k] || null,
      setItem: async (k, v) => { writes.push(["local", k]); local[k] = v; },
    },
    nativeFetch: async (u, init) => {
      const path = u.slice(url.length), sent = init.headers["X-AC2-Token"];
      asks.push({ path, token: sent });
      if (path === "/settings/token/claim") {
        assert.strictEqual(init.method, "POST");
        assert.strictEqual(sent, "", "assignment asks without a token");
        return closed ? reply(409, { code: "token_claim_closed", closed: true, ...closed }) : reply(200, { token: value, checked: true });
      }
      assert.ok(["/version", "/ready", "/settings/token"].includes(path), "unexpected backend call " + path);
      if (checked && sent !== (savedToken || value)) return reply(401, { code: sent ? "token_invalid" : "token_required" });
      if (path === "/ready") return databaseMessage ? reply(503, { code: "database_not_ready", message: databaseMessage }) : reply(200, { status: "ready" });
      if (path === "/settings/token" && sent === value) {
        assert.strictEqual(shared.ac2_token_v1, value, "the shared token must be saved before the next request");
        assert.strictEqual(JSON.parse(local.ac2_conn_v1).token, value, "the device token must be saved before the next request");
      }
      return reply(200, path === "/version" ? { version: "test", connection } : { last4: "test", created_at: new Date().toISOString(), checked });
    },
  };
  const store = { getItem: async (k) => shared[k] ?? null, setItem: async (k, v) => { writes.push(["shared", k]); shared[k] = v; } };
  const code = SRC.slice(SRC.indexOf("  const CONN_KEY"), SRC.indexOf("  // Short content fingerprint"))
    + SRC.slice(SRC.indexOf("  async function bridge("), SRC.indexOf("  // Sending never blocks"))
    + SRC.slice(SRC.indexOf("  const ERR_TEXT"), SRC.indexOf("  // Status (9-3)"));
  const ctx = vm.createContext({ api, store, arg: async (k, fallback) => (await api.getArgument(k)) || fallback,
    FETCH_TIMEOUT_MS: 8000, AbortController, setTimeout, clearTimeout,
    document: { createElement: (t) => new E(t), body: new E("body") } });
  vm.runInContext(code + "\nthis.run = connTab; this.readConn = conn; this.call = bridge;", ctx);
  const body = new E("div");
  await ctx.run(body);
  await tick(20);
  return { asks, writes, shared, local, value, ctx, body, elements: all(body), text: text(body) };
}

test("first connection automatically claims and saves the token in both stores, then uses it on later requests", async () => {
  const ui = await firstConnectionUI();
  assert.deepStrictEqual(ui.asks.map((a) => a.path), ["/version", "/settings/token/claim", "/version", "/ready", "/settings/token"]);
  assert.strictEqual(ui.shared.ac2_token_v1, ui.value);
  assert.strictEqual(JSON.parse(ui.local.ac2_conn_v1).token, ui.value);
  assert.strictEqual(ui.elements.find((e) => e.id === "ac2-token").value, ui.value);
  assert.ok(ui.text.includes("연결됨"), ui.text);
  const claims = ui.asks.filter((a) => a.path.endsWith("/claim"));
  assert.strictEqual(claims.length, 1);
  assert.ok(ui.asks.slice(2).every((a) => a.token === ui.value));
  await ui.ctx.call("/version", { method: "GET" });
  assert.strictEqual(ui.asks.at(-1).token, ui.value);
  // A second device starts with the very same server-side plugin store.
  const state = { storeData: ui.shared, local: {}, calls: [], backendUp: true, rereadChat: chat([]) };
  await loadPlugin(state);
  assert.ok(state.calls.length > 0 && state.calls.every((c) => c.token === ui.value), "another device did not use the shared assignment");
  await state.unload();
});

test("closed first assignment shows its time, address and all recovery instructions without saving a token", async () => {
  const at = new Date().toISOString(), address = "synthetic-previous-device";
  const ui = await firstConnectionUI({ closed: { claimed_at: at, address } });
  assert.ok(ui.text.includes("첫 토큰이 이미 다른 연결에 할당됐어요."), ui.text);
  assert.ok(ui.text.includes(new Date(at).toLocaleString()) && ui.text.includes(address), ui.text);
  assert.ok(!ui.text.includes("토큰 보기"), "recovery instructions must not refer to a missing button");
  for (const phrase of ["백엔드 PC의 데이터 폴더 token.txt", "token.txt를 지우고 백엔드를 다시 켜면", "첫 할당이 다시 열려요"]) assert.ok(ui.text.includes(phrase), phrase);
  assert.strictEqual(ui.writes.length, 0, "closed assignment changed stored credentials");
  assert.strictEqual((await ui.ctx.readConn()).token, "");
  assert.ok(!ui.text.includes("연결됨"));
  assert.ok(ui.elements.some((e) => e.id === "ac2-token"), "manual paste field disappeared");
  const legacy = await firstConnectionUI({ closed: {} });
  assert.ok(legacy.text.includes("할당 시각: 기록 없음") && legacy.text.includes("요청 주소: 기록 없음"), legacy.text);
});

test("the connection tab never claims when token checking is off or an existing token connects", async () => {
  for (const args of [{ checked: false }, { savedToken: "synthetic-saved" }]) {
    const ui = await firstConnectionUI(args);
    assert.ok(ui.text.includes("연결됨"), ui.text);
    assert.ok(!ui.asks.some((a) => a.path.endsWith("/claim")), "unnecessary first-token request");
    assert.strictEqual(ui.writes.length, 0, "already working connection was rewritten");
  }
});

test("connection shows Go's database problem and remedy instead of a false connected status", async () => {
  const message = "백엔드가 데이터베이스에 연결되지 않았습니다. 패키지의 시작 파일로 다시 실행해 주세요.";
  const ui = await firstConnectionUI({ databaseMessage: message });
  assert.ok(ui.text.includes(message), ui.text);
  assert.ok(!ui.text.includes("연결됨"), ui.text);
  assert.strictEqual(ui.shared.ac2_token_v1, ui.value, "a first token must stay saved even when the DB is unavailable");
  assert.ok(ui.asks.some(a => a.path === "/ready"), "database readiness was not requested");
});

test("connection displays the backend's address hint verbatim after readiness", async () => {
  for (const remote_hint of ["다른 기기에서 쓸 주소: synthetic://interface-address (Tailscale)", "이 PC에서만 받도록 설정되어 있어요."]) {
    const connection = { remote_hint, remote_urls: ["unused://not-rendered-by-js"] };
    const ui = await firstConnectionUI({ checked: false, connection });
    assert.ok(ui.text.includes(remote_hint), ui.text);
    assert.ok(!ui.text.includes(connection.remote_urls[0]), "JS assembled its own address advice");
    const unavailable = await firstConnectionUI({ checked: false, connection, databaseMessage: "synthetic database unavailable" });
    assert.ok(!unavailable.text.includes(remote_hint), "address advice was presented as a connected backend before readiness");
  }
});

test("cold start runs for the open chat after naming it and the cost, shows the run's progress and cancels it", async () => {
  class E {
    constructor(tag) { this.tag = tag; this.children = []; this.listeners = {}; this._value = ""; this.textContent = ""; this.className = ""; this.isConnected = true; }
    setAttribute(k, v) { if (k === "value") this.value = v; else this[k] = String(v); }
    addEventListener(k, v) { this.listeners[k] = v; }
    append(...v) { this.children.push(...v.filter((x) => x != null)); }
    replaceChildren(...v) { this.children = v.filter((x) => x != null); }
    get value() { return this._value; }
    set value(v) { this._value = String(v); }
  }
  const all = (e) => [e, ...e.children.filter((c) => c instanceof E).flatMap(all)];
  const text = (e) => (typeof e === "string" ? e : (e.textContent || "") + e.children.map(text).join(""));
  const posts = [];
  const gets = [];
  let reply = { turns: [3, 4] };
  let progress = { total: 0 };
  let chips = 0;
  const timers = [];
  const code = SRC.slice(SRC.indexOf("  const ERR_TEXT"), SRC.indexOf("  // The chat chip"));
  const ctx = vm.createContext({ document: { createElement: (t) => new E(t), body: new E("body") }, refreshChip() { chips++; },
    setTimeout: (fn, ms) => { const h = { fn, ms }; timers.push(h); return h; }, clearTimeout: (h) => { if (h) h.cleared = true; },
    bridge: async (u, i) => {
      if (i.method === "GET") { gets.push(u); return { ok: true, json: progress }; }
      posts.push([u, JSON.parse(i.body)]);
      return { ok: true, json: u.endsWith("/cancel") ? { cancelled: 1 } : reply };
    } });
  // the host snapshot is the test's: each call is recorded with whether
  // the texts were to go again
  const snaps = [];
  ctx.__snap = async (chat, again) => { snaps.push([chat, again]); return { contract: "observation.v1", events: [{ hook: "load", n: snaps.length }] }; };
  vm.runInContext(code + "\ncoldSnapshot = __snap; this.run = coldStart;", ctx);
  const box = ctx.run("chat-1", "수요일의 모험");
  await tick(5);
  assert.deepStrictEqual(gets, ["/memory/backfill?chat_id=chat-1"], "the run of this chat is asked for");
  const btn = (label) => all(box).find((e) => e.tag === "button" && text(e) === label);
  const msg = () => all(box).find((e) => e.role === "status");
  // the first press names the chat and the cost; cancel sends nothing
  btn("콜드 스타트 실행").listeners.click();
  assert.ok(text(box).includes("대상 채팅: 수요일의 모험") && text(box).includes("비용"), text(box));
  btn("취소").listeners.click();
  assert.strictEqual(posts.length, 0);
  // blank: every earlier turn, oldest first; then its progress
  progress = { total: 2, done: 1, failed: 0, waiting: 1, cancelled: 0, failures: [] };
  btn("콜드 스타트 실행").listeners.click();
  await btn("실행").listeners.click();
  // the chat's fresh snapshot goes along, read first by the backend
  assert.strictEqual(JSON.stringify(posts.pop()), JSON.stringify(["/memory/backfill", { chat_id: "chat-1", max_turns: 0, order: "oldest", observation: { contract: "observation.v1", events: [{ hook: "load", n: 1 }] } }]));
  assert.deepStrictEqual(snaps, [["chat-1", false]]);
  assert.ok(chips === 1 && text(box).includes("1 / 2턴") && text(box).includes("진행 중"), text(box));
  const poll = timers.filter((h) => h.ms === 5000 && !h.cleared);
  assert.strictEqual(poll.length, 1, "asked again while turns are left");
  // cancel drops the rest
  progress = { total: 2, done: 1, failed: 0, waiting: 0, cancelled: 1, failures: [] };
  await btn("작업 취소").listeners.click();
  assert.strictEqual(JSON.stringify(posts.pop()), JSON.stringify(["/memory/backfill/cancel", { chat_id: "chat-1" }]));
  assert.ok(text(box).includes("취소됨") && !btn("작업 취소"), text(box));
  assert.strictEqual(timers.filter((h) => h.ms === 5000 && !h.cleared).length, 0, "no asking once nothing is left");
  // failures are named with their turn
  progress = { total: 2, done: 1, failed: 1, waiting: 0, cancelled: 0, failures: [{ turn: 4, error: "model said no" }] };
  await btn("새로고침").listeners.click();
  assert.ok(text(box).includes("4턴 실패: model said no") && text(box).includes("오류를 포함해 끝남"), text(box));
  // waiting after a failed call (bug report 11-12): told apart from running, with the next call's time; still asked again
  progress = { total: 2, done: 0, failed: 0, waiting: 2, cancelled: 0, failures: [], running: false, retry_waiting: true, retry_at: "2026-11-12T10:00:00Z" };
  await btn("새로고침").listeners.click();
  assert.ok(text(box).includes("다시 시도 대기 · 다음 시도 ") && !text(box).includes("진행 중") && btn("작업 취소"), text(box));
  assert.strictEqual(timers.filter((h) => h.ms === 5000 && !h.cleared).length, 1, "asked again while waiting");
  // a call under way again is running, even with a wait left from before
  progress = { ...progress, running: true };
  await btn("새로고침").listeners.click();
  assert.ok(text(box).includes("진행 중") && !text(box).includes("다시 시도 대기"), text(box));
  // a number: the most recent turns
  all(box).find((e) => e.id === "ac2-coldn").value = "5";
  // the backend lacked texts the snapshot sent as fingerprints: once more with them
  let first = true;
  reply = { get turns() { return []; }, get resend_text() { const r = first; first = false; return r; } };
  btn("콜드 스타트 실행").listeners.click();
  assert.ok(text(box).includes("최근 5턴"));
  await btn("실행").listeners.click();
  assert.deepStrictEqual(snaps.slice(1), [["chat-1", false], ["chat-1", true]], "texts sent again when asked");
  assert.strictEqual(JSON.stringify(posts.pop()), JSON.stringify(["/memory/backfill", { chat_id: "chat-1", max_turns: 5, order: "recent", observation: { contract: "observation.v1", events: [{ hook: "load", n: 3 }] } }]));
  assert.ok(text(msg()).includes("만들 지난 턴이 없어요"), text(msg()));
});

// U1: run the real chatsTab with only the host, transport and U2 boundaries replaced.
async function u5Screen() {
  class E {
    constructor(tag) { this.tag = tag; this.children = []; this.listeners = {}; this._value = ""; this.textContent = ""; }
    setAttribute(k, v) { if (k === "value") this.value = v; else this[k] = String(v); }
    addEventListener(k, v) { this.listeners[k] = v; }
    append(...v) { this.children.push(...v); }
    replaceChildren(...v) { this.children = v; }
    get value() { return this.tag === "select" ? ((this.children.find((c) => c.selected) || this.children[0] || {}).value || "") : this._value; }
    set value(v) { if (this.tag === "select") for (const c of this.children) c.selected = c.value === v; else this._value = String(v); }
    querySelector() { return all(this).find((e) => e.role === "status"); }
  }
  const all = (e) => [e, ...e.children.filter((c) => c instanceof E).flatMap(all)];
  const text = (e) => typeof e === "string" ? e : e.textContent + e.children.map(text).join("");
  const view = { critic: { provider: "openai", model: "test", retries: 3 }, embedding: { provider: "voyageai", model: "fake" },
    providers: ["openai", "claude", "gemini", "custom"].map((id) => ({ id, label: id })), embed_providers: [], service_tier_providers: ["openai", "custom"] };
  let prompt = "Default instructions including JSON shape", promptStored = false;
  let pubPrompt = "Default guide rules", pubStored = false;
  const budget = { memory_chars: 2000, secret_chars: 4000, placement: "auto", anchor: "", placements: ["auto", "before_latest_user", "after_anchor_marker", "after_last_cache_point", "after_first_system", "end"] };
  const providers = view.providers.map((p) => ({ ...p, reasoning: ["low", "high"], needs_key: true }));
  const prep = {
    "/settings/preprocess": { mode: "", modes: [{ id: "ai_jev", label: "AI + 판정 모델" }, { id: "ai", label: "AI" }, { id: "jev", label: "판정 모델", recommended: true }, { id: "", label: "사용 안 함" }], ai_own: false, ai: { provider: "", key_set: false }, providers, service_tier_providers: ["openai", "custom"], deadline_s: 90 },
    "/settings/decision": { provider: "", url: "", model: "", key_set: false, providers: [{ id: "ollama", label: "Ollama", default_url: "http://127.0.0.1:11434", default_model: "clef:cloud", key_optional: true }, { id: "typesafe", label: "TypeSafe Jev", default_url: "https://api.typesafe.ai", default_model: "jev-1.13.0" }] },
    "/settings/narrative": { level: "", levels: [{ id: "", label: "끔" }, { id: "weak", label: "약하게" }, { id: "mid", label: "중간" }, { id: "strong", label: "강하게" }], chars: 3000, input: "", inputs: [{ id: "", label: "끔" }, { id: "clarify", label: "뜻만 분명하게" }, { id: "blend", label: "기억 녹이기" }], ai_own: true, ai: { provider: "openai", model: "pub", key_set: true, key_last4: "9z9z" }, providers, service_tier_providers: ["openai", "custom"], deadline_s: 95 },
  };
  const posts = []; let fail = "", failGet = "", failPost = "", holdUrl = "", gate = null;
  const ctx = vm.createContext({ document: { createElement: (tag) => new E(tag) }, refreshChip() {}, bridge: async (url, init) => {
    const b = init.body ? JSON.parse(init.body) : null;
    if (b) { posts.push({ url, body: b }); if (fail) return { ok: false, code: fail }; if (url === failPost) return { ok: false, code: "store_failed" }; }
    if (url === "/settings/prompt") { if (b) { prompt = b.reset ? "Default instructions including JSON shape" : b.content; promptStored = !b.reset; } return { ok: true, json: { content: prompt, custom: promptStored } }; }
    if (url === "/settings/publisher-prompt") { if (b) { pubPrompt = b.reset ? "Default guide rules" : b.content; pubStored = !b.reset; } return { ok: true, json: { content: pubPrompt, custom: pubStored } }; }
    if (url === "/settings/memory") { if (b) Object.assign(budget, b); return { ok: true, json: JSON.parse(JSON.stringify(budget)) }; }
    if (url === "/settings/decision/test") return { ok: true, json: { ok: true, ms: 580 } };
    if (!b && url === failGet) return { ok: false, code: "store_failed" };
    if (!b && url === holdUrl) { holdUrl = ""; await gate; } // the first read of it waits
    if (prep[url]) { if (b) Object.assign(prep[url], b); return { ok: true, json: JSON.parse(JSON.stringify(prep[url])) }; }
    if (b) Object.assign(view.critic, b.critic);
    return { ok: true, json: JSON.parse(JSON.stringify(view)) };
  } });
  vm.runInContext(SRC.slice(SRC.indexOf("  const ERR_TEXT"), SRC.indexOf("  // Status (9-3)")) + "\nthis.models = modelsTab; this.budget = budgetTab; this.prep = prepTab;", ctx);
  const body = new E("body");
  return { body, posts, view, all, text, f: (id) => all(body).find((e) => e.id === id),
    click: (label) => all(body).find((e) => e.tag === "button" && text(e) === label).listeners.click(),
    models: () => ctx.models(body), budget: () => ctx.budget(body), prep: () => ctx.prep(body), prepView: prep, failGet: (u) => { failGet = u; }, hold: (u) => { holdUrl = u; let rel; gate = new Promise((r) => { rel = r; }); return rel; }, failPost: (u) => { failPost = u; }, fail: (code) => { fail = code; } };
}

test("settings screens warn when the backend says a saved choice lacks its connection (1.1)", async () => {
  const shown = (ui, part) => ui.all(ui.body).find((e) => ui.text(e).startsWith("지금 저장된 설정으로는 " + part) && !(e.children || []).some((c) => c && c.tag));
  const ui = await u5Screen();
  ui.prepView["/settings/preprocess"].mode = "ai_jev";
  ui.prepView["/settings/preprocess"].problem = "decision_off";
  ui.prepView["/settings/preprocess"].problems = ["decision_off", "ai_off"];
  await ui.prep();
  const w = shown(ui, "전처리");
  // every part the backend names, told as the critic's problem is (an error, not the outgoing-text notice)
  assert.ok(w && ui.text(w) === "지금 저장된 설정으로는 전처리를 쓸 수 없어요: 판정 모델 없음 · AI 연결 없음" && /ac2-err/.test(w.className || (w.attrs && w.attrs.class) || ""), w && ui.text(w));
  // another value on the screen than the saved one: the warning is not about it
  const mode = ui.f("ac2-prep-mode");
  mode.value = ""; mode.listeners.change();
  assert.ok(w.hidden, "hidden for an unsaved mode");
  mode.value = "ai_jev"; mode.listeners.change();
  assert.ok(!w.hidden, "shown again with the saved mode");
  const ok = await u5Screen(); await ok.prep();
  assert.ok(!shown(ok, "전처리"), "no warning without a problem");
  const pub = await u5Screen();
  pub.prepView["/settings/narrative"].problem = "ai_off";
  await pub.models();
  const pw = shown(pub, "출판사");
  assert.ok(pw && pub.text(pw) === "지금 저장된 설정으로는 출판사를 쓸 수 없어요: AI 연결 없음", pw && pub.text(pw));
  const level = pub.f("ac2-guide-level");
  level.value = "mid"; level.listeners.change();
  assert.ok(pw.hidden, "hidden for an unsaved level");
  level.value = ""; level.listeners.change();
  assert.ok(!pw.hidden, "shown again with the saved level");
  // kept for an unsaved prompt edit while the critic is saved: its warning was the earlier critic's
  pub.f("ac2-pub-prompt").value = "edited, not saved";
  pub.click("평론가·임베딩 저장");
  await tick(20);
  const kept = shown(pub, "출판사");
  assert.ok(kept === pw && pw.hidden, "the kept box keeps no warning about the earlier critic");
  level.value = "mid"; level.listeners.change(); level.value = ""; level.listeners.change();
  assert.ok(pw.hidden, "back to the saved level, the earlier critic's warning stays gone");
});

test("preprocessing tab (step 5): the backend's choices are shown, JEV recommended; one save sends the decision model and the mode; keys only when typed; the publisher is on the models tab", async () => {
  const ui = await u5Screen(); await ui.prep();
  const opts = (id) => ui.f(id).children.map((o) => o.value + "=" + ui.text(o));
  assert.deepStrictEqual(opts("ac2-prep-mode"), ["ai_jev=AI + 판정 모델", "ai=AI", "jev=판정 모델 (권장)", "=사용 안 함"]);
  assert.ok(ui.text(ui.body).includes("후보 기억 원문과 최근 대화가 판정 모델과 AI로 나가요"), "the outgoing-text notice");
  assert.ok(!ui.f("ac2-guide-level") && ui.text(ui.body).includes("'모델' 탭에 있어요"), "the publisher is not on this tab");
  // only the connections the chosen mode uses are shown
  const shown = () => [ui.f("ac2-prep-ai-box").hidden ? "" : "ai", ui.f("ac2-prep-jev-box").hidden ? "" : "jev"].filter(Boolean).join("+");
  const seen = {};
  for (const m of ["", "ai", "jev", "ai_jev"]) { ui.f("ac2-prep-mode").value = m; ui.f("ac2-prep-mode").listeners.change(); seen[m] = shown(); }
  assert.deepStrictEqual(seen, { "": "", ai: "ai", jev: "jev", ai_jev: "ai+jev" });
  ui.f("ac2-prep-mode").value = ""; ui.f("ac2-prep-mode").listeners.change();
  const inside = (e, x) => e === x || (e.children || []).some((c) => inside(c, x));
  const hiddenAt = (id) => ui.all(ui.body).some((e) => e.hidden && inside(e, ui.f(id)));
  ui.f("ac2-prep-mode").value = "ai_jev"; ui.f("ac2-prep-mode").listeners.change(); // both boxes shown
  assert.strictEqual(hiddenAt("ac2-dec-url"), true, "no decision fields while off");
  assert.strictEqual(hiddenAt("ac2-prep-ai-model"), true, "the critic's connection by default: own fields hidden");
  ui.f("ac2-dec-prov").value = "typesafe"; ui.f("ac2-dec-prov").listeners.change();
  assert.strictEqual(hiddenAt("ac2-dec-url"), false);
  assert.deepStrictEqual([ui.f("ac2-dec-url").value, ui.f("ac2-dec-model").value], ["https://api.typesafe.ai", "jev-1.13.0"], "the service's defaults");
  ui.f("ac2-prep-mode").value = "jev";
  await ui.click("저장");
  const sent = (u) => ui.posts.filter((p) => p.url === u).at(-1).body;
  assert.deepStrictEqual(ui.posts.map((p) => p.url), ["/settings/decision", "/settings/preprocess"]);
  assert.deepStrictEqual(sent("/settings/decision"), { provider: "typesafe", url: "https://api.typesafe.ai", model: "jev-1.13.0" }, "no key typed, none sent");
  assert.strictEqual(sent("/settings/preprocess").mode, "jev"); assert.strictEqual(sent("/settings/preprocess").ai_own, false);
  assert.ok(ui.text(ui.body).includes("저장했어요"));
  ui.f("ac2-dec-key").value = "jev-key"; await ui.click("저장");
  assert.strictEqual(sent("/settings/decision").key, "jev-key");
  // a failed save names its part
  ui.fail("unknown_choice"); await ui.click("저장");
  assert.ok(ui.text(ui.body).includes("판정 모델 설정을 저장하지 못했어요"), ui.text(ui.body));
});

test("preprocessing tab (review 20): a service chosen back brings its own address, a failed reload or partial save is told", async () => {
  const ui = await u5Screen();
  Object.assign(ui.prepView["/settings/decision"], { provider: "typesafe", url: "https://jev.example", model: "jev-own", key_set: true, key_last4: "abcd" });
  await ui.prep();
  ui.f("ac2-dec-prov").value = "ollama"; ui.f("ac2-dec-prov").listeners.change();
  ui.f("ac2-dec-prov").value = "typesafe"; ui.f("ac2-dec-prov").listeners.change();
  assert.deepStrictEqual([ui.f("ac2-dec-url").value, ui.f("ac2-dec-model").value], ["https://jev.example", "jev-own"], "the saved service's own address and model");
  await ui.click("저장");
  const d = ui.posts.filter((p) => p.url === "/settings/decision").at(-1).body;
  assert.deepStrictEqual([d.url, d.model, "key" in d], ["https://jev.example", "jev-own", false]);
  // the saved settings cannot be read again: the error stays, no success over it
  ui.failGet("/settings/preprocess");
  await ui.click("저장");
  assert.ok(!ui.text(ui.body).includes("저장했어요") && ui.text(ui.body).length > 0, ui.text(ui.body));
  ui.failGet("");
  // a later part fails: the part already saved is named
  ui.body.replaceChildren(); await ui.prep();
  ui.failPost("/settings/preprocess");
  await ui.click("저장");
  assert.ok(ui.text(ui.body).includes("판정 모델 설정은 저장했고, 기억 고르기 설정을 저장하지 못했어요"), ui.text(ui.body));
  ui.failPost("");
});

test("models tab: the publisher box (AC1's publisher and input improvement) shows the backend's levels and inputs, keeps options it does not show and the reasoning chosen, saves on its own and tells a failed reload", async () => {
  const ui = await u5Screen();
  Object.assign(ui.prepView["/settings/narrative"].ai, { timeout_ms: 12345, temperature: 0, max_tokens: 4567, extra_headers_json: '{"X":"1"}', extra_body_json: '{"top_p":1}', service_tier: "flex", claude_prompt_cache: "ephemeral_5m", reasoning: "low" });
  await ui.models();
  const opts = (id) => ui.f(id).children.map((o) => o.value);
  assert.deepStrictEqual(opts("ac2-guide-level"), ["", "weak", "mid", "strong"]);
  assert.deepStrictEqual(opts("ac2-input-mode"), ["", "clarify", "blend"]);
  assert.ok(ui.text(ui.body).includes("비밀 기억 줄은 출판사 AI에 보내지 않아요") && ui.text(ui.body).includes("최근 대화는 그대로 보내므로"));
  assert.ok(ui.text(ui.body).includes("저장된 키 끝 4자리 9z9z"));
  const inside = (e, x) => e === x || (e.children || []).some((c) => inside(c, x));
  const hiddenAt = (id) => ui.all(ui.body).some((e) => e.hidden && inside(e, ui.f(id)));
  assert.strictEqual(hiddenAt("ac2-pub-ai-model"), false, "a saved own publisher connection shows its fields");
  ui.f("ac2-pub-ai-reason").value = "high"; ui.f("ac2-pub-ai-reason").listeners.change();
  ui.f("ac2-pub-ai-own").checked = false; ui.f("ac2-pub-ai-own").listeners.change();
  ui.f("ac2-pub-ai-own").checked = true; ui.f("ac2-pub-ai-own").listeners.change();
  assert.strictEqual(ui.f("ac2-pub-ai-reason").value, "high");
  ui.f("ac2-guide-level").value = "strong"; ui.f("ac2-input-mode").value = "clarify"; ui.f("ac2-guide-chars").value = "1500";
  await ui.click("출판사 설정 저장");
  assert.deepStrictEqual(ui.posts.map((p) => p.url), ["/settings/narrative"], "the publisher saves on its own");
  const n = ui.posts.at(-1).body;
  assert.deepStrictEqual([n.level, n.chars, n.input, n.ai_own, n.ai.provider, n.ai.model, "key" in n.ai], ["strong", 1500, "clarify", true, "openai", "pub", false]);
  assert.deepStrictEqual([n.ai.timeout_ms, n.ai.temperature, n.ai.max_tokens, n.ai.extra_headers_json, n.ai.extra_body_json, n.ai.service_tier, n.ai.claude_prompt_cache, n.ai.reasoning], [12345, 0, 4567, '{"X":"1"}', '{"top_p":1}', "flex", "ephemeral_5m", "high"]);
  assert.ok(ui.text(ui.body).includes("출판사 설정을 저장했어요"));
  ui.f("ac2-pub-ai-key").value = "sk-new"; await ui.click("출판사 설정 저장");
  assert.strictEqual(ui.posts.at(-1).body.ai.key, "sk-new");
  const before = ui.posts.length;
  ui.f("ac2-guide-chars").value = "-3"; await ui.click("출판사 설정 저장");
  assert.strictEqual(ui.posts.length, before); assert.ok(ui.text(ui.body).includes("0 이상의 정수"));
  ui.f("ac2-guide-chars").value = "3000";
  ui.failGet("/settings/narrative");
  await ui.click("출판사 설정 저장");
  assert.ok(!ui.text(ui.body).includes("출판사 설정을 저장했어요"), ui.text(ui.body));
  ui.failGet("");
});

test("models tab: saving the critic/embedding keeps what was typed (not saved) in the publisher box", async () => {
  const ui = await u5Screen();
  await ui.models();
  ui.f("ac2-guide-chars").value = "1500"; ui.f("ac2-input-mode").value = "blend";
  await ui.click("평론가·임베딩 저장");
  assert.ok(ui.text(ui.body).includes("저장했어요. 다음 기억 생성"), "the critic save is told");
  assert.deepStrictEqual(ui.posts.map((p) => p.url), ["/settings/models"], "the publisher was not saved");
  assert.deepStrictEqual([ui.f("ac2-guide-chars").value, ui.f("ac2-input-mode").value], ["1500", "blend"], "the publisher box was redrawn from the server");
  await ui.click("출판사 설정 저장"); // the kept box still saves
  assert.strictEqual(ui.posts.at(-1).body.chars, 1500);
});

test("models tab: a publisher box that was not edited is read again after a critic save (failed first read; value changed elsewhere)", async () => {
  const ui = await u5Screen();
  ui.failGet("/settings/narrative");
  await ui.models();
  assert.ok(!ui.f("ac2-guide-chars"), "the failed read left an error box");
  ui.failGet("");
  await ui.click("평론가·임베딩 저장");
  assert.ok(ui.f("ac2-guide-chars"), "the box stayed an error box after the save");
  // untouched box, the server value changed elsewhere
  ui.prepView["/settings/narrative"].chars = 4500;
  await ui.click("평론가·임베딩 저장");
  assert.strictEqual(ui.f("ac2-guide-chars").value, "4500");
  await ui.click("출판사 설정 저장");
  assert.strictEqual(ui.posts.at(-1).body.chars, 4500);
});

test("models tab: the critic save works while the publisher box is still loading", async () => {
  const ui = await u5Screen();
  const release = ui.hold("/settings/narrative");
  const loading = ui.models();
  await tick(10);
  await ui.click("평론가·임베딩 저장");
  assert.deepStrictEqual(ui.posts.map((p) => p.url), ["/settings/models"]);
  assert.ok(ui.text(ui.body).includes("저장했어요. 다음 기억 생성"), "no saved message");
  release(); await loading;
});

test("own connections (user 11-08): the publisher and the preprocessing AI take the critic's advanced options — wait time, temperature, max tokens, headers, body, tier for the providers that take it, Claude cache — and tell the backend's call bound", async () => {
  const ui = await u5Screen();
  await ui.models();
  const inside = (e, x) => e === x || (e.children || []).some((c) => inside(c, x));
  const hiddenAt = (id) => ui.all(ui.body).some((e) => e.hidden && inside(e, ui.f(id)));
  assert.ok(ui.text(ui.body).includes("한 요청은 길어도 95초에서 끊겨요."), "the publisher's bound from the backend");
  assert.strictEqual(hiddenAt("ac2-pub-ai-tier"), false, "openai takes a tier");
  assert.strictEqual(hiddenAt("ac2-pub-ai-cache"), true, "the cache is Claude's");
  ui.f("ac2-pub-ai-prov").value = "claude"; ui.f("ac2-pub-ai-prov").listeners.change();
  assert.deepStrictEqual([hiddenAt("ac2-pub-ai-tier"), hiddenAt("ac2-pub-ai-cache")], [true, false]);
  ui.f("ac2-pub-ai-timeout").value = "30"; ui.f("ac2-pub-ai-temperature").value = "0.7"; ui.f("ac2-pub-ai-max-tokens").value = "2048";
  ui.f("ac2-pub-ai-headers").value = '{"H":"2"}'; ui.f("ac2-pub-ai-body").value = '{"top_k":5}'; ui.f("ac2-pub-ai-cache").value = "ephemeral_1h";
  await ui.click("출판사 설정 저장");
  const a = ui.posts.filter((p) => p.url === "/settings/narrative").at(-1).body.ai;
  assert.deepStrictEqual([a.provider, a.timeout_ms, a.temperature, a.max_tokens, a.extra_headers_json, a.extra_body_json, a.claude_prompt_cache], ["claude", 30000, 0.7, 2048, '{"H":"2"}', '{"top_k":5}', "ephemeral_1h"]);
  // emptied fields go back as the default (null), not as 0
  ui.f("ac2-pub-ai-temperature").value = ""; ui.f("ac2-pub-ai-max-tokens").value = ""; ui.f("ac2-pub-ai-timeout").value = "";
  await ui.click("출판사 설정 저장");
  const b = ui.posts.filter((p) => p.url === "/settings/narrative").at(-1).body.ai;
  assert.deepStrictEqual([b.timeout_ms, b.temperature, b.max_tokens], [null, null, null]);
  // the preprocessing AI has the same options and its own bound
  ui.body.replaceChildren(); await ui.prep();
  ui.f("ac2-prep-mode").value = "ai"; ui.f("ac2-prep-mode").listeners.change && ui.f("ac2-prep-mode").listeners.change();
  ui.f("ac2-prep-ai-own").checked = true; ui.f("ac2-prep-ai-own").listeners.change();
  assert.ok(ui.text(ui.body).includes("한 요청은 길어도 90초에서 끊겨요."));
  ui.f("ac2-prep-ai-prov").value = "openai"; ui.f("ac2-prep-ai-prov").listeners.change();
  ui.f("ac2-prep-ai-temperature").value = "0.2"; ui.f("ac2-prep-ai-tier").value = "flex";
  await ui.click("저장");
  const c = ui.posts.filter((p) => p.url === "/settings/preprocess").at(-1).body.ai;
  assert.deepStrictEqual([c.provider, c.temperature, c.service_tier, c.max_tokens], ["openai", 0.2, "flex", null]);
});

test("U5 model controls save optional values, show provider fields, and render JSON errors", async () => {
  const ui = await u5Screen(); await ui.models();
  const parent = (id) => ui.all(ui.body).find((e) => e.children.includes(ui.f(id)));
  assert.strictEqual(!!parent("ac2-tier").hidden, false);
  assert.strictEqual(parent("ac2-claude-cache").hidden, true);
  ui.f("ac2-cprov").value = "claude"; ui.f("ac2-cprov").listeners.change();
  assert.strictEqual(parent("ac2-tier").hidden, true);
  assert.strictEqual(parent("ac2-claude-cache").hidden, false);
  ui.f("ac2-cprov").value = "gemini"; ui.f("ac2-cprov").listeners.change();
  assert.strictEqual(parent("ac2-tier").hidden, true);
  assert.strictEqual(parent("ac2-claude-cache").hidden, true);
  ui.f("ac2-cprov").value = "openai"; ui.f("ac2-cprov").listeners.change();
  const typed = { "ac2-timeout": "65", "ac2-temperature": "0", "ac2-max-tokens": "4567", "ac2-extra-headers": '{"X-U5":"yes"}', "ac2-extra-body": '{"top_p":0.9}', "ac2-tier": "priority", "ac2-claude-cache": "ephemeral_5m" };
  for (const [id, value] of Object.entries(typed)) ui.f(id).value = value;
  await ui.click("평론가·임베딩 저장");
  let c = ui.posts.filter((p) => p.url === "/settings/models").at(-1).body.critic;
  assert.deepStrictEqual([c.timeout_ms, c.temperature, c.max_tokens, c.extra_headers_json, c.extra_body_json, c.service_tier, c.claude_prompt_cache], [65000, 0, 4567, typed["ac2-extra-headers"], typed["ac2-extra-body"], "priority", "ephemeral_5m"]);
  for (const id of ["ac2-timeout", "ac2-temperature", "ac2-max-tokens"]) { assert.notStrictEqual(ui.f(id).value, ""); ui.f(id).value = ""; }
  await ui.click("평론가·임베딩 저장"); c = ui.posts.filter((p) => p.url === "/settings/models").at(-1).body.critic;
  assert.deepStrictEqual([c.timeout_ms, c.temperature, c.max_tokens], [null, null, null]);
  for (const code of ["bad_extra_headers_json", "bad_extra_body_json"]) { ui.fail(code); await ui.click("평론가·임베딩 저장"); assert.ok(ui.text(ui.body).includes(code.includes("headers") ? "Extra Headers는 JSON 객체" : "Extra Body는 JSON 객체")); }
});

test("U5 timeout displays saved milliseconds as seconds and sends seconds as milliseconds", async () => {
  for (const stored of [undefined, null, 12000, 12345, 0]) {
    const ui = await u5Screen();
    ui.view.critic.timeout_ms = stored;
    await ui.models();
    assert.strictEqual(ui.f("ac2-timeout").value, stored == null ? "" : String(stored / 1000));
    assert.strictEqual(ui.f("ac2-timeout")["aria-label"], "응답 대기 시간(초)");
    assert.ok(ui.text(ui.body).includes("비우면 600초(10분)"));
    await ui.click("평론가·임베딩 저장");
    assert.strictEqual(ui.posts.filter((p) => p.url === "/settings/models").at(-1).body.critic.timeout_ms, stored ?? null);
    for (const [seconds, milliseconds] of [["600", 600000], ["12", 12000], ["1.25", 1250], ["1.001", 1001], ["0", 0], ["", null]]) {
      ui.f("ac2-timeout").value = seconds;
      await ui.click("평론가·임베딩 저장");
      const sent = ui.posts.filter((p) => p.url === "/settings/models").at(-1).body.critic;
      assert.strictEqual(sent.timeout_ms, milliseconds);
      assert.strictEqual(ui.f("ac2-timeout").value, seconds);
    }
  }
});

test("U5 prompt editor saves the whole instruction and restores through the backend", async () => {
  const ui = await u5Screen(); await ui.models();
  assert.ok(ui.f("ac2-prompt").value.includes("JSON shape"));
  assert.ok(ui.text(ui.body).includes("잘못 고치면 기억 만들기가 실패"));
  ui.f("ac2-prompt").value = 'Changed instructions\nJSON: {"memories":[]}';
  const critic = ui.all(ui.body).find((d) => d.tag === "details" && ui.all(d).some((e) => e.id === "ac2-prompt"));
  const inCritic = (label) => ui.all(critic).find((e) => e.tag === "button" && ui.text(e) === label).listeners.click(); // the publisher box has the same buttons
  await inCritic("지시문 저장");
  assert.deepStrictEqual(ui.posts.at(-1), { url: "/settings/prompt", body: { content: 'Changed instructions\nJSON: {"memories":[]}' } });
  await inCritic("기본값 복원");
  assert.deepStrictEqual(ui.posts.at(-1), { url: "/settings/prompt", body: { reset: true } });
  assert.strictEqual(ui.f("ac2-prompt").value, "Default instructions including JSON shape");
});

test("publisher prompt editor (user 11-09) saves and restores its own text, apart from the critic's", async () => {
  const ui = await u5Screen(); await ui.models();
  assert.strictEqual(ui.f("ac2-pub-prompt").value, "Default guide rules");
  assert.ok(ui.text(ui.body).includes("출판사 프롬프트 편집") && ui.text(ui.body).includes("단계별 항목과 출력 형식은 자동으로 붙어요"));
  const buttons = (label) => ui.all(ui.body).filter((e) => e.tag === "button" && ui.text(e) === label);
  assert.strictEqual(buttons("지시문 저장").length, 2, "one save per prompt");
  const pub = buttons("지시문 저장").find((b) => ui.all(ui.body).some((d) => d.tag === "details" && ui.all(d).includes(b) && ui.all(d).some((e) => e.id === "ac2-pub-prompt")));
  ui.f("ac2-pub-prompt").value = "느와르 규칙";
  await pub.listeners.click();
  assert.deepStrictEqual(ui.posts.at(-1), { url: "/settings/publisher-prompt", body: { content: "느와르 규칙" } });
  const reset = buttons("기본값 복원").find((b) => ui.all(ui.body).some((d) => d.tag === "details" && ui.all(d).includes(b) && ui.all(d).some((e) => e.id === "ac2-pub-prompt")));
  await reset.listeners.click();
  assert.deepStrictEqual(ui.posts.at(-1), { url: "/settings/publisher-prompt", body: { reset: true } });
  assert.strictEqual(ui.f("ac2-pub-prompt").value, "Default guide rules");
  assert.ok(!ui.posts.some((p) => p.url === "/settings/prompt"), "the critic's prompt is not sent");
});

test("models tab (user 11-10): each prompt editor sits in its own AI's box and tells whether a saved edit is in use", async () => {
  const ui = await u5Screen(); await ui.models();
  const boxOf = (id) => ui.all(ui.body).filter((d) => d.tag === "div" && (d.className || d.class || "").includes("ac2-box") && ui.all(d).some((e) => e.id === id));
  const critic = boxOf("ac2-prompt").find((d) => ui.text(d).includes("평론가 · 대화를 읽고 기억을 만듭니다"));
  assert.ok(critic, "the critic prompt is inside the critic box");
  assert.ok(ui.all(critic).some((e) => e.id === "ac2-cprov") && !ui.all(critic).some((e) => e.id === "ac2-pub-prompt"), "the critic box holds the critic's provider and only its own prompt");
  const pub = boxOf("ac2-pub-prompt").find((d) => ui.all(d).some((e) => e.id === "ac2-guide-level"));
  assert.ok(pub && !ui.all(pub).some((e) => e.id === "ac2-prompt"), "the publisher prompt is inside the publisher box");
  const tellsIn = (box) => ui.text(box.children.find((d) => d.tag === "details" && ui.all(d).some((e) => e.id === "ac2-prompt" || e.id === "ac2-pub-prompt")) || box);
  assert.ok(tellsIn(critic).includes("기본 지시문을 쓰는 중이에요"));
  ui.f("ac2-prompt").value = "My rules";
  await ui.all(critic).find((e) => e.tag === "button" && ui.text(e) === "지시문 저장").listeners.click();
  assert.ok(ui.text(critic).includes("직접 고친 지시문을 쓰는 중이에요"), ui.text(critic));
  // a critic/embedding save redraws; the saved (not dirty) prompt is read again, still inside the critic box
  await ui.click("평론가·임베딩 저장");
  const again = boxOf("ac2-prompt").find((d) => ui.text(d).includes("평론가 · 대화를 읽고 기억을 만듭니다"));
  assert.ok(again && ui.f("ac2-prompt").value === "My rules");
  // an unsaved edit survives the redraw in the critic box
  ui.f("ac2-prompt").value = "typing";
  await ui.click("평론가·임베딩 저장");
  assert.strictEqual(ui.f("ac2-prompt").value, "typing");
  assert.ok(boxOf("ac2-prompt").some((d) => ui.text(d).includes("평론가 · 대화를 읽고 기억을 만듭니다")));
});

test("models tab (review 11-10): each save tells its result in its own place, and the publisher save keeps an unsaved publisher prompt", async () => {
  const ui = await u5Screen(); await ui.models();
  const inPrompt = (id) => ui.all(ui.body).filter((d) => d.tag === "details" && ui.all(d).some((e) => e.id === id)).flatMap((d) => ui.all(d));
  const statuses = () => ui.all(ui.body).filter((e) => e.role === "status" && !e.hidden);
  ui.f("ac2-prompt").value = "critic typing";
  await ui.click("평론가·임베딩 저장");
  const told = statuses().filter((e) => ui.text(e).includes("평론가·임베딩 설정을 저장했어요"));
  assert.strictEqual(told.length, 1, "told once");
  assert.ok(!inPrompt("ac2-prompt").includes(told[0]), "not inside the critic prompt editor");
  assert.strictEqual(ui.f("ac2-prompt").value, "critic typing", "the unsaved critic prompt stays");
  assert.ok(!ui.posts.some((p) => p.url === "/settings/prompt"), "the critic prompt was not sent");
  ui.f("ac2-pub-prompt").value = "publisher typing"; ui.f("ac2-guide-chars").value = "2500";
  await ui.click("출판사 설정 저장");
  assert.strictEqual(ui.posts.at(-1).url, "/settings/narrative");
  assert.strictEqual(ui.f("ac2-pub-prompt").value, "publisher typing", "the unsaved publisher prompt stays");
  const pubTold = statuses().filter((e) => ui.text(e).includes("출판사 설정을 저장했어요"));
  assert.strictEqual(pubTold.length, 1);
  assert.ok(!inPrompt("ac2-pub-prompt").includes(pubTold[0]), "not inside the publisher prompt editor");
});

test("models tab (review 11-10 r3): a save whose reread fails still keeps the unsaved prompt edits", async () => {
  const ui = await u5Screen(); await ui.models();
  ui.f("ac2-prompt").value = "critic typing"; ui.f("ac2-pub-prompt").value = "publisher typing";
  ui.failGet("/settings/models");
  await ui.click("평론가·임베딩 저장");
  assert.strictEqual(ui.posts.at(-1).url, "/settings/models");
  assert.strictEqual(ui.f("ac2-prompt") && ui.f("ac2-prompt").value, "critic typing", "the critic prompt edit stays after the failed reread");
  assert.strictEqual(ui.f("ac2-pub-prompt") && ui.f("ac2-pub-prompt").value, "publisher typing", "the publisher prompt edit stays after the failed reread");
  ui.failGet("");
  const ui2 = await u5Screen(); await ui2.models();
  ui2.f("ac2-pub-prompt").value = "publisher typing";
  ui2.failGet("/settings/narrative");
  await ui2.click("출판사 설정 저장");
  assert.strictEqual(ui2.posts.at(-1).url, "/settings/narrative");
  assert.strictEqual(ui2.f("ac2-pub-prompt") && ui2.f("ac2-pub-prompt").value, "publisher typing", "the publisher prompt edit stays after the failed reread");
});

test("prompt editor (review 11-10): saving the default text unchanged still counts as a saved edit, as the backend says", async () => {
  const ui = await u5Screen(); await ui.models();
  const critic = ui.all(ui.body).find((d) => d.tag === "details" && ui.all(d).some((e) => e.id === "ac2-prompt"));
  assert.ok(ui.text(critic).includes("기본 지시문을 쓰는 중이에요"));
  await ui.all(critic).find((e) => e.tag === "button" && ui.text(e) === "지시문 저장").listeners.click();
  assert.ok(ui.text(critic).includes("직접 고친 지시문을 쓰는 중이에요"), ui.text(critic));
  await ui.all(critic).find((e) => e.tag === "button" && ui.text(e) === "기본값 복원").listeners.click();
  assert.ok(ui.text(critic).includes("기본 지시문을 쓰는 중이에요"));
});

test("U5 memory settings render backend choices and send placement and anchor", async () => {
  const ui = await u5Screen(); await ui.budget();
  assert.strictEqual(ui.f("ac2-transport").value, "text");
  assert.deepStrictEqual(ui.f("ac2-transport").children.map((e) => e.value), ["text", "google_pdf", "llm_gateway_pdf", "provider_manager_pdf"]);
  assert.deepStrictEqual(ui.f("ac2-placement").children.map((e) => e.value), ["auto", "before_latest_user", "after_anchor_marker", "after_last_cache_point", "after_first_system", "end"]);
  ui.f("ac2-placement").value = "after_anchor_marker"; ui.f("ac2-anchor").value = "<U5>";
  ui.f("ac2-transport").value = "google_pdf";
  await ui.click("저장");
  assert.deepStrictEqual(ui.posts.at(-1), { url: "/settings/memory", body: { memory_chars: 2000, secret_chars: 4000, placement: "after_anchor_marker", anchor: "<U5>", transport_mode: "google_pdf" } });
  assert.strictEqual(ui.f("ac2-transport").value, "google_pdf");
  ui.f("ac2-transport").value = "provider_manager_pdf";
  await ui.click("저장");
  assert.strictEqual(ui.posts.at(-1).body.transport_mode, "provider_manager_pdf");
  assert.strictEqual(ui.f("ac2-transport").value, "provider_manager_pdf");
});

test("U5 plugin applies backend indices, sends placement observations, and keeps hash-only text private", async () => {
  for (const captureOff of [false, true]) {
    const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), captureOff };
    const h = await loadPlugin(state);
    try {
      const messages = [{ role: "system", content: "<U5>" }, { role: "assistant", content: "cache", cache_control: { type: "ephemeral" } }, { role: "user", content: "now" }, { role: "assistant", content: "prefill" }];
      for (const index of [0, 1, 2, 3, 4]) {
        state.injectReply = { block: "chosen memory", placement: { index }, anchor: "<U5>" };
        const out = await h.beforeRequest[0](messages, "model");
        assert.strictEqual(out[index].content, "chosen memory");
        assert.deepStrictEqual(out.filter((m) => m.content !== "chosen memory"), messages);
      }
      const obs = state.injects.at(-1).placement_messages;
      assert.deepStrictEqual(obs.map((m) => m.cache_control), [undefined, { type: "ephemeral" }, undefined, undefined]);
      assert.ok(!("cachePoint" in obs[0]) && !("isCache" in obs[0]), "unexposed cache metadata must stay absent");
      // never the text: only whether a message holds the anchor the backend named
      assert.ok(obs.every((m) => !("content" in m)), "placement must not carry prompt text");
      assert.deepStrictEqual(obs.map((m) => m.marker), [true, false, false, false]);
      assert.ok(!state.calls.filter((c) => c.url.endsWith("/observe")).some((c) => JSON.stringify(c.body).includes("placement_messages")), "placement text must not enter durable observation batches");
    } finally { await state.unload(); }
  }
});

test("U5 backend placement integration", { skip: !process.env.AC2_U5_BACKEND }, async () => {
  const base = process.env.AC2_U5_BACKEND;
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([{ role: "user", chatId: "u1", data: "과거 입력" }, { role: "char", chatId: "a1", data: "과거 응답" }, { role: "user", chatId: "u2", data: "다음 입력" }, { role: "char", chatId: "a2", data: "다음 응답" }, { role: "user", chatId: "u3", data: "최근 입력" }, { role: "char", chatId: "a3", data: "최근 응답" }]) };
  const host = makeHost(state), standIn = host.api.nativeFetch;
  const plans = [];
  host.api.nativeFetch = async (url, init) => {
    if (!url.endsWith("/memory/inject")) return standIn(url, init);
    const r = await fetch(base + "/memory/inject", init), raw = await r.text();
    assert.ok(r.ok, raw); plans.push(JSON.parse(raw));
    return { ok: r.ok, status: r.status, text: async () => raw };
  };
  const ctx = vm.createContext({ Risuai: host.api, console: { warn() {}, log() {} }, setTimeout: (fn, ms) => ms >= 10000 ? {} : setTimeout(fn, ms), clearTimeout, AbortController, Date, Math, JSON, Promise, Map, TextEncoder });
  await vm.runInContext(SRC, ctx); await tick(20);
  try {
    const messages = [{ role: "system", content: "card <U5>" }, { role: "user", content: "old" }, { role: "assistant", content: "cache", isCache: true }, { role: "system", content: "late <U5>" }, { role: "user", content: "리나의 직업은?" }, { role: "assistant", content: "prefill" }];
    // Expected positions follow AC1's last marker/cache semantics and the
    // pre-existing AC2 auto behavior, independently of the patch's resolver.
    for (const [mode, index] of [["auto", 4], ["before_latest_user", 4], ["after_anchor_marker", 4], ["after_last_cache_point", 3], ["after_first_system", 1], ["end", 6]]) {
      const saved = await fetch(base + "/settings/memory", { method: "POST", body: JSON.stringify({ placement: mode, anchor: "<U5>" }) }); assert.ok(saved.ok);
      const out = await host.handlers.beforeRequest[0](messages, "model");
      const reply = plans.at(-1); assert.ok(reply.block, "real backend must deliver seeded memory: " + JSON.stringify(reply));
      assert.strictEqual(reply.placement.index, index, mode + " backend instruction");
      assert.strictEqual(out[index].content, reply.block, mode + " Host application");
      assert.deepStrictEqual(out.filter((m) => m.content !== reply.block), messages);
    }
  } finally { await state.unload(); }
});
// U6: full production plugin with asynchronous SafeElement boundaries.
// No HUD renderer, polling policy or retry eligibility is replaced here.
class U6Element {
  constructor(tag) { U6Element.made++; this.tag = tag; this.children = []; this.listeners = {}; this.style = ""; this.text = ""; this.parent = null; this.checked = false; }
  setAttribute(k, v) { this[k] = String(v); }
  addEventListener(type, fn) { const id = "event-" + Object.keys(this.listeners).length; this.listeners[id] = { type, fn }; return Promise.resolve(id); }
  async removeEventListener(type, id) { assert.ok(this.listeners[id], "listener removed from wrong SafeElement"); delete this.listeners[id]; }
  async setTextContent(text) { this.text = text; }
  async setStyleAttribute(style) { this.style = style; }
  async appendChild(child) { if (child.parent) child.parent.children = child.parent.children.filter((e) => e !== child); child.parent = this; this.children.push(child); }
  async remove() { if (this.parent) this.parent.children = this.parent.children.filter((e) => e !== this); this.parent = null; }
  append(...kids) { this.children.push(...kids); }
  replaceChildren(...kids) { this.children = kids; }
  async click() { for (const { type, fn } of Object.values(this.listeners)) if (type === "click") await fn({ type: "click" }); }
}
const u6All = (e) => [e, ...e.children.filter((c) => c instanceof U6Element).flatMap(u6All)];
const u6Text = (e) => e == null ? "" : typeof e === "string" ? e : e.text + e.children.map(u6Text).join(" ");
U6Element.made = 0;
const u6View = (active, failed = false) => {
  const stages = [{ key: "observed", state: "done", at: "2026-10-28T00:00:00Z", elapsed_ms: 0 },
    { key: "generation", state: failed ? "failed" : active ? "running" : "done", at: "2026-10-28T00:00:01Z", elapsed_ms: 1400, count: 2, detail: failed ? "synthetic error" : "" }];
  return { chat: "chat-1", settings: { enabled: true, compact: true }, active,
    turns: [{ turn: 3, stages, current_stage: stages[stages.length - 1], retry: failed, ids: ["M7"],
      injection: { items: 1, chars: 17, chars_max: 100, secret_chars: 0, secret_max: 50 } }], notices: [{ turn: 2, kind: "response_replaced" }] };
};

async function u6Host(options = {}) {
  const state = { storeData: {}, calls: [], backendUp: true, rereadChat: chat([]), ...options };
  const host = makeHost(state), originalFetch = host.api.nativeFetch;
  const root = new U6Element("body"), frame = new U6Element("body");
  const timers = [], settings = [], asks = [], posts = [];
  let hostReads = 0, permissions = 0;
  const doc = { querySelector: async (s) => { assert.strictEqual(s, "body"); return root; }, createElement: async (tag) => new U6Element(tag) };
  host.api.getChatFromIndex = async () => { hostReads++; return state.rereadChat; };
  host.api.getRootDocument = async () => { permissions++; if (state.permissionError) throw new Error("permission denied"); return state.denied ? null : doc; };
  host.api.registerSetting = async (name, fn, icon, type, id) => { settings.push({ name, fn, id }); };
  host.api.showContainer = async (mode) => assert.strictEqual(mode, "fullscreen");
  host.api.hideContainer = async () => {};
  host.api.nativeFetch = async (url, init) => {
    if (url.includes("/status")) return { ok: true, status: 200, text: async () => JSON.stringify({ chip: "done", chat: "chat-1", version: "synthetic", generation: { critic: {}, chat: {} }, embedding: {}, activity: [] }) };
    if (url.includes("/hud?")) {
      asks.push(url);
      const result = JSON.parse(JSON.stringify(state.hud));
      if (state.hudGate) await state.hudGate;
      return { ok: true, status: 200, text: async () => JSON.stringify(result) };
    }
    if (url.endsWith("/settings/hud")) {
      if (init.method === "POST") { state.hud.settings = JSON.parse(init.body); posts.push({ url, body: state.hud.settings }); }
      return { ok: true, status: 200, text: async () => JSON.stringify(state.hud.settings) };
    }
    if (url.endsWith("/hud/retry")) {
      posts.push({ url, body: JSON.parse(init.body) });
      state.hud = u6View(true);
      return { ok: true, status: 200, text: async () => JSON.stringify(state.hud) };
    }
    return originalFetch(url, init);
  };
  const set = (fn, ms) => { if (ms === 2000 || ms === 15000 || ms === 10000) { const h = { fn, ms, cleared: false }; timers.push(h); return h; } return setTimeout(fn, ms); };
  const clear = (h) => { if (h && typeof h === "object" && "cleared" in h) h.cleared = true; else clearTimeout(h); };
  const ctx = vm.createContext({ Risuai: host.api, document: { body: frame, createElement: (tag) => new U6Element(tag), getElementById: () => null, head: new U6Element("head") },
    console: { warn() {}, log() {} }, setTimeout: set, clearTimeout: clear, AbortController, TextEncoder, Date, Math, JSON, Promise, Map });
  await vm.runInContext(SRC, ctx);
  await tick(30);
  return { state, root, frame, host, asks, posts, settings, timers, reads: () => hostReads, permissions: () => permissions,
    pending: () => timers.filter((t) => t.ms === 2000 && !t.cleared),
    fire: async () => { const t = timers.find((t) => t.ms === 2000 && !t.cleared); assert.ok(t, "HUD should have a timer"); t.cleared = true; await t.fn(); await tick(10); },
    close: () => state.unload() };
}


test("turn HUD: dragged by its chip, kept on this device, put back from the settings", async () => {
  const fire = async (el, type, ev) => { for (const { type: t, fn } of Object.values(el.listeners)) if (t === type) await fn({ type, ...ev }); };
  const view = u6View(true); // active: read again while dragged
  const h = await u6Host({ hud: view });
  await tick(20);
  const wrap = () => h.root.children.find((c) => c.tag === "section");
  const chip = () => u6All(wrap()).find((e) => e.tag === "button" && u6Text(e).includes("AC2 ·"));
  assert.ok(wrap().style.includes("calc(12px - 0px)") && wrap().style.includes("- 0px),calc(100vh"), wrap().style);
  // a press that hardly moved is a click (it opens the panel)
  await fire(chip(), "pointerdown", { clientX: 100, clientY: 100 });
  await fire(h.root, "pointermove", { clientX: 102, clientY: 101 });
  await fire(h.root, "pointerup", {});
  await chip().click(); await tick(20);
  assert.ok(u6Text(h.root).includes("AC2 기억 처리"), "a short press opens");
  await chip().click(); await tick(20);
  // a drag moves it and is no click
  await fire(chip(), "pointerdown", { clientX: 100, clientY: 100 });
  await fire(h.root, "pointermove", { clientX: 60, clientY: 150 });
  assert.ok(wrap().style.includes("calc(12px - -40px)") && wrap().style.includes("- 50px),calc(100vh"), "follows the pointer: " + wrap().style);
  // a redraw while dragging (the view read again) stays where the drag is
  h.state.hud = u6View(true, true);
  await h.fire();
  assert.ok(wrap().style.includes("calc(12px - -40px)"), "a redraw moved it back: " + wrap().style);
  await fire(h.root, "pointerup", {});
  await tick(20);
  assert.ok(wrap().style.includes("calc(12px - -40px)"), "after the drop: " + wrap().style);
  // the whole HUD stays on the screen: its width and, open, the room above it
  assert.ok(wrap().style.includes("calc(100vw - min(300px,calc(100vw - 24px)))"), wrap().style);
  const shownBefore = u6Text(h.root).includes("AC2 기억 처리");
  await chip().click(); await tick(20);
  assert.strictEqual(u6Text(h.root).includes("AC2 기억 처리"), shownBefore, "the drag's end toggled the panel");
  assert.deepStrictEqual(h.state.storeData.ac2_hud_pos_v1, { dx: -40, dy: 50 });
  if (!u6Text(h.root).includes("AC2 기억 처리")) { await chip().click(); await tick(20); }
  const panel = u6All(wrap()).find((e) => e.tag === "div" && /max-height/.test(e.style));
  assert.ok(panel && panel.style.includes("calc(100vh - clamp(0px,calc(35vh + env(safe-area-inset-bottom,0px) - 50px)"), panel && panel.style);
  await h.close();
  // kept: a new start puts it where it was left
  const h2 = await u6Host({ hud: u6View(false), storeData: { ac2_hud_pos_v1: { dx: -40, dy: 50 } } });
  await tick(30);
  const w2 = h2.root.children.find((c) => c.tag === "section");
  assert.ok(w2 && w2.style.includes("calc(12px - -40px)"), w2 && w2.style);
  await h2.close();
  // put back from the settings screen
  const h3 = await u6Host({ hud: u6View(false), storeData: { ac2_hud_pos_v1: { dx: -40, dy: 50 } } });
  await tick(30);
  await h3.settings[0].fn();
  await tick(30);
  const all = (e) => [e, ...(e.children || []).filter((c) => c && typeof c === "object").flatMap(all)];
  const back = all(h3.frame).find((e) => e.id === "ac2-hud-place");
  assert.ok(back, "no put-back button in the settings");
  await back.click();
  await tick(30);
  const w3 = h3.root.children.find((c) => c.tag === "section");
  assert.strictEqual(h3.state.storeData.ac2_hud_pos_v1, null);
  assert.ok(w3 && w3.style.includes("calc(12px - 0px)"), w3 && w3.style);
  await h3.close();
});

// ---- The screens: chats, chat actions, memories,
// maintenance and the turn HUD. The real screen functions run against a
// small DOM and a bridge that records every request and answers from the
// test's table; nothing here is a copy of the plugin's logic.
class UiEl {
  constructor(tag) { this.tag = tag; this.children = []; this.listeners = {}; this._value = ""; this.textContent = ""; this.className = ""; this.hidden = false; this.disabled = false; this.checked = false; this.parent = null; }
  setAttribute(k, v) { if (k === "value") this.value = v; else if (k === "class") this.className = v; else this[k] = String(v); }
  getAttribute(k) { return this[k] === undefined ? null : this[k]; }
  addEventListener(k, fn) { this.listeners[k] = fn; }
  click() { if (this.listeners.click) return this.listeners.click(); }
  // as the DOM: a node has one parent, and it is shown only under a root
  append(...v) { for (const c of v) { if (c && typeof c === "object") { if (c.parent) c.parent.children = c.parent.children.filter((x) => x !== c); c.parent = this; } this.children.push(c); } }
  replaceChildren(...v) { for (const c of this.children) if (c && typeof c === "object" && c.parent === this) c.parent = null; this.children = []; this.append(...v.map((x) => (x === null || x === undefined ? String(x) : x))); } // as the DOM: a null becomes the text "null"
  get isConnected() { let n = this; while (n.parent) n = n.parent; return n.root === true; }
  // as the DOM: setting textContent replaces the children
  get textContent() { return this._text || ""; }
  set textContent(v) { if (this.children) { for (const c of this.children) if (c && typeof c === "object" && c.parent === this) c.parent = null; this.children = []; } this._text = v == null ? "" : String(v); }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter((c) => c !== this); this.parent = null; }
  get value() { return this.tag === "select" ? ((this.children.find((c) => c.selected) || this.children[0] || {}).value || "") : this._value; }
  set value(v) { if (this.tag === "select") for (const c of this.children) c.selected = c.value === String(v); else this._value = String(v); }
  querySelector(q) { return uiAll(this).find((e) => e !== this && (q.startsWith(".") ? (" " + e.className + " ").includes(" " + q.slice(1) + " ") : q === "[role=status]" && e.role === "status")) || null; }
  get classList() { const e = this; return { toggle(c, on) { const has = (" " + e.className + " ").includes(" " + c + " "); if (on && !has) e.className += " " + c; if (!on && has) e.className = e.className.replace(c, "").trim(); } }; }
}
const uiAll = (e) => [e, ...e.children.filter((c) => c instanceof UiEl).flatMap(uiAll)];
const uiText = (e) => (e == null ? "" : typeof e === "string" ? e : (e.textContent || "") + e.children.map(uiText).join(""));
const uiNoNull = (e) => assert.ok(!/(^|[^a-z])(null|undefined)([^a-z]|$)/.test(uiText(e)), "a null or undefined was drawn as text: " + uiText(e));
const uiShown = (e) => { for (let x = e; x; x = x.parent) if (x.hidden) return false; return true; };

function uiSim(answers, view = { id: "A", name: "리나 / 첫 채팅", open: "A", isOpen: true }) {
  const calls = [];
  const shown = [];
  const timers = [];
  const downloads = [], revoked = [];
  let pendingDownload;
  class DiagnosticURL extends URL {
    static createObjectURL(blob) { pendingDownload = blob; return "blob:diagnostic-test"; }
    static revokeObjectURL(url) { revoked.push(url); }
  }
  let hudChanges = 0;
  const ctx = vm.createContext({
    document: { createElement: (t) => {
      const e = new UiEl(t);
      if (t === "a") e.click = () => {
        assert.strictEqual(e.href, "blob:diagnostic-test");
        assert.match(e.download, /^AC2-diagnostics-.*\.json$/);
        downloads.push(pendingDownload);
      };
      return e;
    }, body: Object.assign(new UiEl("body"), { root: true }), head: new UiEl("head"), getElementById: () => null },
    api: { showContainer: async () => {}, hideContainer: async () => {}, registerButton: async () => {} }, unloaded: false,
    console: { warn() {}, log() {} },
    URL: DiagnosticURL, Blob,
    PLUGIN_VERSION: SRC.match(/const PLUGIN_VERSION = "([^"]+)"/)[1], runtime: { platform: "node", apiVersion: "3.0" },
    conn: async () => ({ url: "https://ignored:credentials@backend.example:28190/private?token=never-in-report", token: "local-test-token" }),
    setTimeout: (fn, ms) => { const h = { fn, ms }; timers.push(h); return h; }, clearTimeout: (h) => { if (h) h.cleared = true; },
    __stub: { chatInView: async () => ({ ...view }), currentChat: async () => ({ id: view.open, name: view.isOpen ? view.name : "" }), currentChatId: async () => view.open,
      showChat: (c) => shown.push(c), refreshChip() {}, hudSettingsChanged() { hudChanges++; } },
    bridge: async (path, init) => {
      const body = init && init.body ? JSON.parse(init.body) : null;
      calls.push({ path, method: init.method, body, timeoutMs: init.timeoutMs });
      for (const [match, reply] of answers) if (path === match || path.startsWith(match + "?")) return typeof reply === "function" ? reply(body, path) : JSON.parse(JSON.stringify(reply));
      throw new Error("unexpected request " + path);
    },
  });
  vm.runInContext(SRC.slice(SRC.indexOf("  const pluginErrors"), SRC.indexOf("  // Each plugin load")) +
    SRC.slice(SRC.indexOf("  const ERR_TEXT"), SRC.indexOf('  await register("settings"')) +
    // the host reads and the HUD are the test's (the slice defines its own)
    "\nchatInView = __stub.chatInView; currentChat = __stub.currentChat; currentChatId = __stub.currentChatId; showChat = __stub.showChat; refreshChip = __stub.refreshChip; hudSettingsChanged = __stub.hudSettingsChanged;" +
    "\nthis.chatsTab = chatsTab; this.memoriesTab = memoriesTab; this.turnTab = turnTab; this.maintenanceBox = maintenanceBox; this.updateTab = updateTab; this.openSettings = openSettings; this.statusTab = statusTab; this.diagnosticsBox = diagnosticsBox; this.recordPluginError = recordPluginError; this.setTab = (k) => { settingsTab = k; }; this.setSub = (k) => { memorySub = k; };", ctx);
  const body = Object.assign(new UiEl("div"), { root: true }); // the screen the test shows
  const btn = (label, within = body) => uiAll(within).find((e) => e.tag === "button" && uiText(e) === label && uiShown(e));
  return { ctx, body, calls, shown, timers, downloads, revoked, view, hudChanges: () => hudChanges, btn,
    btns: (label) => uiAll(body).filter((e) => e.tag === "button" && uiText(e) === label && uiShown(e)),
    items: () => uiAll(body).filter((e) => e.className === "ac2-item"),
    confirm: () => uiAll(body).find((e) => (" " + e.className + " ").includes(" ac2-confirm ")),
    posts: (p) => calls.filter((c) => c.method === "POST" && c.path === p).map((c) => c.body) };
}
const OK = (json) => ({ ok: true, json });

test("E12 HypaMemory import reads the open chat's summaries, shows Go's count and cost first, and sends them all as they are", async () => {
  const sums = [{ text: "리나가 용을 만났다", isImportant: true, tags: ["용"], categoryId: "c1", chatMemos: ["m1"] }, { text: "리나가 용을 만났다" }, { text: "" }];
  let hostChat = { id: "A", hypaV3Data: { summaries: sums } };
  const s = uiSim([["/memory/backfill", OK({ total: 0 })], ["/memory/hypa", (b) => OK(b.dry_run ? { summaries: 2, to_make: 2, dry_run: true } : { summaries: 2, to_make: 2, turns: [1, 2] })]]);
  s.ctx.coords = async () => ({ character_index: 0, chat_index: 0 });
  s.ctx.api.getChatFromIndex = async () => hostChat;
  s.ctx.setSub("cold");
  await s.ctx.memoriesTab(s.body);
  await s.btn("HypaMemory 가져오기").listeners.click();
  const sent = [{ text: "리나가 용을 만났다", important: true, tags: ["용"], category: "c1" }, { text: "리나가 용을 만났다", important: false, tags: [], category: "" }, { text: "", important: false, tags: [], category: "" }];
  assert.deepStrictEqual(s.posts("/memory/hypa"), [{ chat_id: "A", chat_name: "리나 / 첫 채팅", summaries: sent, dry_run: true }], "all summaries, the duplicate too; nothing decided here");
  const ask = uiText(s.confirm());
  assert.ok(ask.includes("요약 2개") && ask.includes("2개는 새로 만들어") && ask.includes("비용"), ask);
  await s.btn("가져오기", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/memory/hypa").at(-1), { chat_id: "A", chat_name: "리나 / 첫 채팅", summaries: sent });
  assert.strictEqual(s.calls.filter((c) => c.path === "/memory/hypa").at(-1).timeoutMs, 0, "the import waits for the backend (review 15 #2)");
  assert.ok(uiText(s.body).includes("2개의 기억을 백엔드에서 만들고 있어요"), uiText(s.body));
  // no summaries, or another chat open: nothing is sent
  const before = s.posts("/memory/hypa").length;
  hostChat = { id: "A" };
  await s.btn("HypaMemory 가져오기").listeners.click();
  assert.ok(uiText(s.body).includes("HypaMemory 요약이 없어요"));
  hostChat = { id: "B", hypaV3Data: { summaries: sums } };
  await s.btn("HypaMemory 가져오기").listeners.click();
  assert.ok(uiText(s.body).includes("지금 열려 있는 채팅에서만"));
  assert.strictEqual(s.posts("/memory/hypa").length, before);
  uiNoNull(s.body);
});

test("diagnostics: downloads Go's merged report; the plugin's errors go to Go as caught (user 11-08: nothing hidden but credentials)", async () => {
  const safeReport = { contract_version: "ac2.diagnostics.v1", backend: { version: "fixture" }, plugin: { version: "fixture", recent_errors: [{ message: "[REDACTED]", time: "fixture" }] } };
  const s = uiSim([["/status", OK({ chip: "done", version: "fixture", chat: "A", generation: { critic: {}, chat: {} }, embedding: {}, activity: [] })],
    ["/settings/hud", OK({ enabled: false })], ["/diagnostics/report", (body) => {
    assert.deepStrictEqual(Object.keys(body), ["plugin"]);
    assert.strictEqual(body.plugin.version, SRC.match(/const PLUGIN_VERSION = "([^"]+)"/)[1]);
    assert.strictEqual(body.plugin.host_type, "node");
    assert.strictEqual(body.plugin.host_version, undefined, "API 3.0 is not the host version");
    assert.strictEqual(body.plugin.connection_hostname, "backend.example");
    assert.strictEqual(body.plugin.recent_errors.length, 1);
    assert.ok(body.plugin.recent_errors[0].message.includes("password=backend-must-redact"), "the caught error goes whole; Go masks it");
    return OK(safeReport); // only the external Go redaction boundary is stubbed
  }]]);
  s.ctx.recordPluginError(new Error("password=backend-must-redact"));
  await s.ctx.statusTab(s.body);
  assert.ok(uiText(s.body).includes("오류 원문과 로그가 그대로 들어 있어요") && uiText(s.body).includes("API 키와 접속 토큰 값만 가려요"));
  await s.btn("진단 보고서 저장").listeners.click();
  assert.strictEqual(s.downloads.length, 1, "one anchor download was clicked");
  assert.deepStrictEqual(JSON.parse(await s.downloads[0].text()), safeReport, "the actual downloaded blob is Go's result");
  assert.deepStrictEqual(s.calls.filter((x) => x.path === "/diagnostics/report").map((x) => x.method), ["POST"]);
  assert.deepStrictEqual(s.revoked, ["blob:diagnostic-test"]);
  assert.strictEqual(s.btn("진단 보고서 저장").disabled, false);
  assert.ok(uiText(s.body).includes("진단 보고서를 저장했어요"));
});

test("diagnostics: an offline status screen downloads the latest errors as recorded with credential values masked", async () => {
  const privateText = ["synthetic-dialog-body", "synthetic-prompt-body", "synthetic-memory-body", "secret with spaces"];
  const reason = 'parseRisuChat: ' + privateText.join(" ") + ' password="secret with spaces" local-test-token ac2-fake-token-123 sk-fakeAPIkey123 AIzaFakeApiKey1234567890123456789 Bearer bearer-secret api_key=api-secret user:dsn-secret@tcp(localhost:3306)/db https://user:url-secret@example.test/ open u:spaced dsn-pass@tcp(h)/x IDENTIFIED BY "sql-pass"';
  const s = uiSim([["/status", { ok: false, kind: "connect_failed" }], ["/diagnostics/report", { ok: false, kind: "connect_failed", detail: reason }]]);
  for (let i = 0; i < 45; i++) s.ctx.recordPluginError(new Error(i + " " + reason));
  await s.ctx.statusTab(s.body);
  await s.btn("진단 보고서 저장").listeners.click();
  assert.strictEqual(s.downloads.length, 1, "the offline report was downloaded too");
  const text = await s.downloads[0].text(), report = JSON.parse(text);
  assert.strictEqual(report.backend_report_available, false);
  assert.strictEqual(report.plugin.connection_hostname, "backend.example");
  assert.strictEqual(report.plugin.host_type, "node");
  assert.strictEqual(report.plugin.recent_errors.length, 40);
  const sent = s.calls.find((c) => c.path === "/diagnostics/report").body.plugin.recent_errors;
  assert.strictEqual(sent.length, 40);
  assert.ok(sent[0].message.startsWith("Error: 5 ") && sent.at(-1).message.startsWith("Error: 44 "), "the latest 40 caught errors go, with their stack");
  for (const e of report.plugin.recent_errors) {
    assert.deepStrictEqual(Object.keys(e).sort(), ["message", "time"]);
    assert.ok(Number.isFinite(Date.parse(e.time)));
    for (const kept of privateText.slice(0, 3)) assert.ok(e.message.includes(kept), "the error text stays: " + kept);
  }
  assert.ok(report.plugin.recent_errors[0].message.startsWith("Error: 5 parseRisuChat"), report.plugin.recent_errors[0].message.slice(0, 60));
  for (const secret of ['password=\\"secret', "local-test-token", "spaced dsn-pass", "sql-pass", "sk-fakeAPIkey123", "AIzaFakeApiKey", "bearer-secret", "api-secret", "dsn-secret", "url-secret"]) assert.ok(!text.includes(secret), secret);
  assert.ok(report.backend_report_error.startsWith("connect_failed · parseRisuChat: synthetic-dialog-body"), report.backend_report_error);
  assert.ok(uiText(s.body).includes("연결 실패 정보와 플러그인 정보를 저장했어요"));
  assert.deepStrictEqual(s.calls.map((x) => x.path), ["/status?chat_id=A", "/diagnostics/report"]);
  assert.deepStrictEqual(s.revoked, ["blob:diagnostic-test"]);
  uiNoNull(s.body);
});

test("diagnostics: offline failure keeps the HTTP status, code and detail with credential values masked", async () => {
  for (const code of ["token_invalid", "synthetic-private-error-code"]) {
    const s = uiSim([["/diagnostics/report", { ok: false, kind: "http_error", status: 401, code, detail: 'password="secret with spaces" synthetic-private-body' }]]);
    s.body.append(s.ctx.diagnosticsBox());
    await s.btn("진단 보고서 저장").listeners.click();
    const text = await s.downloads[0].text(), report = JSON.parse(text);
    assert.strictEqual(report.backend_report_error, "http_error · 401 · " + code + " · password=[가림] synthetic-private-body");
    assert.ok(!text.includes("secret with spaces"));
  }
});

test("updates: the production settings tab shows only Go's unconfigured message without checking a release", async () => {
  const message = "업데이트 위치가 설정되지 않았어요.";
  const s = uiSim([["/update/status", OK({ status: "not_configured", message, current_version: "fixture-current", update_available: false, apply_supported: false })]]);
  s.ctx.setTab("update");
  await s.ctx.openSettings();
  const root = s.ctx.document.body;
  const tab = s.btn("업데이트", root);
  assert.strictEqual(tab.getAttribute("aria-pressed"), "true", "the real settings router opened the update tab");
  const panel = uiAll(root).find((e) => e.className === "ac2-box");
  assert.strictEqual(uiText(panel), "프로그램 업데이트" + message);
  assert.ok(!s.btn("업데이트 확인", root) && !s.btn("지금 업데이트", root) && !s.btn("상태 새로고침", root));
  assert.deepStrictEqual(s.calls.map((c) => [c.path, c.method]), [["/update/status", "GET"]]);
  assert.strictEqual(s.timers.length, 0, "no release polling or retry timer");
  uiNoNull(root);
});

test("updates: check, apply and a manual status refresh use Go decisions without choosing a version in JS", async () => {
  const current = "fixture-current-z", latest = "fixture-target-a";
  let refreshed = false, finishApply;
  const applying = new Promise((resolve) => { finishApply = resolve; });
  const s = uiSim([
    ["/update/status", () => OK(refreshed ? { status: "committed", current_version: latest, target_version: latest, apply_supported: true }
      : { status: "idle", current_version: current, apply_supported: true })],
    ["/update/check", OK({ status: "ok", current_version: current, latest_version: latest, update_available: true, apply_supported: true, message: "Go가 시험 후보를 골랐어요." })],
    ["/update/apply", () => applying],
  ]);
  await s.ctx.updateTab(s.body);
  assert.ok(s.btn("업데이트 확인") && !s.btn("지금 업데이트"), "status alone does not invent a release");
  await s.btn("업데이트 확인").listeners.click();
  assert.ok(uiText(s.body).includes(current) && uiText(s.body).includes(latest) && uiText(s.body).includes("Go가 시험 후보를 골랐어요."));
  assert.ok(s.btn("지금 업데이트"), "the backend's decision works with opaque version strings");
  const pending = s.btn("지금 업데이트").listeners.click();
  assert.ok(s.btn("지금 업데이트").disabled && s.btn("업데이트 확인").disabled && s.btn("상태 새로고침").disabled);
  finishApply(OK({ status: "accepted", latest_version: latest, shutdown_requested: true, message: "적용을 준비했어요. 백엔드가 다시 시작돼요." }));
  await pending;
  assert.deepStrictEqual(s.posts("/update/apply"), [{}], "Go determines current version, platform and asset");
  assert.ok(uiText(s.body).includes("적용을 준비했어요. 백엔드가 다시 시작돼요.") && !s.btn("지금 업데이트"));
  assert.deepStrictEqual(s.calls.map((c) => [c.path, c.method, c.timeoutMs]), [
    ["/update/status", "GET", undefined], ["/update/check", "GET", 0], ["/update/apply", "POST", 0],
  ], "no automatic reconnect, secondary download, or verification request");
  refreshed = true;
  await s.btn("상태 새로고침").listeners.click();
  assert.ok(uiText(s.body).includes("업데이트 완료") && uiText(s.body).includes(latest));
  assert.strictEqual(s.calls.at(-1).path, "/update/status");
  assert.strictEqual(s.timers.length, 0);
  uiNoNull(s.body);
});

test("updates: no apply action when Go reports no candidate or no apply support", async () => {
  for (const decision of [{ update_available: false, apply_supported: true }, { update_available: true, apply_supported: false }]) {
    const s = uiSim([["/update/status", OK({ status: "idle" })], ["/update/check", OK({ status: "ok", ...decision, current_version: "fixture-current", latest_version: "fixture-other" })]]);
    await s.ctx.updateTab(s.body);
    await s.btn("업데이트 확인").listeners.click();
    assert.ok(!s.btn("지금 업데이트"), JSON.stringify(decision));
    assert.strictEqual(s.posts("/update/apply").length, 0);
  }
});

test("updates: Go verification failures are displayed and transport failure leaves a manual retry", async () => {
  let failStatus = true;
  const s = uiSim([
    ["/update/status", () => failStatus ? { ok: false, kind: "connect_failed" } : OK({ status: "idle" })],
    ["/update/check", OK({ status: "ok", update_available: true, apply_supported: true })],
    ["/update/apply", { ok: false, status: 400, code: "update_integrity_failed", json: { message: "시험 패키지 해시가 맞지 않아요." } }],
  ]);
  await s.ctx.updateTab(s.body);
  assert.ok(uiText(s.body).includes("백엔드에 연결할 수 없어요") && s.btn("상태 새로고침"));
  failStatus = false;
  await s.btn("상태 새로고침").listeners.click();
  await s.btn("업데이트 확인").listeners.click();
  await s.btn("지금 업데이트").listeners.click();
  const note = uiAll(s.body).find((e) => e.role === "status");
  assert.ok(uiShown(note) && uiText(note).includes("시험 패키지 해시가 맞지 않아요."));
  assert.ok(!s.btn("지금 업데이트").disabled && !s.btn("업데이트 확인").disabled);
  assert.strictEqual(s.posts("/update/apply").length, 1, "a failure is not automatically retried");
  assert.strictEqual(s.timers.length, 0);
});

test("update transport: production bridge can wait for Go staging while ordinary requests keep their timeout", async () => {
  const timers = [], requests = [];
  let finish;
  const ctx = vm.createContext({
    FETCH_TIMEOUT_MS: 8000, TOKEN_HEADER: "X-AC2-Token", AbortController,
    conn: async () => ({ url: "http://fixture-backend", token: "fixture-token" }),
    setTimeout(fn, ms) { const h = { fn, ms }; timers.push(h); return h; },
    clearTimeout(h) { if (h) h.cleared = true; },
    api: { nativeFetch: async (url, init) => { requests.push({ url, init }); return new Promise((resolve) => { finish = resolve; }); } },
  });
  vm.runInContext(SRC.slice(SRC.indexOf("  const pluginErrors"), SRC.indexOf("  // Each plugin load")) +
    SRC.slice(SRC.indexOf("  async function bridge("), SRC.indexOf("  // Sending never blocks")), ctx);
  const pending = ctx.bridge("/update/apply", { method: "POST", body: "{}", timeoutMs: 0 });
  await tick();
  assert.strictEqual(timers.length, 0, "staging is not aborted by the observation timer");
  assert.ok(!Object.hasOwn(requests[0].init, "timeoutMs"), "bridge options do not leak into fetch");
  assert.strictEqual(requests[0].init.headers["X-AC2-Token"], "fixture-token");
  finish({ ok: true, status: 200, text: async () => JSON.stringify({ status: "accepted" }) });
  assert.strictEqual((await pending).json.status, "accepted");
  const regular = ctx.bridge("/update/status", { method: "GET" });
  await tick();
  assert.strictEqual(timers.length, 1);
  assert.strictEqual(timers[0].ms, ctx.FETCH_TIMEOUT_MS);
  timers[0].fn();
  assert.strictEqual((await regular).kind, "timeout");
  assert.strictEqual(vm.runInContext("pluginErrors.length", ctx), 1, "the already caught timeout is available to diagnostics");
  assert.ok(requests[1].init.signal.aborted && timers[0].cleared);
});

const CHATS = { chats: [
  { chat_id: "B", char_name: "리나", chat_name: "두 번째", turns: 4, memories: { event: 1, fact: 2, view: 3, total: 6 }, names: 2, last_at: "2026-10-28T10:00:00Z", branch_of: { chat_id: "A", turn: 2 } },
  { chat_id: "A", char_name: "리나", chat_name: "첫 채팅", turns: 9, memories: { event: 4, fact: 5, view: 1, total: 10 }, names: 7, last_at: "2026-10-28T09:00:00Z", branch_of: null },
  { chat_id: "Cxxxxxxxxx", char_name: "", chat_name: "", turns: 1, memories: { event: 0, fact: 0, view: 0, total: 0 }, names: 0, last_at: null, branch_of: { chat_id: "gone-parent-id", turn: 0 } }] };

test("chats tab: backend order, names and counts, branch parents, open and viewed marks, choosing", async () => {
  const s = uiSim([["/chats", OK(CHATS)], ["/chats/attach", OK({ chats: [], turns: 0, memories: 0 })]], { id: "B", name: "리나 / 두 번째", open: "A", isOpen: false });
  const chosen = [];
  await s.ctx.chatsTab(s.body, (c) => chosen.push(c));
  const rows = s.items();
  assert.deepStrictEqual(rows.map((r) => uiText(r.children[0].children[0].children[0])), ["리나 / 두 번째", "리나 / 첫 채팅", "채팅 Cxxxxxxx"]);
  assert.ok(uiText(rows[0]).includes("턴 4 · 기억 6 (사건 1 · 사실 2 · 생각 3) · 이름 2"), uiText(rows[0]));
  assert.ok(uiText(rows[0]).includes("리나 / 첫 채팅에서 2턴 뒤 갈라짐"), "the parent by its name and the turn");
  assert.ok(uiText(rows[2]).includes("gone-par에서 갈라짐") && !uiText(rows[2]).includes("턴 뒤"), "a parent not listed by its id; turn 0 not named");
  assert.ok(uiText(rows[0]).includes("보는 중") && !uiText(rows[0]).includes("현재 채팅"));
  assert.ok(uiText(rows[1]).includes("현재 채팅") && !uiText(rows[1]).includes("보는 중"));
  assert.strictEqual(rows[0]["aria-current"], "true");
  await s.btn("기억 보기", rows[1]).listeners.click();
  assert.strictEqual(JSON.stringify(chosen), JSON.stringify([{ id: "A", name: "리나 / 첫 채팅" }]));
  assert.ok(uiText(s.body).includes("채팅 3개"));
  uiNoNull(s.body);
});

test("chats tab: the open chat without records comes first; refresh asks again and shows a failure", async () => {
  let fail = false;
  const s = uiSim([["/chats", () => (fail ? { ok: false, kind: "connect_failed" } : OK({ chats: CHATS.chats.slice(0, 1) }))], ["/chats/attach", OK({ chats: [], turns: 0, memories: 0 })]], { id: "N", name: "새 채팅", open: "N", isOpen: true });
  await s.ctx.chatsTab(s.body, () => {});
  const first = s.items()[0];
  assert.ok(uiText(first).includes("새 채팅") && uiText(first).includes("기록 없음") && uiText(first).includes("아직 기록이 없어요"), uiText(first));
  assert.ok(!s.btn("지금 채팅으로 복사", first), "the open chat cannot be copied into itself");
  assert.ok(s.btn("지금 채팅으로 복사", s.items()[1]), "another chat can");
  fail = true;
  await s.btn("새로고침").listeners.click();
  assert.strictEqual(s.calls.filter((c) => c.path === "/chats").length, 2);
  assert.ok(uiText(s.body).includes("백엔드에 연결할 수 없어요"));
  uiNoNull(s.body);
});

test("chat actions: copy previews the counts, runs only after the confirmation and redraws", async () => {
  const preview = { copied_turns: 3, copied_memories: 8, unmatched_turns: 1, existing_turns: 2, deleted_turns: 0, deleted_memories: 0, branches: [] };
  const s = uiSim([["/chats", OK(CHATS)], ["/chats/attach", OK({ chats: [], turns: 0, memories: 0 })], ["/chats/copy", (b) => OK(b.dry_run ? preview : { ...preview, copied_turns: 3 })]]);
  await s.ctx.chatsTab(s.body, () => {});
  const row = s.items()[0]; // B, not the open chat
  await s.btn("지금 채팅으로 복사", row).listeners.click();
  assert.deepStrictEqual(s.posts("/chats/copy"), [{ from: "B", to: "A", dry_run: true }]);
  const ask = uiText(s.confirm());
  assert.ok(ask.includes("옮길 턴 3개 · 기억 8개") && ask.includes("못 옮기는 턴 1개") && ask.includes("그대로 둘 턴 2개") && ask.includes("원본은 그대로"), ask);
  await s.btn("취소").listeners.click();
  assert.strictEqual(s.posts("/chats/copy").length, 1, "cancel sends nothing");
  await s.btn("지금 채팅으로 복사", s.items()[0]).listeners.click();
  const lists = s.calls.filter((c) => c.path === "/chats").length;
  await s.btn("지금 채팅으로 복사", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/chats/copy").at(-1), { from: "B", to: "A" });
  assert.strictEqual(s.calls.filter((c) => c.path === "/chats").length, lists + 1, "the list is drawn again");
  assert.ok(uiText(s.body).includes("지금 채팅으로 복사 완료 · 옮긴 턴 3개, 기억 8개"));
  uiNoNull(s.body);
});

test("chat actions: deleting the chat in view goes back to the open chat; failures stay in the confirmation", async () => {
  let fail = false;
  const preview = { deleted_turns: 4, deleted_memories: 6, branches: ["C"], copied_turns: 0, copied_memories: 0 };
  const s = uiSim([["/chats", OK(CHATS)], ["/chats/attach", OK({ chats: [], turns: 0, memories: 0 })], ["/chats/delete", (b) => (b.dry_run ? OK(preview) : fail ? { ok: false, status: 500, kind: "http_error" } : OK(preview))]], { id: "B", name: "", open: "A", isOpen: false });
  await s.ctx.chatsTab(s.body, () => {});
  await s.btn("삭제", s.items()[0]).listeners.click();
  const ask = uiText(s.confirm());
  assert.ok(ask.includes("지울 턴 4개 · 기억 6개") && ask.includes("갈라진 채팅 1개") && ask.includes("RisuAI 대화 원문은 그대로"), ask);
  fail = true;
  await s.btn("삭제", s.confirm()).listeners.click();
  assert.ok(s.confirm() && uiText(s.confirm()).includes("요청이 실패했어요"), "the error stays in the confirmation");
  assert.deepStrictEqual(s.shown, []);
  fail = false;
  await s.btn("삭제", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/chats/delete").filter((b) => !b.dry_run), [{ chat_id: "B" }, { chat_id: "B" }]);
  assert.deepStrictEqual(s.shown, [null], "the deleted chat was in view");
  assert.ok(uiText(s.body).includes("삭제 완료 · 지운 턴 4개, 기억 6개"));
  uiNoNull(s.body);
});

test("chats tab: other chats are put in order before the open chat (C22/C25), after a preview; the order shows and can be detached", async () => {
  let attached = [];
  const res = (chats) => ({ chats, turns: chats.reduce((n, c) => n + c.turns, 0), memories: chats.reduce((n, c) => n + c.memories, 0) });
  const attach = (b, path) => {
    if (path.includes("?")) return OK(res(attached));
    if (b.detach) { attached = []; return OK(res([])); }
    const chats = b.chats.map((id, i) => ({ chat_id: id, turns: i + 1, memories: 2 }));
    if (b.dry_run) return OK({ ...res(chats), dry_run: true });
    attached = chats;
    return OK(res(chats));
  };
  const s = uiSim([["/chats", OK(CHATS)], ["/chats/attach", attach]], { id: "B", name: "", open: "A", isOpen: false });
  await s.ctx.chatsTab(s.body, () => {});
  assert.deepStrictEqual(s.calls.filter((c) => c.path.startsWith("/chats/attach")).map((c) => c.path), ["/chats/attach?chat_id=A"], "the open chat's past is read with the list");
  assert.ok(uiText(s.items()[1]).includes("현재 채팅") && !s.btn("이어붙일 목록에 넣기", s.items()[1]), "the open chat is not put before itself");
  assert.ok(!s.btn("지금 채팅 앞에 이어붙이기") && !s.btn("과거 연결 풀기"), "nothing chosen, nothing attached");
  // chosen in this order: C, then B; B again changes nothing
  const card = (text) => s.items().find((r) => uiText(r).includes(text) && s.btn("기억 보기", r)); // a chat card, not a chosen row
  await s.btn("이어붙일 목록에 넣기", card("채팅 Cxxxxxxx")).listeners.click();
  await s.btn("이어붙일 목록에 넣기", card("두 번째")).listeners.click();
  await s.btn("이어붙일 목록에 넣기", card("두 번째")).listeners.click();
  assert.ok(uiText(s.body).includes("1. 채팅 Cxxxxxxx") && uiText(s.body).includes("2. 리나 / 두 번째") && !uiText(s.body).includes("3. "), uiText(s.body));
  await s.btn("빼기").listeners.click(); // the first one goes
  assert.ok(uiText(s.body).includes("1. 리나 / 두 번째") && !uiText(s.body).includes("Cxxxxxxx(") , uiText(s.body));
  await s.btn("이어붙일 목록에 넣기", card("채팅 Cxxxxxxx")).listeners.click();
  await s.btn("지금 채팅 앞에 이어붙이기").listeners.click();
  assert.deepStrictEqual(s.posts("/chats/attach"), [{ chat_id: "A", chats: ["B", "Cxxxxxxxxx"], dry_run: true }]);
  const ask = uiText(s.confirm());
  assert.ok(ask.includes("리나 / 두 번째(턴 1 · 기억 2) → 채팅 Cxxxxxxx(턴 2 · 기억 2) → 지금 채팅") && ask.includes("모두 턴 3개 · 기억 4개") && ask.includes("원본은 그대로"), ask);
  await s.btn("취소").listeners.click();
  assert.strictEqual(s.posts("/chats/attach").length, 1, "cancel sends nothing");
  await s.btn("지금 채팅 앞에 이어붙이기").listeners.click();
  await s.btn("이어붙이기", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/chats/attach").at(-1), { chat_id: "A", chats: ["B", "Cxxxxxxxxx"] });
  // the backend waits for running memory work before it writes: the answer is waited for
  assert.deepStrictEqual(s.calls.filter((c) => c.path === "/chats/attach").map((c) => c.timeoutMs), [0, 0, 0]);
  assert.ok(uiText(s.body).includes("앞부분 과거로 이어붙였어요 · 턴 3개, 기억 4개"), uiText(s.body));
  assert.ok(uiText(s.body).includes("지금 연결: 리나 / 두 번째(턴 1 · 기억 2) → 채팅 Cxxxxxxx(턴 2 · 기억 2) → 지금 채팅"), uiText(s.body));
  assert.ok(!s.btn("지금 채팅 앞에 이어붙이기"), "the chosen list is emptied after it is attached");
  await s.btn("과거 연결 풀기").listeners.click();
  await s.btn("연결 풀기", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/chats/attach").at(-1), { chat_id: "A", detach: true });
  assert.ok(!uiText(s.body).includes("지금 연결:") && uiText(s.body).includes("앞부분 과거 연결을 풀었어요"), uiText(s.body));
  uiNoNull(s.body);
});

test("chats tab: another chat is compared with the open chat (C26), read only, and a missing record is said", async () => {
  const side = (id, turns) => ({ chat_id: id, turns, memories: { event: 1, fact: 2, view: 0, total: 3 }, names: 2, last_at: null, branch_of: null,
    originals: [{ turn_id: 1, input: "들어온 말 " + id, response: "x".repeat(200) }], memory_preview: [{ id: "M7", kind: "fact", body: "기억 " + id, secret: true, inherited: false }] });
  let missing = false;
  const s = uiSim([["/chats", OK(CHATS)], ["/chats/attach", OK({ chats: [], turns: 0, memories: 0 })],
    ["/chats/compare", () => (missing ? { ok: false, status: 404, code: "chat_not_found" } : OK({ left: side("B", 4), right: side("A", 9) }))]], { id: "B", name: "", open: "A", isOpen: false });
  await s.ctx.chatsTab(s.body, () => {});
  assert.ok(!s.btn("지금 채팅과 비교", s.items()[1]), "the open chat is not compared with itself");
  const row = s.items()[0];
  await s.btn("지금 채팅과 비교", row).listeners.click(); await tick(3);
  assert.deepStrictEqual(s.calls.filter((c) => c.path.startsWith("/chats/compare")).map((c) => [c.path, c.method]),
    [["/chats/compare?left_chat_id=B&right_chat_id=A&left_limit=5&right_limit=5", "GET"]]);
  const t = uiText(row);
  assert.ok(t.includes("이 채팅 · 리나 / 두 번째") && t.includes("지금 채팅 · 리나 / 첫 채팅") && t.includes("턴 4 · 기억 3 (사건 1 · 사실 2 · 생각 0) · 이름 2") && t.includes("턴 9 ·"), t);
  assert.ok(t.includes("1턴 · 들어온 말 B → " + "x".repeat(120) + "…") && t.includes("M7 · 사실 · 비밀 · 기억 B"), t);
  assert.strictEqual(s.calls.filter((c) => c.method !== "GET").length, 0, "nothing is written");
  await s.btn("닫기", row).listeners.click();
  assert.ok(!uiText(row).includes("이 채팅 ·"), "closed");
  missing = true;
  await s.btn("지금 채팅과 비교", row).listeners.click(); await tick(3);
  assert.ok(uiText(row).includes("한쪽 채팅의 기록이 없어 비교할 수 없어요"), uiText(row));
  uiNoNull(s.body);
});

test("chats tab: a chat chosen while an attach waits stays chosen after it succeeds (review 11 R03)", async () => {
  let finish;
  const res = (b) => ({ chats: b.chats.map((id) => ({ chat_id: id, turns: 1, memories: 2 })), turns: b.chats.length, memories: 2 * b.chats.length });
  const s = uiSim([["/chats", OK(CHATS)], ["/chats/attach", (b, p) => (p.includes("?") ? OK({ chats: [], turns: 0, memories: 0 }) : b.dry_run ? OK(res(b)) : new Promise((r) => { finish = () => r(OK(res(b))); }))]],
    { id: "B", name: "", open: "A", isOpen: false });
  await s.ctx.chatsTab(s.body, () => {});
  const card = (text) => s.items().find((r) => uiText(r).includes(text) && s.btn("기억 보기", r));
  await s.btn("이어붙일 목록에 넣기", card("두 번째")).listeners.click();
  await s.btn("지금 채팅 앞에 이어붙이기").listeners.click();
  const run = s.btn("이어붙이기", s.confirm()).listeners.click(); await tick();
  await s.btn("이어붙일 목록에 넣기", card("채팅 Cxxxxxxx")).listeners.click();
  finish(); await run;
  assert.deepStrictEqual(s.posts("/chats/attach").at(-1), { chat_id: "A", chats: ["B"] });
  assert.ok(uiText(s.body).includes("1. 채팅 Cxxxxxxx") && !uiText(s.body).includes("2. ") && s.btn("지금 채팅 앞에 이어붙이기"), uiText(s.body));
  uiNoNull(s.body);
});

test("chats tab: a failed read of the open chat's past is said, and the list is still drawn (review 10 R06)", async () => {
  const s = uiSim([["/chats", OK(CHATS)], ["/chats/attach", { ok: false, status: 500, code: "store_failed" }]], { id: "B", name: "", open: "A", isOpen: false });
  await s.ctx.chatsTab(s.body, () => {});
  assert.strictEqual(s.items().length, 3, "the list is drawn");
  assert.ok(uiText(s.body).includes("앞부분 과거 연결을 읽지 못했어요"), uiText(s.body));
  uiNoNull(s.body);
});

test("chat actions: deleting a chat another chat reads as its past says the attachment goes", async () => {
  const preview = { deleted_turns: 4, deleted_memories: 6, branches: [], past_readers: ["A"], copied_turns: 0, copied_memories: 0 };
  const s = uiSim([["/chats", OK(CHATS)], ["/chats/attach", OK({ chats: [], turns: 0, memories: 0 })], ["/chats/delete", (b) => OK(preview)]], { id: "A", name: "", open: "A", isOpen: false });
  await s.ctx.chatsTab(s.body, () => {});
  await s.btn("삭제", s.items()[0]).listeners.click();
  assert.ok(uiText(s.confirm()).includes("앞부분 과거로 연결한 채팅 1개는 연결이 풀려요"), uiText(s.confirm()));
});

const MEMS = { total: 3, memories: [
  { id: "M1", turn: 3, kind: "event", body: "리나가 문을 열었다", about: "문", effects: [{ slot: "상태", before: "닫힘", value: "열림" }] },
  { id: "M2", turn: 2, kind: "view", body: "리나는 불안했다", owner: "리나", secret: true },
  { id: "M3", turn: 1, kind: "fact", body: "부모 채팅의 사실", inherited: true }] };

const GIVEN = { chat: "A", none: false, at: "2026-10-28T12:00:00Z", items: 2, chars: 40, chars_max: 100, secret_chars: 25, secret_max: 50, total_ms: 31, warning: "query_vector_off",
  block: "[기억]\n- 리나는 매일 밤 보고한다\n- 카일은 왕자다",
  memories: [
    { id: "M7", turn: 4, kind: "fact", body: "리나는 매일 밤 보고한다", about: "리나", mode: "의무", terms: "해가 진 뒤", secret: false, inherited: false },
    { id: "M2", turn: 1, kind: "view", body: "카일은 왕자다", owner: "리나", aspect: "belief", secret: true, inherited: true }],
  generation: { key: "generation", state: "done", count: 9, elapsed_ms: 12000, turn: 5, next_at: null } };

test("this turn: the last request's memories in order, budgets, the previous turn's generation and the block as given", async () => {
  const s = uiSim([["/injection", OK(GIVEN)]]);
  await s.ctx.turnTab(s.body);
  assert.deepStrictEqual(s.calls.map((c) => c.path), ["/injection?chat_id=A"], "the chat in view");
  const rows = s.items().map(uiText);
  assert.strictEqual(rows.length, 2);
  assert.ok(rows[0].includes("리나는 매일 밤 보고한다") && rows[0].includes("의무") && rows[1].includes("카일은 왕자다"), "delivery order, not story order");
  assert.ok(rows[1].includes("비밀") && rows[1].includes("물려받음") && rows[1].includes("믿음") && !rows[0].includes("비밀"));
  const t = uiText(s.body);
  assert.ok(t.includes("2개") && t.includes("40 / 100자") && t.includes("25 / 50자"), t);
  const meters = uiAll(s.body).filter((e) => e.className === "ac2-meter").map((e) => e["aria-label"]);
  assert.deepStrictEqual(meters, ["기억 예산 40%", "비밀 예산 50%"]);
  assert.ok(t.includes("임베딩 꺼짐 · 최근 순서"), "the warning");
  assert.ok(t.includes("직전 5턴 기억 만들기") && t.includes("9개 만듦") && t.includes("12.0초") && t.includes("기억 저장됨"), t);
  const pre = uiAll(s.body).find((e) => (" " + e.className + " ").includes(" ac2-pre "));
  assert.strictEqual(uiText(pre), GIVEN.block, "the block as it went");
  await s.btn("자세히", s.items()[0]).listeners.click();
  assert.ok(uiText(s.items()[0]).includes("조건 · 해가 진 뒤"));
  uiNoNull(s.body);
});

test("this turn (1.1 A1): where the last request's time went, step by step as the backend measured it", async () => {
  const stages = [{ name: "observe", ms: 3 }, { name: "session", ms: 0 }, { name: "embed", ms: 512 }, { name: "body", ms: 0 }, { name: "select", ms: 40 },
    { name: "preprocess", ms: 31738 }, { name: "narrative", ms: 11510 }, { name: "other", ms: 319 }];
  const s = uiSim([["/injection", OK({ ...GIVEN, total_ms: 44122, stages })]]);
  await s.ctx.turnTab(s.body);
  const t = uiText(s.body);
  assert.ok(t.includes("단계별 시간") && t.includes("입력 임베딩 0.5초 · 전처리 31.7초 · 출판사 11.5초 · 그 밖 0.3초"), t);
  assert.ok(!t.includes("기억 고르기 0.0초") && !t.includes("관찰 반영"), "steps under 0.1 s are left out");
  const old = uiSim([["/injection", OK(GIVEN)]]); // a trace from before 1.1 has no steps
  await old.ctx.turnTab(old.body);
  assert.ok(!uiText(old.body).includes("단계별"));
  uiNoNull(s.body);
});

test("this turn (1.1 A2): how much of the request equals the previous one from the start, as the backend compared", async () => {
  const s = uiSim([["/injection", OK({ ...GIVEN, prefix: { same: 0, total: 41, prev_total: 41, same_chars: 0, chars: 52310 } })]]);
  await s.ctx.turnTab(s.body);
  const t = uiText(s.body);
  assert.ok(t.includes("직전 요청과 같은 앞부분") && t.includes("메시지 0 / 41개, 0 / 52310자 (직전 요청 41개)"), t);
  const first = uiSim([["/injection", OK(GIVEN)]]); // nothing to compare since the backend started
  await first.ctx.turnTab(first.body);
  assert.ok(uiText(first.body).includes("비교 자료 없음"), uiText(first.body));
  uiNoNull(s.body);
});

test("this turn: the narrative guide sent and the input as written and as sent; the trace shows each line's decision answers", async () => {
  const narrative = { level: "mid", guide: "made", kept: 2, dropped: 0, input: "clarify", input_result: "changed", guide_text: "[서사 참고]\n- 약속", input_before: "묻는다", input_after: "리나에게 약속을 묻는다", ms: 900 };
  const rows = [{ id: "M1", turn: 1, kind: "fact", rank: 1, size: 20, result: "taken", unrelated: 0.12 }, { id: "M2", turn: 2, kind: "fact", rank: 2, size: 20, result: "not_chosen", unrelated: 0.91, ai_unrelated: true }];
  const s = uiSim([["/injection/trace", () => OK({ chat: "A", delivery: { id: 7, row_total: 2, trace: rows, next_cursor: null } })], ["/injection", OK({ ...GIVEN, delivery_id: 7, narrative })]]);
  await s.ctx.turnTab(s.body);
  const t = uiText(s.body);
  assert.ok(t.includes("서사 지원 0.9초") && t.includes("가이드 중간 · 2개 · 입력 다듬음"), t);
  const det = (label) => uiAll(s.body).find((e) => e.tag === "details" && uiText(e).includes(label));
  assert.ok(uiText(det("넣은 서사 가이드 보기")).includes("- 약속"));
  const io = uiText(det("입력 원문과 다듬은 글"));
  assert.ok(io.includes("쓴 글묻는다") && io.includes("보낸 글리나에게 약속을 묻는다"), io);
  const box = det("고른 과정 보기");
  box.open = true; box.listeners.toggle(); await tick(5);
  const tr = uiText(box);
  assert.ok(tr.includes("판정 관계없음 12%") && tr.includes("판정 관계없음 91%") && tr.includes("AI 관계없음") && tr.includes("전처리에서 고르지 않음"), tr);
  uiNoNull(s.body);
});

test("C18 the trace shows a pinned row as pinned, unranked, and one that did not fit as not fitting its budget (review 14)", async () => {
  const rows = [{ id: "M1", turn: 1, kind: "fact", rank: 0, size: 101, result: "over_ceiling", hold: "pin" },
    { id: "M3", turn: 2, kind: "event", rank: 0, size: 40, result: "taken", hold: "pin" },
    { id: "M5", turn: 2, kind: "fact", rank: 3, size: 50, result: "over_ceiling" }];
  const trace = () => OK({ chat: "A", delivery: { id: 7, row_total: rows.length, trace: rows, next_cursor: null } });
  const s = uiSim([["/injection/trace", trace], ["/injection", OK({ ...GIVEN, delivery_id: 7 })]]);
  await s.ctx.turnTab(s.body);
  const box = uiAll(s.body).find((e) => e.tag === "details" && uiText(e).includes("고른 과정 보기"));
  box.open = true; await box.listeners.toggle(); await tick(5);
  const items = uiAll(box).filter((e) => e.className === "ac2-item").map(uiText);
  assert.ok(items[0].includes("고정") && items[0].includes("예산에 들어가지 못함") && !items[0].includes("등수 합") && !items[0].includes("순위에서 밀림"), items[0]);
  assert.ok(items[1].includes("고정") && items[1].includes("넣음") && !items[1].includes("등수 합"), items[1]);
  assert.ok(items[2].includes("등수 합 3") && items[2].includes("순위에서 밀림") && !items[2].includes("고정"), items[2]);
  uiNoNull(s.body);
});

test("this turn: how Go chose is read only when opened, a page at a time, with each candidate's result as stored", async () => {
  const rows = [
    { id: "M7", turn: 4, kind: "fact", rank: 3, size: 21, result: "taken" },
    { id: "M2", turn: 1, kind: "view", rank: 5, size: 30, secret: true, result: "taken" },
    { id: "M9", turn: 2, kind: "event", rank: 8, size: 400, result: "over_ceiling" },
    { id: "M11", turn: 6, kind: "fact", rank: 9, result: "parent_not_taken" },
    { id: "M12", turn: 7, kind: "event", rank: 12, size: 50, result: "contrast_removed" }];
  const pages = [];
  const trace = (body, path) => {
    const q = new URL("http://x" + path).searchParams;
    pages.push([q.get("delivery_id"), q.get("row_cursor"), q.get("row_limit")]);
    const from = Number(q.get("row_cursor")), to = Math.min(rows.length, from + 3); // the backend may give fewer than asked
    return OK({ chat: "A", delivery: { id: 101, row_total: rows.length, trace: rows.slice(from, to), next_cursor: to < rows.length ? to : null } });
  };
  const s = uiSim([["/injection/trace", trace], ["/injection", OK({ ...GIVEN, delivery_id: 101 })]]);
  await s.ctx.turnTab(s.body);
  assert.deepStrictEqual(s.calls.map((c) => c.path), ["/injection?chat_id=A"], "nothing read before it is opened");
  const box = uiAll(s.body).find((e) => e.tag === "details" && uiText(e).includes("고른 과정 보기"));
  assert.ok(box);
  box.open = true; await box.listeners.toggle(); await tick(5);
  box.open = false; await box.listeners.toggle(); box.open = true; await box.listeners.toggle(); await tick(5);
  assert.deepStrictEqual(pages, [["101", "0", "50"]], "opened twice, read once");
  let t = uiText(box);
  assert.ok(t.includes("후보 5개 중 3개"), t);
  assert.ok(t.includes("M7") && t.includes("넣음") && t.includes("순위에서 밀림") && t.includes("비밀"), t);
  assert.ok(!t.includes("예산에 걸려"), "a candidate pushed out at the first fill is not shown as a budget failure: " + t);
  const pushedTag = uiAll(box).find((e) => e.className === "ac2-tag" && uiText(e) === "순위에서 밀림");
  assert.ok(pushedTag && !pushedTag["data-tone"], "no warning colour on a pushed-out candidate");
  assert.ok(t.includes("처음 예산을 채울 때 자리가 없던 기억") && !t.includes("다시 채우지 않아요"), "only what this page shows is explained: " + t);
  assert.ok(!t.includes("M11"), "the second page is not read yet");
  await s.btn("더 보기", box).listeners.click(); await tick(5);
  assert.deepStrictEqual(pages[1], ["101", "3", "50"], "the next page starts where the backend said");
  t = uiText(box);
  assert.ok(t.includes("후보 5개 중 5개") && t.includes("근거 사건이 안 들어가 빠짐") && t.includes("장면 대비 단계에서 빠짐"), t);
  assert.ok(t.includes("장면 대비로 뺀 자리는 다른 기억으로 다시 채우지 않아요") && t.includes("처음 예산을 채울 때 자리가 없던 기억"), "a removal seen on a later page is explained too: " + t);
  const m11 = uiAll(box).find((e) => e.className === "ac2-item" && uiText(e).includes("M11"));
  assert.ok(!/\d+자/.test(uiText(m11)), "no size is shown where Go did not compute one");
  assert.ok(!uiText(m11).includes("비밀"), "no secret mark is shown where Go did not compute one");
  assert.ok(!uiAll(box).some((e) => e.tag === "button" && uiText(e) === "더 보기"), "no more pages");
  uiNoNull(s.body);
});
test("this turn: a failed read of how Go chose says so and can be asked again from the same page", async () => {
  const rows = [{ id: "M7", turn: 4, kind: "fact", rank: 3, size: 21, result: "taken" }, { id: "M9", turn: 2, kind: "event", rank: 8, size: 400, result: "over_ceiling" }];
  const asked = [];
  let fail = true;
  const trace = (body, path) => {
    const q = new URL("http://x" + path).searchParams;
    asked.push(q.get("row_cursor"));
    if (fail) { fail = false; return { ok: false, status: 500, code: "store_failed" }; }
    const from = Number(q.get("row_cursor"));
    if (from === 1 && asked.length === 3) return { ok: false, status: 500, code: "store_failed" };
    return OK({ chat: "A", delivery: { id: 101, row_total: 2, trace: rows.slice(from, from + 1), next_cursor: from + 1 < 2 ? from + 1 : null } });
  };
  const s = uiSim([["/injection/trace", trace], ["/injection", OK({ ...GIVEN, delivery_id: 101 })]]);
  await s.ctx.turnTab(s.body);
  const box = uiAll(s.body).find((e) => e.tag === "details" && uiText(e).includes("고른 과정 보기"));
  box.open = true; await box.listeners.toggle(); await tick(5);
  assert.ok(uiText(box).includes("고른 과정 기록을 읽지 못했어요"), "a read failure, not a save failure: " + uiText(box));
  assert.ok(!uiText(box).includes("저장하지 못했어요"));
  await s.btn("다시 시도", box).listeners.click(); await tick(5);
  assert.ok(uiText(box).includes("M7") && uiText(box).includes("후보 2개 중 1개"));
  await s.btn("더 보기", box).listeners.click(); await tick(5);
  assert.ok(uiText(box).includes("M7") && uiText(box).includes("고른 과정 기록을 읽지 못했어요"), "rows already shown stay");
  await s.btn("다시 시도", box).listeners.click(); await tick(5);
  assert.deepStrictEqual(asked, ["0", "0", "1", "1"], "each retry asks the same page again");
  assert.ok(uiText(box).includes("M9") && uiText(box).includes("후보 2개 중 2개"));
  uiNoNull(s.body);
});
test("this turn: no request yet, a failed generation, and a request that found nothing", async () => {
  let reply = { chat: "A", none: true, generation: { key: "generation", state: "failed", count: 0, detail: "", turn: 3, next_at: "2026-10-28T12:05:00Z" } };
  const s = uiSim([["/injection", () => OK(JSON.parse(JSON.stringify(reply)))]]);
  await s.ctx.turnTab(s.body);
  let t = uiText(s.body);
  assert.ok(t.includes("아직 기억을 넣지 않았어요") && t.includes("기억 만들지 못함") && t.includes("다음 시도"), t);
  assert.strictEqual(s.items().length, 0);
  uiNoNull(s.body);
  reply = { ...GIVEN, items: 0, chars: 0, secret_chars: 0, block: "", memories: [], warning: "", generation: null };
  await s.btn("새로고침").listeners.click();
  t = uiText(s.body);
  assert.ok(t.includes("맞는 기억이 없어 넣지 않았어요") && !t.includes("넣은 원문 그대로 보기") && !t.includes("기억 만들기"), t);
  uiNoNull(s.body);
});

test("this turn: previous-turn query use comes from Go, including absent historical evidence", async () => {
  for (const [used, label] of [[true, "사용"], [false, "사용 안 함"], [undefined, "미관측"]]) {
    const reply = { ...GIVEN, recent_query_used: used, warning: used === false ? "recent_query_vector_failed" : "" };
    const s = uiSim([["/injection", OK(reply)]]);
    await s.ctx.turnTab(s.body);
    assert.ok(uiText(s.body).includes("직전 턴 질의" + label), uiText(s.body));
    if (used === false) assert.ok(uiText(s.body).includes("직전 턴 질의 실패 · 입력 유사도와 최근 순서"));
    assert.deepStrictEqual(s.calls.map((c) => c.path), ["/injection?chat_id=A"]);
    uiNoNull(s.body);
  }
});

test("this turn: a failed request shows the error and a way to ask again", async () => {
  const s = uiSim([["/injection", { ok: false, status: 0, kind: "connect_failed" }]]);
  await s.ctx.turnTab(s.body);
  assert.ok(uiText(s.body).includes("연결할 수 없어요") && s.btn("새로고침"), uiText(s.body));
  uiNoNull(s.body);
});

test("memories: a row shows its sub-kind and opens the stored details (FIX1 fields)", async () => {
  const list = { total: 3, memories: [
    { id: "F1", turn: 4, kind: "fact", body: "리나는 매일 밤 보고한다", about: "리나", mode: "의무", to: ["기사단장"], terms: "해가 진 뒤", status: "open", q: "매일 밤 보고하게", at: "E1", date: "Day 3", when: "새벽", when_from: "at" },
    { id: "V1", turn: 5, kind: "view", body: "리나는 그가 거짓말한다고 믿는다", owner: "리나", aspect: "belief", basis: { via: "추론", from: "하인" , confidence: "중간" }, affect: { intensity: "강함", persistence: "남음" }, differs: "unknown", date: "Day 4", when: "Day 4", when_from: "turn" },
    { id: "E1", turn: 6, kind: "event", body: "리나가 열쇠를 건넸다", outcome: "success", roles: { actor: ["리나"], target: ["카일"], object: ["열쇠"] }, time: "새벽" }] };
  const s = uiSim([["/memory/list", OK(list)]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  const rows = s.items();
  assert.ok(uiText(rows[0]).includes("의무") && uiText(rows[1]).includes("믿음"), "the fact mode and view aspect as tags");
  for (const i of [0, 1, 2]) await s.btn("자세히", s.items()[i]).listeners.click();
  const [f, v, e] = s.items().map(uiText);
  assert.ok(f.includes("근거 원문 · “매일 밤 보고하게”") && f.includes("대상 · 기사단장") && f.includes("조건 · 해가 진 뒤") && f.includes("상태 · open"), f);
  assert.ok(v.includes("아는 경로 · 추론 · 하인에게서 · 확신 중간") && v.includes("감정의 세기 · 강함 · 남음") && v.includes("사실과 같은지 몰라요"), v);
  assert.ok(e.includes("결과 · 성공") && e.includes("한 사람 · 리나") && e.includes("받은 사람 · 카일") && e.includes("물건 · 열쇠") && e.includes("때 · 새벽"), e);
  assert.ok(!e.includes("장소 ·") && !e.includes("범위 ·"), "fields it does not have stay out");
  // C12: when it happened and where that came from, and the recorded date
  assert.ok(f.includes("일어난 때 · 새벽 (근거 사건의 때)") && f.includes("근거 사건 · E1") && f.includes("기록된 날 · Day 3"), f);
  assert.ok(v.includes("일어난 때 · Day 4 (기록된 날로 채움)") && v.includes("기록된 날 · Day 4"), v);
  uiNoNull(s.body);
});

test("memories: a memory opens its own turn text and what it points to, read from Go only when asked, a page at a time", async () => {
  const list = { total: 2, memories: [
    { id: "M42", turn: 4, turn_id: 7, kind: "view", body: "리나는 카일을 의심한다", owner: "리나" },
    { id: "M43", turn: 5, kind: "fact", body: "옛 기록" },
    { id: "M44", turn: 6, turn_id: 9, kind: "event", body: "지워진 기록" }] };
  const asked = [];
  const detail = (b, path) => {
    const q = new URL("http://x" + path).searchParams;
    asked.push([q.get("turn_id"), q.get("id"), q.get("cursor")]);
    if (q.get("id") === "M44") return { ok: false, status: 404, code: "memory_not_found" };
    if (!q.get("cursor")) return OK({ id: "M42", turn_id: 7, turn: 4, inherited: false, labels: { N1: "카일" },
      memory: { id: "M42", kind: "view", body: "리나는 카일을 의심한다" },
      source: { members: [{ key: "u1", data: "카일, 어젯밤 어디 있었어?" }], responses: [{ key: "a1", data: "카일은 대답을 피했다." }] },
      related: [{ kind: "reference", field: "at", id: "M40", status: "available", memory: { id: "M40", body: "카일이 밤에 성을 나갔다" }, secret: true },
        { kind: "reference", field: "was.0", id: "M12", status: "deleted" }],
      next_cursor: "refs:2" });
    return OK({ id: "M42", turn_id: 7, turn: 4, labels: { N1: "카일" }, source: {}, related: [{ kind: "awareness", id: "9", status: "available", awareness: { who: "N1", kind: "unaware" } }], next_cursor: null });
  };
  const s = uiSim([["/memory/list", OK(list)], ["/memory/detail", detail]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  assert.ok(!s.calls.some((c) => c.path.startsWith("/memory/detail")), "nothing read before it is asked");
  const [first, second, third] = s.items();
  assert.ok(!uiAll(second).some((e) => e.tag === "button" && uiText(e) === "원문·근거"), "a memory without a turn locator offers no button");
  await s.btn("원문·근거", first).listeners.click(); await tick(5);
  let t = uiText(s.items()[0]);
  assert.ok(t.includes("4턴에서 만든 기억") && t.includes("카일, 어젯밤 어디 있었어?") && t.includes("카일은 대답을 피했다."), "the turn's own text: " + t);
  assert.ok(t.includes("근거 사건 · M40") && t.includes("카일이 밤에 성을 나갔다") && t.includes("비밀"), t);
  assert.ok(t.includes("이전 값 · M12") && t.includes("지워짐"), t);
  assert.ok(!t.includes("카일 모름"), "the second page is not read yet");
  await s.btn("더 보기", s.items()[0]).listeners.click(); await tick(5);
  t = uiText(s.items()[0]);
  assert.ok(t.includes("누가 아는가") && t.includes("카일 모름"), "names come from Go's labels: " + t);
  assert.deepStrictEqual(asked, [["7", "M42", null], ["7", "M42", "refs:2"]]);
  await s.btn("원문·근거", s.items().find((e) => uiText(e).includes("지워진 기록"))).listeners.click(); await tick(5);
  t = uiText(s.items().find((e) => uiText(e).includes("지워진 기록")));
  assert.ok(t.includes("이 기억을 찾지 못했어요") && t.includes("다시 시도"), t);
  uiNoNull(s.body);
});
test("memories: an inherited memory's place is told apart from where it was made; allow-to and a link's out-of-scope end are named", async () => {
  const list = { total: 1, memories: [{ id: "M50", turn: 1, turn_id: 3, kind: "fact", body: "물려받은 사실", inherited: true }] };
  const detail = () => OK({ id: "M50", turn_id: 3, turn: 1, source_turn: 7, source_session: "chat:P", inherited: true, labels: { N2: "도윤" },
    source: { members: [{ key: "u", data: "부모 채팅 입력" }], responses: [] },
    related: [{ kind: "awareness", id: "4", status: "available", awareness: { who: "N2", kind: "allow_to" } },
      { kind: "link", id: "L9", status: "available", link: { from: "M50", rel: "causes", to: "M77" }, endpoints: [{ field: "from", id: "M50", status: "available" }, { field: "to", id: "M77", status: "out_of_scope" }] }],
    next_cursor: null });
  const s = uiSim([["/memory/list", OK(list)], ["/memory/detail", detail]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  await s.btn("원문·근거", s.items()[0]).listeners.click(); await tick(5);
  const t = uiText(s.items()[0]);
  assert.ok(t.includes("이 채팅 1턴 자리") && t.includes("부모 채팅의 기록에서 물려받음") && !t.includes("1턴에서 만든 기억"), "not told as made in this chat's turn: " + t);
  assert.ok(t.includes("도윤 알려도 됨") && !t.includes("allow_to"), t);
  assert.ok(t.includes("원인 · M50 → M77 (M77 이 채팅 범위 밖)"), t);
  uiNoNull(s.body);
});
test("memories: kind tabs with counts; one kind is asked of Go and its groups are headed (facts by mode, subjective memories by whose)", async () => {
  const kinds = { event: 1, fact: 2, view: 2, person: 4 };
  const pages = {
    "": [{ id: "E1", turn: 2, kind: "event", body: "리나가 왔다" }, { id: "F1", turn: 2, kind: "fact", body: "리나는 지쳤다", mode: "상태" }, { id: "F2", turn: 1, kind: "fact", body: "리나는 약사다", mode: "상태" }],
    fact: [{ id: "F1", turn: 2, kind: "fact", body: "리나는 지쳤다", mode: "상태" }, { id: "F2", turn: 1, kind: "fact", body: "리나는 약사다", mode: "상태" }],
    view: [{ id: "V1", turn: 1, kind: "view", body: "도윤은 리나를 믿는다", owner: "도윤", aspect: "belief" }, { id: "V2", turn: 1, kind: "view", body: "리나는 고맙다", owner: "리나", aspect: "emotion" }] };
  const s = uiSim([["/memory/list", (b, path) => { const k = (path.match(/[?&]kind=(\w+)/) || [])[1] || ""; return OK({ total: pages[k].length, kinds, memories: pages[k] }); }]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  const tabs = () => uiAll(s.body).filter((e) => e.className === "ac2-kind");
  const heads = () => uiAll(s.body).filter((e) => e.className === "ac2-group").map(uiText);
  assert.deepStrictEqual(tabs().map(uiText), ["전체5", "사건1", "사실2", "주관 기억2", "이름별4"], "전체 counts the three kinds, not the by-name view");
  assert.deepStrictEqual(heads(), ["2턴", "1턴"], "all of them headed by turn");
  assert.ok(!s.calls[0].path.includes("kind="), s.calls[0].path);
  await tabs()[3].listeners.click();
  assert.ok(s.calls.at(-1).path.includes("&kind=view"), s.calls.at(-1).path);
  assert.deepStrictEqual(heads(), ["도윤의 기억", "리나의 기억"]);
  assert.strictEqual(s.items().length, 2);
  assert.ok(uiText(s.items()[0]).includes("주관 기억") && uiText(s.items()[0]).includes("믿음"));
  assert.strictEqual(tabs()[3].getAttribute("aria-pressed"), "true");
  await tabs()[2].listeners.click();
  assert.deepStrictEqual(heads(), ["상태"], "one heading for a group, not one per row");
  uiNoNull(s.body);
});

test("memories: a fact whose slots were changed by later records is tagged '일부 바뀜' with the slots; one without is not", async () => {
  const kinds = { event: 0, fact: 2, view: 0, person: 0 };
  const s = uiSim([["/memory/list", () => OK({ total: 2, kinds, replaced: 0, memories: [
    { id: "F1", turn: 1, kind: "fact", body: "리나는 상인이다", mode: "상태", changed: ["소지금", "위치"] },
    { id: "F2", turn: 1, kind: "fact", body: "마을은 북쪽에 있다", mode: "세계 규칙", changed: [] }] })]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  assert.ok(uiText(s.items()[0]).includes("일부 바뀜: 소지금, 위치"), uiText(s.items()[0]));
  assert.ok(!uiText(s.items()[1]).includes("일부 바뀜"));
});

test("memories: C14 이름별 is asked of Go, one name tab at a time with its type and folded parts, its changed values first (user 11-09); replaced values are counted and marked", async () => {
  const kinds = { event: 1, fact: 3, view: 1, person: 3 };
  const F1 = { id: "F1", turn: 2, kind: "fact", body: "리나는 약국을 그만뒀다", mode: "상태", about: "리나", person: "리나" };
  const V1 = { id: "V1", turn: 1, kind: "view", body: "리나는 떠나려 한다", owner: "리나", about: "도윤", aspect: "goal", person: "리나" };
  const W1 = { id: "W1", turn: 1, kind: "fact", body: "마을에서는 밤에 문을 닫는다", mode: "세계 규칙" };
  const people = [{ name: "리나", ref: "N1", type: "person", count: 2, changes: 2 }, { name: "성검", ref: "N7", type: "thing", count: 0, changes: 1 }, { name: "", ref: "", type: "", count: 1, changes: 0 }];
  const changes = {
    N1: [{ turn: 1, id: "F9", slot: "상태", value: "리나는 약사다", replaced: true }, { turn: 2, id: "E2", slot: "소지품", before: "없음", value: "성검", event: "리나가 성검을 뽑았다" }],
    N7: [{ turn: 2, id: "E3", slot: "상태", before: "빛남", value: "금 감", delta: "", event: "성검이 마왕의 검과 부딪쳤다" }], "": [] };
  const s = uiSim([["/memory/list", (b, path) => {
    const q = new URLSearchParams(path.split("?")[1]);
    if (q.get("kind") !== "person") return OK({ total: 1, kinds, replaced: 0, memories: [{ id: "F9", turn: 1, kind: "fact", body: "리나는 약사다", mode: "상태", replaced: true }] });
    if (!q.has("person")) return OK({ total: 3, kinds, replaced: 1, people, memories: [F1, V1, W1] });
    const who = q.get("person");
    const mine = who === "N1" ? [F1, V1] : who === "" ? [W1] : [];
    const parts = who === "N1" ? [{ name: "상태", count: 1 }, { name: "주관 기억", count: 1 }] : who === "" ? [{ name: "세계 규칙", count: 1 }] : [];
    return OK({ total: mine.length, kinds, replaced: 1, people, parts, changes: changes[who], memories: mine });
  }]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  const tabs = () => uiAll(s.body).filter((e) => e.className === "ac2-kind");
  const heads = () => uiAll(s.body).filter((e) => e.className === "ac2-group").map(uiText);
  const folds = () => uiAll(s.body).filter((e) => e.className === "ac2-fold");
  assert.ok(uiText(s.items()[0]).includes("바뀐 값"), "a replaced value is marked where it is listed");
  await tabs()[4].listeners.click();
  await tick(5);
  const last = new URLSearchParams(s.calls.at(-1).path.split("?")[1]);
  assert.strictEqual(last.get("kind"), "person");
  assert.strictEqual(last.get("person"), "N1", "the first name Go lists is shown first, asked by its reference: " + s.calls.at(-1).path);
  assert.deepStrictEqual(tabs().slice(5).map(uiText), ["리나 인물2", "성검 물건0", "특정 이름 없음(세계 규칙 등)1"], "a tab per name with its type, with Go's counts");
  assert.strictEqual(tabs()[5].getAttribute("aria-pressed"), "true");
  assert.strictEqual(tabs()[6].getAttribute("title"), "바뀐 값 1개");
  assert.strictEqual(tabs()[7].getAttribute("title"), null, "no changes, no title");
  assert.deepStrictEqual(heads(), ["바뀐 값 2개", "상태 1개", "주관 기억 1개"], "its changed values first, then one name's parts, counted by Go");
  assert.strictEqual(folds().length, 3);
  assert.ok(folds().every((f) => !f.open), "parts start folded");
  assert.strictEqual(s.items().length, 2, "only that person's memories");
  assert.ok(uiText(folds()[2]).includes("리나는 떠나려 한다"), "a view is inside its part's box");
  const rowsOf = (f) => uiAll(f).filter((e) => e.className === "ac2-item ac2-change").map(uiText);
  assert.deepStrictEqual(rowsOf(folds()[0]), ["1턴 · 상태리나는 약사다 (나중 기록으로 바뀐 옛 값)", "2턴 · 소지품없음 → 성검리나가 성검을 뽑았다"], "oldest first: the replaced value, then before → value with its event");
  assert.ok(uiText(s.body).includes("새 값으로 바뀐 옛 기억 1개는 각 이름의 \"바뀐 값\"에서 볼 수 있어요"), uiText(s.body));
  assert.ok(!uiText(s.items()[0]).includes("바뀐 값"));
  await tabs()[6].listeners.click();
  await tick(5);
  assert.strictEqual(new URLSearchParams(s.calls.at(-1).path.split("?")[1]).get("person"), "N7");
  assert.deepStrictEqual(heads(), ["바뀐 값 1개"], "a thing with changes only: its changes, no empty-list note");
  assert.deepStrictEqual(rowsOf(folds()[0]), ["2턴 · 상태빛남 → 금 감성검이 마왕의 검과 부딪쳤다"]);
  assert.ok(!uiText(s.body).includes("아직 없어요"), uiText(s.body));
  await tabs()[7].listeners.click();
  await tick(5);
  assert.strictEqual(new URLSearchParams(s.calls.at(-1).path.split("?")[1]).get("person"), "", "what is about nobody has its own tab");
  assert.deepStrictEqual(heads(), ["세계 규칙 1개"]);
  assert.strictEqual(s.items().length, 1);
  await tabs()[0].listeners.click(); // the kind is kept across screens: leave 전체
  uiNoNull(s.body);
});

test("memories: a row edits and deletes in place; several are chosen and deleted at once, inherited ones cannot be", async () => {
  const s = uiSim([["/memory/list", OK(MEMS)], ["/memory/delete", OK({ status: "ok" })], ["/memory/edit", OK({ status: "ok" })]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  const rows = s.items();
  assert.ok(uiText(rows[0]).includes("사건") && uiText(rows[1]).includes("비밀") && uiText(rows[2]).includes("물려받음"));
  assert.ok(!s.btn("삭제", rows[2]) && !s.btn("수정", rows[2]), "an inherited memory is read only");
  await s.btn("자세히", rows[0]).listeners.click();
  assert.ok(uiText(s.items()[0]).includes("상태 닫힘 → 열림"));
  await s.btn("삭제", s.items()[0]).listeners.click();
  assert.ok(uiText(s.confirm()).includes("이 기억을 지울까요"));
  await s.btn("삭제", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/memory/delete"), [{ chat_id: "A", id: "M1" }]);
  assert.strictEqual(s.items().length, 2, "the deleted row is gone");
  await s.btn("여러 개 고르기").listeners.click();
  await tick(5);
  const boxes = s.items().flatMap((row) => uiAll(row).filter((e) => e.tag === "input" && e.type === "checkbox"));
  assert.strictEqual(boxes.length, MEMS.memories.length, "one row checkbox per memory, apart from group selection");
  assert.ok(boxes[2].disabled, "an inherited memory cannot be chosen");
  assert.ok(s.btn("고른 기억 0개 지우기").disabled);
  boxes[0].checked = true; boxes[0].listeners.change(); boxes[1].checked = true; boxes[1].listeners.change();
  await s.btn("고른 기억 2개 지우기").listeners.click();
  assert.ok(uiText(s.confirm()).includes("고른 기억 2개를 지울까요"));
  await s.btn("삭제", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/memory/delete").at(-1), { chat_id: "A", ids: ["M1", "M2"] });
  uiNoNull(s.body);
});

test("memories: C18 a card pins or suppresses its memory through Go and shows the flag; an inherited memory has neither; a failed write keeps the flag", async () => {
  const list = { total: 3, memories: [{ id: "M1", turn: 3, kind: "fact", body: "리나는 약사다", mode: "상태" },
    { id: "M2", turn: 2, kind: "fact", body: "리나는 겁이 많다", mode: "성향", hold: "mute" },
    { id: "M3", turn: 1, kind: "fact", body: "부모 채팅의 사실", inherited: true, hold: "pin" }] };
  let fail = false;
  const s = uiSim([["/memory/list", OK(list)], ["/memory/hold", (b) => fail ? { ok: false, status: 500, code: "store_failed" } : OK({ status: "ok", hold: b.hold, changed: true })]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  const rows = () => s.items();
  assert.ok(uiText(rows()[1]).includes("억제") && s.btn("억제 풀기", rows()[1]) && s.btn("고정", rows()[1]));
  assert.ok(uiText(rows()[2]).includes("고정") && !s.btn("고정 풀기", rows()[2]) && !s.btn("고정", rows()[2]) && !s.btn("억제", rows()[2]), "an inherited memory shows its flag, read only");
  await s.btn("고정", rows()[0]).listeners.click();
  assert.deepStrictEqual(s.posts("/memory/hold"), [{ chat_id: "A", id: "M1", hold: "pin" }]);
  assert.ok(uiText(rows()[0]).includes("고정 풀기") && uiText(rows()[0]).includes("다시 만들면 고정이 풀려요"), uiText(rows()[0]));
  await s.btn("고정 풀기", rows()[0]).listeners.click();
  assert.deepStrictEqual(s.posts("/memory/hold").at(-1), { chat_id: "A", id: "M1", hold: "" });
  fail = true;
  await s.btn("억제 풀기", rows()[1]).listeners.click();
  assert.deepStrictEqual(s.posts("/memory/hold").at(-1), { chat_id: "A", id: "M2", hold: "" });
  assert.ok(s.btn("억제 풀기", rows()[1]), "a failed write keeps the shown flag");
  fail = false;
  await s.btn("억제 풀기", rows()[1]).listeners.click();
  assert.deepStrictEqual(s.posts("/memory/hold").at(-1), { chat_id: "A", id: "M2", hold: "" }, "the kept flag is still the one released");
  assert.ok(s.btn("억제", rows()[1]) && !uiText(rows()[1]).includes("억제 풀기"));
  uiNoNull(s.body);
});

test("memories: stored turns are read only; remaking a turn asks first and names the cost", async () => {
  const turns = { total: 2, turns: [{ turn: 2, user: "들어가자", reply: "문이 열렸다", memories: 3, state: "made" }, { turn: 1, user: "안녕", reply: "반가워", memories: 0, state: "failed", inherited: true }] };
  const s = uiSim([["/chats/turns", OK(turns)], ["/memory/turn/remake", OK({ status: "ok" })], ["/memory/turn/clear", OK({ status: "ok" })]]);
  s.ctx.setSub("turns");
  await s.ctx.memoriesTab(s.body);
  await tick(5);
  const rows = s.items();
  assert.ok(uiText(rows[0]).includes("만듦") && uiText(rows[1]).includes("실패") && uiText(rows[1]).includes("부모 채팅에서 읽음"));
  assert.ok(!uiAll(s.body).some((e) => e.tag === "textarea" || (e.tag === "input" && e.type !== "number")), "the texts cannot be edited");
  assert.ok(!s.btn("이 턴 다시 만들기", rows[1]), "a turn read from the parent has no actions");
  await s.btn("이 턴 다시 만들기", rows[0]).listeners.click();
  assert.ok(uiText(s.confirm()).includes("비용이 들어요"));
  assert.strictEqual(s.posts("/memory/turn/remake").length, 0, "nothing before the confirmation");
  await s.btn("다시 만들기", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/memory/turn/remake"), [{ chat_id: "A", turn: 2 }]);
  assert.ok(!s.btn("이 턴 기억 지우기", s.items()[0]), "the current UI has one remake action after deletion also triggers regeneration");
  assert.strictEqual(s.posts("/memory/turn/clear").length, 0);
  uiNoNull(s.body);
});

test("memories: the turns whose memory could not be made are made again after the backend's preview (C30)", async () => {
  const turns = { total: 1, turns: [{ turn: 1, user: "안녕", reply: "반가워", memories: 0, state: "failed" }] };
  let failed = [3, 7];
  const s = uiSim([["/chats/turns", OK(turns)], ["/memory/turn/remake-failed", (b) => { const r = OK({ turns: failed, dry_run: !!b.dry_run }); if (!b.dry_run) failed = []; return r; }]]);
  s.ctx.setSub("turns");
  await s.ctx.memoriesTab(s.body);
  await tick(5);
  await s.btn("실패한 턴 다시 만들기").listeners.click();
  assert.deepStrictEqual(s.posts("/memory/turn/remake-failed"), [{ chat_id: "A", dry_run: true }]);
  const ask = uiText(s.confirm());
  assert.ok(ask.includes("2개 턴(3, 7턴)") && ask.includes("비용이 들어요"), ask);
  await s.btn("다시 만들기", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/memory/turn/remake-failed").at(-1), { chat_id: "A" });
  assert.ok(uiText(s.body).includes("2개 턴을 다시 만들기 시작했어요"), uiText(s.body));
  await s.btn("실패한 턴 다시 만들기").listeners.click();
  assert.ok(uiText(s.body).includes("기억을 만들지 못한 턴이 없어요") && !s.confirm(), "nothing to ask when none failed");
  assert.strictEqual(s.posts("/memory/turn/remake-failed").filter((b) => !b.dry_run).length, 1);
  uiNoNull(s.body);
});

test("memories: the body screen shows the switches, each person's state and facts, and sends records and settings (1.0 E09/E10)", async () => {
  const spec = { cycle_days: 28, variation_days: 2, period_days: 5, luteal_min_days: 10, luteal_max_days: 16, viability: 0.4, peak: 0.7, gestation_days: 266, can_conceive: true, cycle_enabled: true };
  let view = { cycle_tracking_enabled: false, automatic_pregnancy_enabled: false, wait_for_latest_reply: false, budget: 3000, tendency_budget: 3000, people: [] };
  const intensity = { cramps: 1, premenstrual: 2, nausea: 0, pregnancy_discomfort: 3, recovery_speed: 3 };
  const person = { who: "N1", name: "리나", spec, intensity, state: { lines: ["- body 리나 [calculated, private, not known to anyone]: on Day 5 cycle day 6",
    "[Archive Center: body tendencies — modeled]", "- body tendency 리나 [felt by her]: menstrual (cycle day 6) — mild cramps, fatigue"] },
    facts: [{ id: 1, kind: "period_start", day: "Day 0", user: false }, { id: 2, kind: "conception_exposure", day: "Day 3", scope: "flashback", user: false, partners: ["카일"], result: { status: "evaluated", implanted: false } },
      { id: 6, kind: "conception_exposure", day: "Day 5", user: false, partners: ["카일"], result: { status: "unknown", reason: "contraception_context_not_modeled", implanted: false } },
      { id: 7, kind: "conception_exposure", day: "Day 6", user: false, result: { status: "not_applicable", reason: "modeled_pregnancy_ongoing", implanted: false } },
      { id: 8, kind: "pregnancy_ended", day: "Day 7", user: false, outcome: "birth" },
      { id: 13, kind: "pregnancy_ended", day: "Day 300", user: false, outcome: "birth", child: "하늘", paternity: "confirmed", fathers: ["카일"], unaware: ["리나"] },
      { id: 14, kind: "child_named", day: "Day 303", user: false, child: "하늘" },
      { id: 10, kind: "conception_exposure", day: "Day 8", user: false, result: { status: "evaluated", implanted: false, contraception: "condom" } },
      { id: 11, kind: "emergency_contraception", day: "Day 9", user: false },
      { id: 12, kind: "conception_exposure", day: "Day 10", user: false, result: { status: "evaluated", implanted: false, contraception: "magic" } },
      { id: 3, kind: "recovery_started", day: "Day 4", user: true }] };
  const s = uiSim([["/body", () => OK(view)], ["/body/settings", (b) => { view = { ...view, ...b }; return OK(b); }], ["/body/record", OK({ id: 9 })],
    ["/body/record/delete", OK({ status: "ok" })], ["/body/person", OK({ status: "ok" })]]);
  s.ctx.setSub("body");
  await s.ctx.memoriesTab(s.body); await tick(5);
  assert.ok(uiText(s.body).includes("꺼져 있어요"), uiText(s.body));
  const sw = uiAll(s.body).find((e) => e.id === "ac2-body-cycle");
  await sw.listeners.click(); await tick(5);
  assert.deepStrictEqual(s.posts("/body/settings"), [{ cycle_tracking_enabled: true, automatic_pregnancy_enabled: false, wait_for_latest_reply: false }]);
  assert.ok(uiText(s.body).includes("모든 채팅에 적용돼요"));
  // the wait for the latest reply goes with the other switches kept
  await uiAll(s.body).find((e) => e.id === "ac2-body-wait").listeners.click(); await tick(5);
  assert.deepStrictEqual(s.posts("/body/settings")[1], { cycle_tracking_enabled: true, automatic_pregnancy_enabled: false, wait_for_latest_reply: true });
  await uiAll(s.body).find((e) => e.id === "ac2-body-cycle").listeners.click(); await tick(5);
  assert.deepStrictEqual(s.posts("/body/settings")[2], { cycle_tracking_enabled: false, automatic_pregnancy_enabled: false, wait_for_latest_reply: true }, "another switch keeps the wait");
  await uiAll(s.body).find((e) => e.id === "ac2-body-cycle").listeners.click(); await tick(5);
  view = { ...view, people: [person] };
  await s.btn("새로고침").listeners.click(); await tick(5);
  const t = uiText(s.body);
  assert.ok(t.includes("리나") && t.includes("[calculated, private, not known to anyone]: on Day 5 cycle day 6") && !t.includes("- body 리나"), t);
  // the tendency line shows, its section header does not (1.1)
  assert.ok(t.includes("[felt by her]: menstrual (cycle day 6) — mild cramps, fatigue") && !t.includes("[Archive Center: body tendencies") && t.includes("몸 변화 경향 몫 3000자"), t);
  assert.ok(t.includes("Day 6 · 임신 가능한 관계 · 모형: 계산 안 함(모형 임신 중)"), t);
  assert.ok(t.includes("Day 7 · 임신 끝남 · 출산"), t);
  // a birth's child and father, and whose secret the father is (1.1)
  assert.ok(t.includes("Day 300 · 임신 끝남 · 출산 · 아이 하늘 · 아버지 카일 · 비밀: 리나 모름"), t);
  assert.ok(t.includes("Day 303 · 아이 이름 · 아이 하늘"), t);
  // the method an exposure was drawn with, and emergency contraception
  assert.ok(t.includes("Day 8 · 임신 가능한 관계 · 모형: 착상 없음 (피임: 콘돔)") && t.includes("Day 9 · 사후 피임") && t.includes("Day 10 · 임신 가능한 관계 · 모형: 착상 없음 (피임: 마법)"), t);
  assert.ok(uiAll(s.body).some((e) => e.tag === "option" && e.value === "emergency_contraception" && uiText(e) === "사후 피임"), "emergency contraception can be recorded");
  assert.strictEqual(uiAll(s.body).find((e) => e["aria-label"] === "회복 속도").value, "3");
  assert.ok(t.includes("Day 0 · 월경 시작") && t.includes("Day 3 · 임신 가능한 관계 · 회상 · 상대 카일 · 모형: 착상 없음") && t.includes("Day 5 · 임신 가능한 관계 · 상대 카일 · 모형: 계산 안 함(피임 언급)") && !t.includes("Day 5 · 임신 가능한 관계 · 상대 카일 · 모형: 착상 없음") && t.includes("Day 4 · 회복 시작 · 직접 기록"), t);
  assert.strictEqual(uiAll(s.body).filter((e) => e.tag === "button" && uiText(e) === "지우기").length, 1, "only a fact recorded here is removed here");
  // record a fact
  const day = uiAll(s.body).find((e) => e["aria-label"] === "작중 날");
  day.value = " Day 7 ";
  await s.btn("기록").listeners.click(); await tick(5);
  assert.deepStrictEqual(s.posts("/body/record"), [{ chat_id: "A", who: "N1", kind: "period_start", day: "Day 7" }]);
  // remove a recorded fact after asking
  await s.btn("지우기").listeners.click();
  assert.ok(uiText(s.confirm()).includes("회복 시작 기록을 지울까요"));
  assert.strictEqual(s.posts("/body/record/delete").length, 0);
  await s.btn("지우기", s.confirm()).listeners.click(); await tick(5);
  assert.deepStrictEqual(s.posts("/body/record/delete"), [{ chat_id: "A", id: 3 }]);
  // settings go as numbers
  const cycle = uiAll(s.body).find((e) => e["aria-label"] === "주기(일)");
  cycle.value = "30";
  // the intensity drawn in Go shows and goes back as edited (1.1)
  const cramps = uiAll(s.body).find((e) => e["aria-label"] === "생리통");
  assert.strictEqual(cramps.value, "1");
  cramps.value = "3";
  await cramps.listeners.change();
  // a choice not saved yet survives a redraw
  cycle.listeners.input();
  await s.btn("새로고침").listeners.click(); await tick(5);
  assert.strictEqual(uiAll(s.body).find((e) => e["aria-label"] === "생리통").value, "3");
  await s.btn("설정 저장").listeners.click(); await tick(5);
  assert.deepStrictEqual(s.posts("/body/person"), [{ chat_id: "A", who: "N1", spec: { ...spec, cycle_days: 30 }, intensity: { ...intensity, cramps: 3 } }]);
  uiNoNull(s.body);
});

test("memories: body switches write one at a time, typing during a save is kept, and a screen drawn again meanwhile shows the result (review 12 R03 R09 R13)", async () => {
  const spec = { cycle_days: 28, variation_days: 2, period_days: 5, luteal_min_days: 10, luteal_max_days: 16, viability: 0.4, peak: 0.7, gestation_days: 266, can_conceive: true, cycle_enabled: true };
  const fresh = () => ({ cycle_tracking_enabled: false, automatic_pregnancy_enabled: false, wait_for_latest_reply: false, budget: 3000, tendency_budget: 3000, people: [] });
  // R03: a second switch while the first is saving is not sent
  let view = fresh(); const pending = [];
  let s = uiSim([["/body", () => OK(JSON.parse(JSON.stringify(view)))], ["/body/settings", (b) => new Promise((r) => pending.push(() => { view = { ...view, ...b }; r(OK(b)); }))]]);
  s.ctx.setSub("body"); await s.ctx.memoriesTab(s.body); await tick();
  const sw = (id) => uiAll(s.body).find((e) => e.id === id);
  const first = sw("ac2-body-cycle").listeners.click();
  await sw("ac2-body-preg").listeners.click();
  assert.strictEqual(pending.length, 1, "one write at a time");
  assert.strictEqual(sw("ac2-body-preg").getAttribute("aria-checked"), "false", "the refused switch goes back");
  pending[0](); await first; await tick(5);
  assert.deepStrictEqual(view, { ...fresh(), cycle_tracking_enabled: true });
  assert.strictEqual(sw("ac2-body-cycle").getAttribute("aria-checked"), "true");
  // R09: a value typed while the save runs stays as typed
  view = { ...fresh(), cycle_tracking_enabled: true, people: [{ who: "N1", name: "리나", spec: { ...spec }, state: { lines: [] }, facts: [] }] };
  let finish;
  s = uiSim([["/body", () => OK(JSON.parse(JSON.stringify(view)))], ["/body/person", (b) => new Promise((r) => { finish = () => { view.people[0].spec = b.spec; r(OK({ status: "ok" })); }; })]]);
  s.ctx.setSub("body"); await s.ctx.memoriesTab(s.body); await tick();
  const input = () => uiAll(s.body).find((e) => e["aria-label"] === "주기(일)");
  const type = (v) => { const i = input(); i.value = v; i.listeners.input(); };
  type("35");
  await s.btn("설정 저장").listeners.click(); await tick();
  assert.strictEqual(s.posts("/body/person")[0].spec.cycle_days, 35);
  type("36"); finish(); await tick(10);
  assert.strictEqual(input().value, "36", "typed after the save was sent");
  // R13: a screen drawn again during the write shows the stored value after it
  view = fresh(); let end;
  s = uiSim([["/body", () => OK(JSON.parse(JSON.stringify(view)))], ["/body/settings", (b) => new Promise((r) => { end = () => { view = { ...view, ...b }; r(OK(b)); }; })]]);
  s.ctx.setSub("body"); await s.ctx.memoriesTab(s.body); await tick();
  const saving = uiAll(s.body).find((e) => e.id === "ac2-body-cycle").listeners.click();
  s.body.replaceChildren(); await s.ctx.memoriesTab(s.body); await tick();
  end(); await saving; await tick(10);
  assert.strictEqual(uiAll(s.body).find((e) => e.id === "ac2-body-cycle").getAttribute("aria-checked"), "true");
  uiNoNull(s.body);
});

test("memories: a name is renamed or merged into another; merging without a choice keeps the confirmation", async () => {
  const names = { names: [{ id: "N1", name: "리나", type: "person", memories: 4, aliases: ["리나쨩"] }, { id: "N2", name: "Rina", type: "person", memories: 1, aliases: [] }, { id: "P1", name: "부모 이름", type: "place", memories: 0, aliases: [], inherited: true }] };
  const s = uiSim([["/names", OK(names)], ["/names/edit", OK({ status: "ok" })], ["/names/merge", OK({ status: "ok" })]]);
  s.ctx.setSub("names");
  await s.ctx.memoriesTab(s.body);
  await tick(5);
  assert.ok(uiText(s.items()[0]).includes("다른 이름 리나쨩") && uiText(s.items()[2]).includes("장소"));
  assert.ok(!s.btn("이름 고치기", s.items()[2]));
  await s.btn("이름 고치기", s.items()[0]).listeners.click();
  uiAll(s.confirm()).find((e) => e.tag === "input").value = "리나 (고침)";
  await s.btn("이름 저장", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/names/edit"), [{ chat_id: "A", id: "N1", name: "리나 (고침)" }]);
  await tick(5);
  await s.btn("같은 것과 합치기", s.items()[1]).listeners.click();
  const pick = uiAll(s.confirm()).find((e) => e.tag === "select");
  assert.deepStrictEqual(pick.children.map((o) => o.value), ["", "N1"], "only another own name can be kept");
  await s.btn("합치기", s.confirm()).listeners.click();
  assert.ok(uiText(s.confirm()).includes("남길 이름을 골라 주세요"));
  assert.strictEqual(s.posts("/names/merge").length, 0);
  pick.value = "N1";
  await s.btn("합치기", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/names/merge"), [{ chat_id: "A", keep: "N1", merge: "N2" }]);
  // the backend runs a merge between worker runs, so it may wait past 8 s
  assert.deepStrictEqual(s.calls.filter((c) => c.path === "/names/merge").map((c) => c.timeoutMs), [0]);
  uiNoNull(s.body);
});

test("names: picking the name to keep reads what the merge would change before it is confirmed; a failed count shows ?", async () => {
  const names = { names: [{ id: "N1", name: "리나", type: "person", memories: 4, aliases: [] }, { id: "N2", name: "Rina", type: "thing", memories: 1, aliases: [] }] };
  const v = (x) => ({ count: x, status: "available" });
  const preview = { keep: { id: "N1", type: "person" }, merge: { id: "N2", type: "thing" }, warnings: ["different_types", "protected_name_change"],
    counts: { memories: v(5), memories_event: v(2), memories_fact: v(2), memories_view: v(1), effects: v(1), links: { count: null, status: "unavailable" }, awareness: v(3), user_aliases: v(0), other_sessions: v(1), flat_documents: v(5), contextual_documents: v(9) } };
  const s = uiSim([["/names/merge/preview", OK(preview)], ["/names", OK(names)], ["/names/merge", OK({ status: "ok" })]]);
  s.ctx.setSub("names");
  await s.ctx.memoriesTab(s.body);
  await tick(5);
  await s.btn("같은 것과 합치기", s.items()[1]).listeners.click();
  assert.ok(!s.calls.some((c) => c.path === "/names/merge/preview"), "nothing read before a name is picked");
  const pick = uiAll(s.confirm()).find((e) => e.tag === "select");
  pick.value = "N1";
  await pick.listeners.change(); await tick(5);
  assert.deepStrictEqual(s.posts("/names/merge/preview"), [{ chat_id: "A", keep: "N1", merge: "N2" }]);
  const t = uiText(s.confirm());
  assert.ok(t.includes("합치면 바뀌는 기억 5개(사건 2 · 사실 2 · 주관 1)") && t.includes("연결 ?") && t.includes("누가 아는가 3"), t);
  assert.ok(t.includes("다른 채팅 1개") && t.includes("임베딩 문서 5개 · 문맥 문서 9개"), t);
  assert.ok(t.includes("종류가 달라요") && t.includes("비밀 표시(숨김·모름)에 쓰인 이름이 바뀌어요") && t.includes("일부 수는 읽지 못해"), t);
  assert.strictEqual(s.posts("/names/merge").length, 0, "the preview does not merge");
  await s.btn("합치기", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/names/merge"), [{ chat_id: "A", keep: "N1", merge: "N2" }]);
  uiNoNull(s.body);
});
test("memories: editing reads the stored fields by ID and sends only what changed; an emptied field clears; body-only edits keep the old request; an unchanged save sends nothing", async () => {
  const list = { total: 2, memories: [{ id: "M60", turn: 3, turn_id: 5, kind: "fact", body: "리나는 성의 경비대장이다", about: "리나", mode: "신분" }, { id: "M61", turn: 3, kind: "event", body: "옛 기록" }] };
  const raw = { id: "M60", kind: "fact", body: "리나는 성의 경비대장이다", about: "N1", mode: "신분", terms: "왕이 있는 동안" };
  const s = uiSim([["/memory/list", OK(list)], ["/memory/detail", OK({ id: "M60", turn_id: 5, turn: 3, memory: raw, labels: { N1: "리나" }, related: [], next_cursor: null })],
    ["/names", OK({ names: [{ id: "N1", name: "리나", type: "person", memories: 3, aliases: [] }, { id: "N2", name: "카일", type: "person", memories: 2, aliases: [] }] })], ["/memory/edit", OK({ status: "ok" })]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  await s.btn("수정", s.items()[0]).listeners.click(); await tick(5);
  const panel = s.items()[0];
  const about = uiAll(panel).find((e) => e.tag === "select" && e["aria-label"] === "누구·무엇에 대한");
  const terms = uiAll(panel).find((e) => e.tag === "input" && e["aria-label"] === "조건");
  assert.ok(about && terms, uiText(panel));
  assert.strictEqual(about.value, "N1", "the stored ID, not the label");
  assert.strictEqual(terms.value, "왕이 있는 동안");
  about.value = "N2"; terms.value = "";
  await s.btn("저장", panel).listeners.click(); await tick(5);
  // the untouched body is not sent, so an old value cannot be written back
  assert.deepStrictEqual(s.posts("/memory/edit"), [{ chat_id: "A", id: "M60", patch: { about: "N2", terms: "" } }], "only changed fields, the emptied one as a clear");
  await s.btn("수정", s.items()[1]).listeners.click(); await tick(5);
  assert.ok(!s.calls.some((c) => c.path.includes("id=M61")), "a memory without a turn locator reads nothing more");
  await s.btn("저장", s.items()[1]).listeners.click(); await tick(5);
  assert.strictEqual(s.posts("/memory/edit").length, 1, "a save with nothing changed sends nothing");
  uiAll(s.items()[1]).find((e) => e.tag === "textarea").value = "고친 기록";
  await s.btn("저장", s.items()[1]).listeners.click(); await tick(5);
  assert.deepStrictEqual(s.posts("/memory/edit")[1], { chat_id: "A", id: "M61", body: "고친 기록" }, "the old body edit is unchanged");
  uiNoNull(s.body);
});
test("names: a recorded merge can be undone; one without a record says so", async () => {
  let names = { names: [{ id: "N1", name: "리나", type: "person", memories: 4, aliases: ["Rina"], alias_ids: ["N2"] }], merges: [{ merge_id: 12, keep: "N1", merge: "N2" }] };
  let answer = OK({ status: "ok" });
  const s = uiSim([["/names/unmerge", () => answer], ["/names", () => OK(names)]]);
  s.ctx.setSub("names");
  await s.ctx.memoriesTab(s.body);
  await tick(5);
  const row = s.items().find((e) => uiText(e).includes("Rina → 리나 합침"));
  assert.ok(row, uiText(s.body));
  answer = { ok: false, status: 404, code: "no_record" };
  await s.btn("되돌리기", row).listeners.click();
  await s.btn("되돌리기", s.confirm()).listeners.click(); await tick(5);
  assert.ok(uiText(s.confirm()).includes("되돌릴 기록을 찾지 못했어요"), uiText(s.confirm()));
  answer = OK({ status: "ok" });
  names = { names: [{ id: "N1", name: "리나", type: "person", memories: 3, aliases: [], alias_ids: [] }, { id: "N2", name: "Rina", type: "person", memories: 1, aliases: [], alias_ids: [] }], merges: [] };
  await s.btn("되돌리기", s.confirm()).listeners.click(); await tick(5);
  assert.deepStrictEqual(s.posts("/names/unmerge"), [{ chat_id: "A", merge_id: 12 }, { chat_id: "A", merge_id: 12 }]);
  // it waits for a running generation/index run; no 8 s timeout that would
  // show a failure before the undo is applied
  assert.deepStrictEqual(s.calls.filter((c) => c.path === "/names/unmerge").map((c) => c.timeoutMs), [0, 0]);
  assert.ok(!uiText(s.body).includes("합침"), "the list is read again after undoing");
  uiNoNull(s.body);
});
test("names: while a merge or an undo is under way, its row's button cannot open a second confirmation and send it again", async () => {
  const names = { names: [{ id: "N1", name: "리나", type: "person", memories: 4, aliases: ["Rina"], alias_ids: ["N9"] }, { id: "N2", name: "Lina", type: "person", memories: 1, aliases: [] }], merges: [{ merge_id: 12, keep: "N1", merge: "N9" }] };
  const answers = [];
  const wait = () => new Promise((r) => answers.push(r));
  const s = uiSim([["/names/merge/preview", OK({ counts: {} })], ["/names/merge", wait], ["/names/unmerge", wait], ["/names", () => OK(names)]]);
  s.ctx.setSub("names");
  await s.ctx.memoriesTab(s.body);
  await tick(5);
  const lina = s.items().find((e) => uiText(e).includes("Lina") && !uiText(e).includes("합침"));
  await s.btn("같은 것과 합치기", lina).listeners.click();
  uiAll(s.confirm()).find((e) => e.tag === "select").value = "N1";
  const merging = s.btn("합치기", s.confirm()).listeners.click(); await tick(1);
  const running = s.confirm();
  assert.ok(uiAll(running).some((e) => e.tag === "button" && uiText(e) === "처리 중…" && e.disabled), uiText(running));
  await s.btn("같은 것과 합치기", lina).listeners.click(); await tick(1);
  assert.strictEqual(s.confirm(), running, "the running confirmation stays");
  assert.ok(!s.btn("합치기", running), "no second enabled merge button");
  assert.strictEqual(s.posts("/names/merge").length, 1);
  answers.shift()(OK({ status: "ok" })); await merging; await tick(5);
  const row = s.items().find((e) => uiText(e).includes("Rina → 리나 합침"));
  await s.btn("되돌리기", row).listeners.click();
  const undoing = s.btn("되돌리기", s.confirm()).listeners.click(); await tick(1);
  const undoConfirm = s.confirm();
  await s.btn("되돌리기", row).listeners.click(); await tick(1);
  assert.strictEqual(s.confirm(), undoConfirm, "the running undo confirmation stays");
  assert.strictEqual(s.posts("/names/unmerge").length, 1);
  answers.shift()(OK({ status: "ok" })); await undoing; await tick(5);
  assert.strictEqual(s.posts("/names/unmerge").length, 1);
  uiNoNull(s.body);
});
test("memories: editing keeps a stored name the list lacks, offers each kind's fields, and the card shows what was sent", async () => {
  const list = { total: 2, memories: [{ id: "M70", turn: 3, turn_id: 5, kind: "view", body: "옛 본문", owner: "도윤", about: "누군가" }, { id: "M71", turn: 4, turn_id: 6, kind: "event", body: "사건" }] };
  const raws = { M70: { id: "M70", kind: "view", body: "옛 본문", owner: "N88", about: "N99", aspect: "belief" }, M71: { id: "M71", kind: "event", body: "사건", time: "새벽", outcome: "success" } };
  let release; const held = new Promise((r) => { release = r; });
  const s = uiSim([["/memory/list", OK(list)], ["/memory/detail", (b, path) => { const id = new URL("http://x" + path).searchParams.get("id"); return OK({ id, memory: raws[id], labels: { N88: "도윤", N99: "누군가" }, related: [] }); }],
    ["/names", OK({ names: [{ id: "N1", name: "리나", type: "person", memories: 1, aliases: [] }] })], ["/memory/edit", async () => { await held; return OK({ status: "ok" }); }]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  await s.btn("수정", s.items()[0]).listeners.click(); await tick(5);
  let panel = s.items()[0];
  const owner = uiAll(panel).find((e) => e.tag === "select" && e["aria-label"] === "누구의 생각");
  const aspect = uiAll(panel).find((e) => e.tag === "input" && e["aria-label"] === "생각의 갈래");
  assert.ok(owner && aspect, "the view's fields: " + uiText(panel));
  assert.strictEqual(owner.value, "N88", "a stored ID missing from the list is kept as an option");
  const text = uiAll(panel).find((e) => e.tag === "textarea");
  text.value = "보낸 본문"; aspect.value = "emotion";
  const saving = s.btn("저장", panel).listeners.click(); await tick(5);
  text.value = "보내지 않은 본문";
  release(); await saving; await tick(5);
  assert.deepStrictEqual(s.posts("/memory/edit")[0], { chat_id: "A", id: "M70", body: "보낸 본문", patch: { aspect: "emotion" } }, "untouched names are not sent as cleared; the changed body goes with the fields in one request");
  panel = s.items()[0];
  assert.ok(uiText(panel).includes("보낸 본문") && !uiText(panel).includes("보내지 않은 본문"), "the card shows what was sent: " + uiText(panel));
  await s.btn("수정", s.items()[1]).listeners.click(); await tick(5);
  const ev = s.items()[1];
  for (const label of ["때", "상황", "결과"]) assert.ok(uiAll(ev).some((e) => e.tag === "input" && e["aria-label"] === label), "event field " + label);
  uiNoNull(s.body);
});
test("names: a late names answer does not bring back an undone merge; a failed read after undoing is told as a read", async () => {
  const before = { names: [{ id: "N1", name: "리나", type: "person", memories: 4, aliases: ["Rina"], alias_ids: ["N2"] }], merges: [{ merge_id: 12, keep: "N1", merge: "N2" }] };
  const after = { names: [{ id: "N1", name: "리나", type: "person", memories: 3, aliases: [], alias_ids: [] }], merges: [] };
  const queue = [];
  const s = uiSim([["/names/unmerge", OK({ status: "ok" })], ["/names", () => new Promise((resolve) => queue.push(resolve))]]);
  s.ctx.setSub("names");
  const opened = s.ctx.memoriesTab(s.body); await tick(5);
  queue.shift()(OK(before)); await opened; await tick(5);
  s.btn("새로고침", s.body).listeners.click(); await tick(5); // left waiting on purpose
  const late = queue.shift();
  const row = s.items().find((e) => uiText(e).includes("Rina → 리나 합침"));
  await s.btn("되돌리기", row).listeners.click();
  const undoing = s.btn("되돌리기", s.confirm()).listeners.click(); await tick(5);
  queue.shift()({ ok: false, status: 500, code: "store_failed" }); await undoing; await tick(5);
  assert.ok(uiText(s.body).includes("이름 목록을 읽지 못했어요") && !uiText(s.body).includes("저장하지 못했어요"), uiText(s.body));
  assert.ok(!uiText(s.body).includes("Rina → 리나 합침"), "the undone merge's row is gone");
  late(OK(before)); await tick(5);
  assert.ok(!uiText(s.body).includes("Rina → 리나 합침"), "a late older answer does not draw it again");
  const again = s.btn("새로고침", s.body).listeners.click(); await tick(5);
  queue.shift()(OK(after)); await again; await tick(5);
  assert.ok(!uiText(s.body).includes("합침"));
  uiNoNull(s.body);
});
test("memories: a second save in the same editor can put a field back, and a slow save shows the field values that were sent", async () => {
  const list = { total: 1, memories: [{ id: "M80", turn: 2, turn_id: 4, kind: "view", body: "본문", owner: "리나", aspect: "old-aspect" }] };
  const raw = { id: "M80", kind: "view", body: "본문", owner: "N1", aspect: "old-aspect" };
  let hold = null;
  const s = uiSim([["/memory/list", OK(list)], ["/memory/detail", OK({ id: "M80", memory: raw, labels: { N1: "리나" }, related: [] })],
    ["/names", OK({ names: [{ id: "N1", name: "리나", type: "person", memories: 1, aliases: [] }] })],
    ["/memory/edit", async () => { if (hold) await hold; return OK({ status: "ok" }); }]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  await s.btn("수정", s.items()[0]).listeners.click(); await tick(5);
  const panel = s.items()[0];
  const aspect = uiAll(panel).find((e) => e.tag === "input" && e["aria-label"] === "생각의 갈래");
  aspect.value = "new-aspect";
  await s.btn("저장", panel).listeners.click(); await tick(5);
  // the editor stays open after a save; the same editor saves again
  const again = uiAll(s.items()[0]).find((e) => e.tag === "input" && e["aria-label"] === "생각의 갈래");
  assert.strictEqual(again, aspect, "the same editor is still open");
  again.value = "old-aspect";
  let release; hold = new Promise((r) => { release = r; });
  const saving = s.btn("저장", s.items()[0]).listeners.click(); await tick(5);
  again.value = "unsent-aspect";
  release(); await saving; await tick(5);
  const posts = s.posts("/memory/edit");
  assert.deepStrictEqual(posts.map((p) => p.patch && p.patch.aspect), ["new-aspect", "old-aspect"], "the second save sends the value put back");
  assert.ok(uiText(s.items()[0]).includes("old-aspect") && !uiText(s.items()[0]).includes("unsent-aspect"), "the card shows what was sent: " + uiText(s.items()[0]));
  uiNoNull(s.body);
});
test("memories: a save clicked while the editor's earlier save is still running is not sent, and the card keeps the accepted values", async () => {
  const list = { total: 1, memories: [{ id: "M81", turn: 2, turn_id: 4, kind: "view", body: "본문", owner: "리나", aspect: "original" }] };
  const raw = { id: "M81", kind: "view", body: "본문", owner: "N1", aspect: "original" };
  const answers = [];
  const s = uiSim([["/memory/list", OK(list)], ["/memory/detail", OK({ id: "M81", memory: raw, labels: { N1: "리나" }, related: [] })],
    ["/names", OK({ names: [{ id: "N1", name: "리나", type: "person", memories: 1, aliases: [] }] })],
    ["/memory/edit", () => new Promise((r) => answers.push(r))]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  await s.btn("수정", s.items()[0]).listeners.click(); await tick(5);
  const aspect = uiAll(s.items()[0]).find((e) => e.tag === "input" && e["aria-label"] === "생각의 갈래");
  aspect.value = "first";
  const first = s.btn("저장", s.items()[0]).listeners.click(); await tick(5);
  aspect.value = "original"; // put back while the first save is still running
  s.btn("저장", s.items()[0]).listeners.click(); await tick(5); // not awaited: an unguarded editor would wait for its own answer
  assert.strictEqual(s.posts("/memory/edit").length, 1, "the second click is not sent while the first is running");
  assert.ok(uiText(s.items()[0]).includes("앞의 저장이 끝난 뒤 다시 눌러 주세요."), uiText(s.items()[0]));
  answers.shift()(OK({ status: "ok" })); await first; await tick(5);
  assert.ok(uiText(s.items()[0]).includes("first"), "the card shows the accepted value: " + uiText(s.items()[0]));
  const again = s.btn("저장", s.items()[0]).listeners.click(); await tick(5);
  assert.deepStrictEqual(s.posts("/memory/edit").map((p) => p.patch && p.patch.aspect), ["first", "original"], "after it ends the put-back value is sent");
  answers.shift()({ ok: false, status: 500, code: "store_failed" }); await again; await tick(5);
  const card = uiText(s.items()[0]);
  assert.ok(card.includes("first") && card.includes("백엔드가 값을 저장하지 못했어요."), "a failed save keeps the accepted value and says it failed: " + card);
  uiNoNull(s.body);
});
test("memories: while a save runs, its editor can be neither closed nor opened again, so a second editor cannot compare with old values", async () => {
  const list = { total: 1, memories: [{ id: "M82", turn: 2, turn_id: 4, kind: "view", body: "본문", owner: "리나", aspect: "original" }] };
  const raw = { id: "M82", kind: "view", body: "본문", owner: "N1", aspect: "original" };
  const answers = [];
  const s = uiSim([["/memory/list", OK(list)], ["/memory/detail", OK({ id: "M82", memory: raw, labels: { N1: "리나" }, related: [] })],
    ["/names", OK({ names: [{ id: "N1", name: "리나", type: "person", memories: 1, aliases: [] }] })],
    ["/memory/edit", () => new Promise((r) => answers.push(r))]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  await s.btn("수정", s.items()[0]).listeners.click(); await tick(5);
  const area = () => uiAll(s.items()[0]).find((e) => e.tag === "textarea");
  const first = area();
  uiAll(s.items()[0]).find((e) => e.tag === "input" && e["aria-label"] === "생각의 갈래").value = "accepted-first";
  const saving = s.btn("저장", s.items()[0]).listeners.click(); await tick(5);
  await s.btn("취소", s.items()[0]).listeners.click(); await tick(5);
  assert.strictEqual(area(), first, "the editor stays while its save runs");
  assert.ok(uiText(s.items()[0]).includes("저장이 끝난 뒤 닫을 수 있어요."), uiText(s.items()[0]));
  await s.btn("수정", s.items()[0]).listeners.click(); await tick(5);
  assert.strictEqual(area(), first, "no second editor opens while the save runs");
  answers.shift()(OK({ status: "ok" })); await saving; await tick(5);
  assert.strictEqual(s.posts("/memory/edit").length, 1);
  await s.btn("취소", s.items()[0]).listeners.click(); await tick(5);
  assert.ok(!area(), "after the save ends the editor closes");
  uiNoNull(s.body);
});
test("memories: a save on the way keeps a redrawn list's card of that memory from opening an editor on old values", async () => {
  for (const route of ["same tab", "other tab and back", "kind filter"]) {
    let stored = { id: "M83", kind: "view", body: "body", owner: "N1", aspect: "original" };
    const answers = [];
    const s = uiSim([["/memory/list", () => OK({ total: 1, kinds: { view: 1 }, memories: [{ ...stored, turn: 2, turn_id: 4, owner: "name" }] })],
      ["/memory/detail", () => OK({ id: "M83", memory: { ...stored }, labels: { N1: "name" }, related: [] })],
      ["/names", OK({ names: [{ id: "N1", name: "name", type: "person", memories: 1, aliases: [] }], merges: [] })],
      ["/memory/edit", (body) => new Promise((r) => answers.push(() => {
        stored = { ...stored, ...body.patch, ...(body.body !== undefined ? { body: body.body } : {}), ...(body.imp !== undefined ? { imp: body.imp } : {}) };
        r(OK({ status: "ok" }));
      }))]]);
    s.ctx.setSub("list");
    await s.ctx.memoriesTab(s.body);
    await s.btn("수정", s.items()[0]).listeners.click(); await tick(5);
    uiAll(s.items()[0]).find((e) => e["aria-label"] === "생각의 갈래").value = "accepted-first";
    uiAll(s.items()[0]).find((e) => e.tag === "textarea").value = "accepted body";
    uiAll(s.items()[0]).find((e) => e["aria-label"] === "중요도").value = "7";
    const saving = s.btn("저장", s.items()[0]).listeners.click(); await tick(1);
    if (route === "other tab and back") { await s.btn("이름").listeners.click(); await tick(5); await s.btn("기억").listeners.click(); }
    else if (route === "kind filter") await uiAll(s.body).find((e) => e.className === "ac2-kind" && uiText(e).startsWith("주관 기억")).listeners.click();
    else await s.btn("기억").listeners.click();
    await tick(5);
    const card = s.items()[0];
    await s.btn("수정", card).listeners.click(); await tick(5);
    assert.ok(!uiAll(card).find((e) => e.tag === "textarea"), route + ": no editor opens while the save runs");
    assert.ok(uiText(card).includes("다른 작업을 처리하는 중이에요. 끝난 뒤 다시 눌러 주세요."), route + ": " + uiText(card));
    answers.shift()(); await saving; await tick(5);
    await s.btn("수정", card).listeners.click(); await tick(5);
    const aspect = uiAll(card).find((e) => e["aria-label"] === "생각의 갈래");
    assert.strictEqual(aspect.value, "accepted-first", route + ": the editor opened after the save reads the saved value");
    // the redrawn card held the old body and importance; the editor shows the
    // stored ones, so changing another field cannot write the old back
    assert.strictEqual(uiAll(card).find((e) => e.tag === "textarea").value, "accepted body", route);
    assert.strictEqual(uiAll(card).find((e) => e["aria-label"] === "중요도").value, "7", route);
    aspect.value = "original";
    const second = s.btn("저장", card).listeners.click(); await tick(1);
    answers.shift()(); await second;
    assert.strictEqual(s.posts("/memory/edit").length, 2, route);
    assert.strictEqual(s.posts("/memory/edit")[1].patch.aspect, "original", route + ": changing it back is sent");
    assert.strictEqual(stored.aspect, "original", route);
    assert.strictEqual(stored.body, "accepted body", route + ": the saved body stays");
    assert.strictEqual(stored.imp, 7, route + ": the saved importance stays");
    uiNoNull(s.body);
  }
});
test("memories: a card drawn before another save ended never writes the saved body or importance back, whether its reads fail or are late", async () => {
  // the editor sends only what the user changed from what it showed
  const stale = async (mode) => {
    let stored = { id: "M84", kind: "view", body: "original body", imp: 2, owner: "N1", aspect: "original" };
    let details = 0, names = 0, late;
    const answers = [];
    const s = uiSim([["/memory/list", () => OK({ total: 1, kinds: { view: 1 }, memories: [{ ...stored, turn: 2, turn_id: 4, owner: "name" }] })],
      ["/memory/detail", () => { details++; if (details === 1) return OK({ memory: { ...stored } }); if (mode === "detail fails") return { ok: false, code: "store_failed" }; if (mode === "detail late") return new Promise((r) => { late = () => r(OK({ memory: { ...stored } })); }); return OK({ memory: { ...stored } }); }],
      ["/names", () => { names++; return names > 1 && mode === "names fail" ? { ok: false, code: "store_failed" } : OK({ names: [{ id: "N1", name: "name", type: "person", aliases: [] }], merges: [] }); }],
      ["/memory/edit", (b) => new Promise((r) => answers.push(() => { stored = { ...stored, ...(b.patch || {}), ...(b.body !== undefined ? { body: b.body } : {}), ...(b.imp !== undefined ? { imp: b.imp } : {}) }; r(OK({ status: "ok" })); }))]]);
    s.ctx.setSub("list");
    await s.ctx.memoriesTab(s.body);
    await s.btn("수정", s.items()[0]).listeners.click(); await tick(5);
    uiAll(s.items()[0]).find((e) => e.tag === "textarea").value = "accepted body";
    uiAll(s.items()[0]).find((e) => e["aria-label"] === "중요도").value = "7";
    const first = s.btn("저장", s.items()[0]).listeners.click(); await tick(1);
    await s.btn("기억").listeners.click(); await tick(5); // the list is read while that save is on the way
    answers.shift()(); await first; await tick(5);
    const card = s.items()[0];
    await s.btn("수정", card).listeners.click(); await tick(5);
    const field = (label) => uiAll(card).find((e) => e["aria-label"] === label || (label === "기억 내용" && e.tag === "textarea"));
    return { s, card, field, answers, stored: () => stored, late: () => late && late() };
  };
  for (const mode of ["names fail", "detail fails", "detail late"]) {
    let e = await stale(mode);
    e.field("중요도").value = "8";
    let saving = e.s.btn("저장", e.card).listeners.click(); await tick(1);
    assert.strictEqual(e.s.posts("/memory/edit")[1].body, undefined, mode + ": the untouched body is not sent");
    e.answers.shift()(); await saving;
    assert.strictEqual(e.stored().body, "accepted body", mode);
    assert.strictEqual(e.stored().imp, 8, mode);
    e = await stale(mode);
    e.field("기억 내용").value = "second body";
    saving = e.s.btn("저장", e.card).listeners.click(); await tick(1);
    assert.strictEqual(e.s.posts("/memory/edit")[1].imp, undefined, mode + ": the untouched importance is not sent");
    e.answers.shift()(); await saving;
    assert.strictEqual(e.stored().body, "second body", mode);
    assert.strictEqual(e.stored().imp, 7, mode);
  }
  // typed before a late read: the read does not replace it, and it is sent (0 is a value)
  const e = await stale("detail late");
  e.field("기억 내용").value = "user typed"; e.field("중요도").value = "0";
  e.late(); await tick(5);
  assert.strictEqual(e.field("기억 내용").value, "user typed");
  assert.strictEqual(e.field("중요도").value, "0");
  const saving = e.s.btn("저장", e.card).listeners.click(); await tick(1);
  e.answers.shift()(); await saving;
  assert.strictEqual(e.stored().body, "user typed");
  assert.strictEqual(e.stored().imp, 0);
  uiNoNull(e.s.body);
});
test("memories: a read answered after the editor already saved does not put the older text back on screen", async () => {
  let stored = { id: "M85", kind: "view", body: "old body", imp: 2, owner: "N1", aspect: "a" };
  let lateDetail;
  const s = uiSim([["/memory/list", () => OK({ total: 1, kinds: { view: 1 }, memories: [{ ...stored, turn: 2, turn_id: 4, owner: "name" }] })],
    ["/memory/detail", () => new Promise((r) => { const at = { ...stored }; lateDetail = () => r(OK({ memory: at })); })],
    ["/names", OK({ names: [{ id: "N1", name: "name", type: "person", aliases: [] }], merges: [] })],
    ["/memory/edit", (b) => { stored = { ...stored, ...(b.body !== undefined ? { body: b.body } : {}) }; return OK({ status: "ok" }); }]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  await s.btn("수정", s.items()[0]).listeners.click(); await tick(5);
  const text = uiAll(s.items()[0]).find((e) => e.tag === "textarea");
  text.value = "new body";
  await s.btn("저장", s.items()[0]).listeners.click(); await tick(5);
  lateDetail(); await tick(5); // answered with the values from before the save
  assert.strictEqual(text.value, "new body", "the editor keeps what was saved");
  assert.strictEqual(stored.body, "new body");
  uiNoNull(s.body);
});
test("names: while a merge or undo runs, a list drawn again cannot send it a second time, and the list shown then reads the result", async () => {
  // one write at a time, and the view drawn during the write reads again after it
  for (const op of ["merge", "unmerge"]) {
    const before = { names: [{ id: "N1", name: "Keep", type: "person", memories: 1, aliases: ["Merge"], alias_ids: ["N2"] }, { id: "N3", name: "Other", type: "person", memories: 1, aliases: [] }], merges: [{ merge_id: 12, merge: "N2", keep: "N1" }] };
    const after = op === "merge"
      ? { names: [{ id: "N3", name: "Other", type: "person", memories: 2, aliases: ["Keep"], alias_ids: ["N1"] }], merges: [{ merge_id: 12, merge: "N2", keep: "N1" }, { merge_id: 13, merge: "N1", keep: "N3" }] }
      : { names: [{ id: "N1", name: "Keep", type: "person", memories: 1, aliases: [] }, { id: "N2", name: "Merge", type: "person", memories: 0, aliases: [] }, { id: "N3", name: "Other", type: "person", memories: 1, aliases: [] }], merges: [] };
    let done = false;
    const answers = [];
    const s = uiSim([["/names/merge/preview", OK({ counts: {} })], ["/names/" + op, () => new Promise((r) => answers.push(() => { done = true; r(OK({ status: "ok" })); }))], ["/names", () => OK(done ? after : before)]]);
    s.ctx.setSub("names");
    await s.ctx.memoriesTab(s.body);
    await tick(5);
    const begin = async () => {
      if (op === "merge") { await s.btn("같은 것과 합치기", s.items()[0]).listeners.click(); const pick = uiAll(s.confirm() || s.body).find((e) => e.tag === "select"); if (pick) pick.value = "N3"; }
      else await s.btn("되돌리기", s.items().find((e) => uiText(e).includes("합침"))).listeners.click();
    };
    await begin();
    const running = s.btn(op === "merge" ? "합치기" : "되돌리기", s.confirm()).listeners.click(); await tick(1);
    await s.btn("이름").listeners.click(); await tick(5); // the list is drawn again while it runs
    await begin(); await tick(1);
    assert.ok(uiText(s.body).includes("다른 작업을 처리하는 중이에요. 끝난 뒤 다시 눌러 주세요."), op + ": " + uiText(s.body));
    assert.ok(!s.confirm(), op + ": no second confirmation opens, so nothing can be sent again");
    assert.strictEqual(s.posts("/names/" + op).length, 1, op);
    answers.shift()(); await running; await tick(10);
    assert.strictEqual(s.posts("/names/" + op).length, 1, op);
    const shown = uiText(s.body);
    if (op === "merge") assert.ok(shown.includes("Keep → Other 합침") || shown.includes("다른 이름 Keep"), "the list shown now has the merge: " + shown);
    else assert.ok(!shown.includes("Merge → Keep 합침") && shown.includes("Merge"), "the list shown now has the undo: " + shown);
    uiNoNull(s.body);
  }
});
test("memories: an importance typed while the save runs is sent by the next save", async () => {
  // The comparison after a save is what that save sent
  const answers = [];
  const s = uiSim([["/memory/edit", (b) => new Promise((r) => answers.push(() => r(OK({ status: "ok" }))))]]);
  vm.runInContext("this.makeCard = memoryCard;", s.ctx);
  const card = s.ctx.makeCard("A", { id: "M90", kind: "view", body: "body", imp: 2 }, null);
  s.body.append(card);
  await s.btn("수정", card).listeners.click(); await tick(1);
  const imp = uiAll(card).find((e) => e["aria-label"] === "중요도");
  imp.value = "7";
  const first = s.btn("저장", card).listeners.click(); await tick(1);
  imp.value = "8";
  answers.shift()(); await first;
  const second = s.btn("저장", card).listeners.click(); await tick(1);
  assert.deepStrictEqual(s.posts("/memory/edit").map((b) => b.imp), [7, 8]);
  answers.shift()(); await second;
  uiNoNull(s.body);
});
test("one write at a time: each of the six other confirmations sends once though its screen is drawn again, and the list shown then has the result", async () => {
  // cold start, one memory, chosen memories, a turn remake, a chat delete and the reindex
  for (const kind of ["one memory", "chosen memories", "turn remake", "cold start", "chat delete", "reindex"]) {
    const pending = [];
    let deleted = false;
    const mem = { id: "M1", kind: "view", body: "body" };
    const s = uiSim([
      ["/memory/list", () => OK(deleted ? { total: 0, kinds: {}, memories: [] } : { total: 1, kinds: { view: 1 }, memories: [mem] })],
      ["/memory/delete", () => new Promise((r) => pending.push(() => { deleted = true; r(OK({ remade: [] })); }))],
      ["/chats/turns", OK({ total: 1, turns: [{ turn: 1, user: "u", reply: "a", memories: 1, state: "made" }] })],
      ["/memory/turn/remake", () => new Promise((r) => pending.push(() => r(OK({}))))],
      ["/memory/backfill", (b) => (b && b.max_turns !== undefined ? new Promise((r) => pending.push(() => r(OK({ turns: [] })))) : OK({ total: 0 }))],
      ["/chats/delete", (b) => (b.dry_run ? OK({ deleted_turns: 1, deleted_memories: 1 }) : new Promise((r) => pending.push(() => { deleted = true; r(OK({ deleted_turns: 1, deleted_memories: 1 })); })))],
      ["/chats", () => OK({ chats: deleted ? [] : [{ chat_id: "B", char_name: "", chat_name: "B", turns: 1, memories: { total: 1, event: 0, fact: 1, view: 0 }, names: 0 }], total: deleted ? 0 : 1 })],
      ["/settings/hud", OK({ enabled: false })], ["/status", OK({ embedding: { waiting_vectors: 1, failures: 0 } })], ["/chats/attach", OK({ chats: [], turns: 0, memories: 0 })],
      ["/maintenance/reindex", (b) => (b ? new Promise((r) => pending.push(() => r(OK({ count: 1 })))) : OK({ count: 1, chats: 1, model: "synthetic" }))]]);
    vm.runInContext("coldSnapshot = async () => ({ synthetic: true });", s.ctx);
    let start, again, path, label;
    if (kind === "one memory" || kind === "chosen memories") {
      s.ctx.setSub("list"); await s.ctx.memoriesTab(s.body); path = "/memory/delete"; label = "삭제";
      start = async () => {
        if (kind === "one memory") return s.btn("삭제", s.items()[0]).listeners.click();
        const choose = s.btn("여러 개 고르기"); // still choosing after the list was drawn again
        if (choose) { await choose.listeners.click(); await tick(3); }
        const c = uiAll(s.items()[0]).find((x) => x.type === "checkbox"); c.checked = true; c.listeners.change();
        await s.btn("고른 기억 1개 지우기").listeners.click();
      };
      again = () => uiAll(s.body).find((x) => x.className === "ac2-kind" && uiText(x).startsWith("주관 기억")).listeners.click();
    } else if (kind === "turn remake") {
      s.ctx.setSub("turns"); await s.ctx.memoriesTab(s.body); path = "/memory/turn/remake"; label = "다시 만들기";
      start = () => s.btn("이 턴 다시 만들기").listeners.click(); again = () => s.btn("대화 원문").listeners.click();
    } else if (kind === "cold start") {
      s.ctx.setSub("cold"); await s.ctx.memoriesTab(s.body); path = "/memory/backfill"; label = "실행";
      start = () => s.btn("콜드 스타트 실행").listeners.click(); again = () => s.btn("지난 대화로 만들기").listeners.click();
    } else if (kind === "chat delete") {
      await s.ctx.chatsTab(s.body, () => {}); path = "/chats/delete"; label = "삭제";
      start = () => s.btn("삭제", s.items().find((e) => uiText(e).includes("B"))).listeners.click(); again = () => s.btn("새로고침").listeners.click();
    } else {
      const mount = () => s.body.replaceChildren(s.ctx.maintenanceBox());
      mount(); path = "/maintenance/reindex"; label = "채우기";
      start = () => s.btn("빠진 벡터 채우기").listeners.click(); again = mount;
    }
    await tick(5);
    const sends = () => s.posts(path).filter((b) => !b.dry_run && (path !== "/memory/backfill" || b.max_turns !== undefined)).length;
    await start(); await tick(5);
    const running = s.btn(label, s.confirm()).listeners.click(); await tick(1);
    await again(); await tick(5); // the screen is drawn again while it runs
    await start(); await tick(5);
    const second = s.confirm() && s.btn(label, s.confirm());
    if (second && !second.disabled) { second.listeners.click(); await tick(1); }
    assert.strictEqual(sends(), 1, kind + ": sent once");
    // either the reason, or the running confirmation itself where it stayed (an in-place reload)
    assert.ok(uiText(s.body).includes("다른 작업을 처리하는 중이에요") || uiText(s.body).includes("처리 중…"), kind + ": the reason is shown: " + uiText(s.body));
    pending.shift()(); await running; await tick(10);
    assert.strictEqual(sends(), 1, kind);
    if (kind === "one memory" || kind === "chosen memories") assert.ok(!s.items().some((e) => uiText(e).includes("body")), kind + ": the list shown now has no deleted memory: " + uiText(s.body));
    if (kind === "chat delete") assert.ok(!s.items().some((e) => uiText(e).includes("B")), kind + ": the list shown now has no deleted chat: " + uiText(s.body));
    uiNoNull(s.body);
  }
});
test("settings: the write under way is shown on every tab; a failure after its tab was left is told there, and nothing is sent twice", async () => {
  for (const outcome of ["failure", "success"]) {
    let finish, name = "First";
    const s = uiSim([["/update/status", OK({ status: "not_configured", message: "synthetic", current_version: "fixture", update_available: false, apply_supported: false })],
      ["/names", () => OK({ names: [{ id: "N1", name, type: "person", aliases: [], memories: 1 }], merges: [] })],
      ["/names/edit", (b) => new Promise((r) => { finish = () => { if (outcome === "success") name = b.name; r(outcome === "success" ? OK({ status: "ok" }) : { ok: false, code: "store_failed" }); }; })]]);
    s.ctx.setTab("memories"); s.ctx.setSub("names");
    await s.ctx.openSettings(); await tick(5);
    const root = s.ctx.document.body;
    const tab = async (label) => { uiAll(root).find((e) => e.className === "ac2-tab" && uiText(e) === label).listeners.click(); await tick(10); };
    const items = () => uiAll(root).filter((e) => e.className === "ac2-item");
    const confirm = () => uiAll(root).find((e) => (" " + e.className + " ").includes(" ac2-confirm "));
    const shown = (text) => uiAll(root).some((e) => uiShown(e) && typeof e.children[0] === "string" && e.children[0].includes(text));
    await s.btn("이름 고치기", items()[0]).listeners.click();
    uiAll(confirm()).find((e) => e.tag === "input").value = "Second";
    const running = s.btn("이름 저장", confirm()).listeners.click(); await tick(1);
    assert.ok(shown("처리 중인 작업이 있어요"), uiText(root));
    await tab("업데이트");
    assert.ok(shown("처리 중인 작업이 있어요"), "shown on another tab: " + uiText(root));
    await tab("기억");
    await s.ctx.openSettings(); await tick(10); // closed and opened again while it runs
    assert.ok(shown("처리 중인 작업이 있어요"), "shown when the screen is opened again: " + uiText(root));
    await s.btn("이름 고치기", items()[0]).listeners.click(); await tick(1);
    assert.ok(!confirm(), "no second confirmation opens");
    assert.strictEqual(s.posts("/names/edit").length, 1);
    finish(); await running; await tick(10);
    if (outcome === "failure") assert.ok(shown("앞의 작업이 끝나지 못했어요"), "the failure is told on the screen shown now: " + uiText(root));
    else {
      assert.ok(!shown("처리 중인 작업이 있어요") && !shown("끝나지 못했어요"), uiText(root));
      assert.ok(uiText(items()[0]).includes("Second"), "the names shown now have the saved name: " + uiText(root));
    }
    assert.strictEqual(s.posts("/names/edit").length, 1);
    uiNoNull(root);
  }
});
test("settings: a failure is told though its confirmation was cleared by a list read or the screen was closed, and an editor saving stays with its typing", async () => {
  // failures stay visible; a busy notice never covers an editor
  const fail = { ok: false, code: "store_failed" };
  const mem = { id: "M1", kind: "view", body: "body", imp: 2, turn: 1 };
  const setup = async (answers) => {
    const s = uiSim([["/memory/list", () => OK({ total: 1, kinds: { view: 1 }, memories: [{ ...mem }] })], ...answers]);
    s.ctx.setTab("memories"); s.ctx.setSub("list");
    await s.ctx.openSettings(); await tick(5);
    const root = s.ctx.document.body;
    s.root = root;
    s.itemsNow = () => uiAll(root).filter((e) => e.className === "ac2-item");
    s.confirmNow = () => uiAll(root).find((e) => (" " + e.className + " ").includes(" ac2-confirm "));
    s.visible = () => uiAll(root).filter(uiShown).flatMap((e) => [e.textContent || "", ...e.children.filter((x) => typeof x === "string")]).join(" ");
    return s;
  };
  // chosen memories: the list is read again in place (kind filter) while the delete runs
  let finish;
  let s = await setup([["/memory/delete", () => new Promise((r) => { finish = r; })]]);
  await s.btn("여러 개 고르기", s.root).listeners.click(); await tick(3);
  const c = uiAll(s.itemsNow()[0]).find((e) => e.type === "checkbox"); c.checked = true; c.listeners.change();
  await s.btn("고른 기억 1개 지우기", s.root).listeners.click();
  let running = s.btn("삭제", s.confirmNow()).listeners.click(); await tick(1);
  await uiAll(s.root).find((e) => e.className === "ac2-kind" && uiText(e).startsWith("주관 기억")).listeners.click(); await tick(3);
  finish(fail); await running; await tick(3);
  assert.match(s.visible(), /백엔드가 값을 저장하지 못했어요|끝나지 못했어요/, "bulk delete failure: " + s.visible());
  assert.strictEqual(s.posts("/memory/delete").length, 1);
  // one memory: settings closed while it runs, then opened again
  s = await setup([["/memory/delete", () => new Promise((r) => { finish = r; })]]);
  await s.btn("삭제", s.itemsNow()[0]).listeners.click();
  running = s.btn("삭제", s.confirmNow()).listeners.click(); await tick(1);
  await s.btn("닫기", s.root).listeners.click();
  finish(fail); await running;
  await s.ctx.openSettings(); await tick(3);
  assert.match(s.visible(), /끝나지 못했어요/, "failure told after opening again: " + s.visible());
  // an editor saving: its card's delete button shows the line above and leaves the editor and typing
  s = await setup([["/memory/edit", () => new Promise((r) => { finish = r; })]]);
  const card = s.itemsNow()[0];
  await s.btn("수정", card).listeners.click();
  const field = uiAll(card).find((e) => e.tag === "textarea");
  field.value = "sent body";
  running = s.btn("저장", card).listeners.click(); await tick(1);
  field.value = "unsaved later body";
  await s.btn("삭제", card).listeners.click(); await tick(1);
  assert.ok(field.isConnected, "the editor stays");
  assert.match(s.visible(), /처리 중인 작업이 있어요/);
  finish(fail); await running; await tick(2);
  assert.ok(field.isConnected && field.value === "unsaved later body", "the typing stays");
  assert.match(s.visible(), /백엔드가 값을 저장하지 못했어요/, "the failure is told in the editor: " + s.visible());
  assert.strictEqual(s.posts("/memory/delete").length, 0);
  uiNoNull(s.root);
});
test("lists: an older read answered after a newer one is not drawn, so a deleted row does not come back", async () => {
  // only the latest read of a list draws
  let deleted = false, finish, oldRead, reads = 0;
  const mem = { id: "M1", kind: "view", body: "body", imp: 2, turn: 1 };
  const s = uiSim([["/memory/list", () => { reads++; if (reads === 2) return new Promise((r) => { oldRead = () => r(OK({ total: 1, kinds: { view: 1 }, memories: [{ ...mem }] })); }); return OK(deleted ? { total: 0, kinds: {}, memories: [] } : { total: 1, kinds: { view: 1 }, memories: [{ ...mem }] }); }],
    ["/memory/delete", () => new Promise((r) => { finish = () => { deleted = true; r(OK({ remade: [] })); }; })]]);
  s.ctx.setSub("list");
  await s.ctx.memoriesTab(s.body);
  await s.btn("삭제", s.items()[0]).listeners.click();
  const p = s.btn("삭제", s.confirm()).listeners.click(); await tick(1);
  const filter = uiAll(s.body).find((e) => e.className === "ac2-kind" && uiText(e).startsWith("주관 기억")).listeners.click(); await tick(1);
  finish(); await p; await tick(2);
  oldRead(); await filter; await tick(2);
  assert.strictEqual(s.items().filter((e) => uiText(e).includes("body")).length, 0, "the deleted row stays gone: " + uiText(s.body));
  uiNoNull(s.body);
});
test("lists: in the turns, chats, cold start and reindex views an older read answered late is not drawn over a newer one", async () => {
  // only the latest read of each view draws
  const late = [];
  const once = (oldAnswer, newAnswer) => { let n = 0; return () => (++n === 1 ? new Promise((r) => late.push(() => r(oldAnswer))) : newAnswer); };
  const cases = [
    ["turns", "/chats/turns", OK({ total: 1, turns: [{ turn: 1, user: "OLD-TURN", reply: "a", memories: 1, state: "made" }] }), OK({ total: 1, turns: [{ turn: 1, user: "NEW-TURN", reply: "a", memories: 1, state: "made" }] }),
      async (s) => { s.ctx.setSub("turns"); await s.ctx.memoriesTab(s.body); }, (s) => vm.runInContext("refreshShown()", s.ctx), "OLD-TURN", "NEW-TURN"],
    ["chats", "/chats", OK({ chats: [{ chat_id: "B", char_name: "", chat_name: "OLD-CHAT", turns: 1, memories: { total: 1, event: 0, fact: 1, view: 0 }, names: 0 }], total: 1 }), OK({ chats: [{ chat_id: "B", char_name: "", chat_name: "NEW-CHAT", turns: 1, memories: { total: 1, event: 0, fact: 1, view: 0 }, names: 0 }], total: 1 }),
      async (s) => { await s.ctx.chatsTab(s.body, () => {}); }, (s) => s.btn("새로고침").listeners.click(), "OLD-CHAT", "NEW-CHAT"],
    ["cold start", "/memory/backfill", OK({ total: 3, done: 1, failed: 0, waiting: 2 }), OK({ total: 3, done: 3, failed: 0, waiting: 0 }),
      async (s) => { s.ctx.setSub("cold"); await s.ctx.memoriesTab(s.body); }, (s) => vm.runInContext("refreshShown()", s.ctx), "1 / 3턴", "3 / 3턴"],
    ["reindex", "/status", OK({ embedding: { waiting_vectors: 77, failures: 0 } }), OK({ embedding: { waiting_vectors: 5, failures: 0 } }),
      async (s) => { s.body.append(s.ctx.maintenanceBox()); }, (s) => s.btn("새로고침").listeners.click(), "77", "5"],
  ];
  for (const [kind, path, oldAnswer, newAnswer, open, again, oldText, newText] of cases) {
    late.length = 0;
    const s = uiSim([[path, once(oldAnswer, newAnswer)], ["/settings/hud", OK({ enabled: false })], ["/chats/attach", OK({ chats: [], turns: 0, memories: 0 })]]);
    vm.runInContext("coldSnapshot = async () => ({ synthetic: true });", s.ctx);
    const opening = open(s); await tick(3); // the first read waits
    await again(s); await tick(5); // a newer read of the same view answers first
    assert.ok(uiText(s.body).includes(newText), kind + ": " + uiText(s.body));
    for (const r of late) r(); await opening; await tick(5);
    assert.ok(uiText(s.body).includes(newText) && !uiText(s.body).includes(oldText), kind + ": the older answer is not drawn: " + uiText(s.body));
    uiNoNull(s.body);
  }
});
test("names: an open name editor keeps its typing while another name saves, and a failed save keeps its input to retry", async () => {
  // a busy notice or a failure never replaces a confirmation's inputs
  const names = { names: [{ id: "N1", name: "FIRST", type: "person", memories: 1, aliases: [] }, { id: "N2", name: "SECOND", type: "person", memories: 1, aliases: [] }], merges: [] };
  const answers = [];
  const s = uiSim([["/names", () => OK(names)], ["/names/edit", () => new Promise((r) => answers.push(r))]]);
  s.ctx.setSub("names");
  await s.ctx.memoriesTab(s.body);
  await tick(5);
  const row = (name) => s.items().find((e) => uiText(e).includes(name));
  const confirmOf = (r) => uiAll(r).find((e) => (" " + e.className + " ").includes(" ac2-confirm "));
  await s.btn("이름 고치기", row("FIRST")).listeners.click();
  await s.btn("이름 고치기", row("SECOND")).listeners.click();
  const second = uiAll(confirmOf(row("SECOND"))).find((e) => e.tag === "input");
  second.value = "UNSAVED_SECOND_NAME";
  const first = s.btn("이름 저장", confirmOf(row("FIRST"))).listeners.click(); await tick(1);
  await s.btn("이름 고치기", row("SECOND")).listeners.click(); await tick(1); // its row button again
  assert.ok(second.isConnected && second.value === "UNSAVED_SECOND_NAME", "the typing stays when the row button is pressed");
  await s.btn("이름 저장", confirmOf(row("SECOND"))).listeners.click(); await tick(1); // its own save
  assert.ok(second.isConnected, "the typing stays when its save waits");
  assert.ok(uiText(confirmOf(row("SECOND"))).includes("다른 작업을 처리하는 중이에요"), uiText(row("SECOND")));
  assert.strictEqual(s.posts("/names/edit").length, 1, "nothing more is sent");
  answers.shift()({ ok: false, code: "store_failed" }); await first; await tick(5);
  const firstInput = uiAll(confirmOf(row("FIRST"))).find((e) => e.tag === "input");
  assert.ok(firstInput && firstInput.isConnected, "a failed save keeps its input to fix and retry");
  assert.ok(uiText(confirmOf(row("FIRST"))).includes("백엔드가 값을 저장하지 못했어요"), uiText(row("FIRST")));
  uiNoNull(s.body);
});
test("maintenance: pressing the reindex button again while it runs keeps its running confirmation", async () => {
  let resolve;
  const s = uiSim([["/settings/hud", OK({ enabled: false })], ["/status", OK({ embedding: { waiting_vectors: 1, failures: 0 } })],
    ["/maintenance/reindex", (b) => (b ? new Promise((r) => { resolve = r; }) : OK({ count: 1, chats: 1, model: "synthetic" }))]]);
  s.body.append(s.ctx.maintenanceBox()); await tick(5);
  await s.btn("빠진 벡터 채우기").listeners.click(); await tick(5);
  const p = s.btn("채우기", s.confirm()).listeners.click(); await tick(1);
  const before = s.confirm();
  const reads = s.calls.filter((c) => c.path.startsWith("/maintenance/reindex?")).length;
  await s.btn("빠진 벡터 채우기").listeners.click(); await tick(5);
  assert.strictEqual(s.confirm(), before, "the running confirmation stays");
  assert.strictEqual(s.calls.filter((c) => c.path.startsWith("/maintenance/reindex?")).length, reads, "nothing is read again, so a changed count cannot hide it");
  assert.ok(uiText(before).includes("처리 중…"));
  assert.strictEqual(s.posts("/maintenance/reindex").length, 1);
  resolve(OK({ count: 1 })); await p; await tick(5);
  uiNoNull(s.body);
});
test("status: the backend's count of chats being worked on now is shown", async () => {
  const s = uiSim([["/status", OK({ chip: "done", version: "fixture", chat: "A", generation: { critic: {}, chat: {}, running_chats: 2 }, embedding: { running_chats: 1 }, activity: [] })], ["/settings/hud", OK({ enabled: false })]]);
  await s.ctx.statusTab(s.body);
  const t = uiText(s.body);
  assert.ok(t.includes("지금 기억을 만드는 채팅2") && t.includes("지금 벡터를 만드는 채팅1"), t);
});
test("maintenance: the HUD setting saves at once; reindex shows the targets and cost before it runs; the reset takes the exact phrase", async () => {
  let embeddingOn = true;
  const s = uiSim([["/settings/hud", (b) => OK(b || { enabled: true, compact: true })], ["/status", OK({ embedding: { waiting_vectors: 5, failures: 1 } })],
    ["/maintenance/reindex", (b, path) => (!embeddingOn ? { ok: false, status: 400, code: "embedding_off" } : b ? OK({ count: 7 }) : OK({ count: 7, chats: path.includes("chat_id") ? 1 : 3, model: "voyage-context-4" }))],
    ["/maintenance/reset", (b) => (b.phrase === "전체 DB 초기화" ? OK({ status: "ok" }) : { ok: false, status: 400, code: "reset_phrase_mismatch" })]], { id: "A", name: "리나 / 첫 채팅", open: "A", isOpen: true });
  s.body.append(s.ctx.maintenanceBox());
  await tick(5);
  const on = uiAll(s.body).find((e) => e.id === "ac2-hud-on");
  assert.strictEqual(on.role, "switch");
  assert.strictEqual(on["aria-checked"], "true");
  await on.listeners.click();
  assert.deepStrictEqual(s.posts("/settings/hud"), [{ enabled: false, compact: true }]);
  assert.strictEqual(on["aria-checked"], "false", "the switch shows off");
  assert.strictEqual(s.hudChanges(), 1, "the HUD is told");
  assert.ok(uiText(s.body).includes("벡터를 기다리는 기억5"));
  await s.btn("빠진 벡터 채우기").listeners.click();
  assert.ok(uiText(s.confirm()).includes("대상: 리나 / 첫 채팅 · 1개 채팅, 7개 기억 · voyage-context-4") && uiText(s.confirm()).includes("비용이 들어요"));
  assert.strictEqual(s.posts("/maintenance/reindex").length, 0);
  await s.btn("채우기", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/maintenance/reindex"), [{ mode: "missing", chat_id: "A" }]);
  uiAll(s.body).find((e) => e.tag === "select").value = "all";
  await s.btn("벡터 전부 다시 만들기").listeners.click();
  await s.btn("다시 만들기", s.confirm()).listeners.click();
  assert.deepStrictEqual(s.posts("/maintenance/reindex").at(-1), { mode: "all" });
  embeddingOn = false;
  await s.btn("빠진 벡터 채우기").listeners.click();
  assert.ok(uiText(s.body).includes("임베딩이 꺼져 있어요") && !s.confirm());
  const phrase = uiAll(s.body).find((e) => e["aria-label"] === "초기화 확인 문구");
  phrase.value = "전체 DB 초기화 ";
  await s.btn("전체 DB 초기화").listeners.click();
  assert.ok(uiText(s.body).includes("‘전체 DB 초기화’를 정확히 입력해 주세요"));
  phrase.value = "전체 DB 초기화";
  await s.btn("전체 DB 초기화").listeners.click();
  assert.deepStrictEqual(s.posts("/maintenance/reset").map((b) => b.phrase), ["전체 DB 초기화 ", "전체 DB 초기화"], "the phrase goes as typed; the backend checks it");
  assert.ok(uiText(s.body).includes("모든 채팅의 기록을 지웠어요"));
  uiNoNull(s.body);
});

test("turn HUD: off draws nothing; on, the chip names the memory given and the last turn's state, and it asks again only while a turn is under way", async () => {
  const off = u6View(false); off.settings.enabled = false;
  const h0 = await u6Host({ hud: off });
  assert.strictEqual(h0.root.children.length, 0);
  assert.strictEqual(h0.permissions(), 0, "the page is not asked for while the HUD is off");
  await h0.close();
  const h = await u6Host({ hud: u6View(true) });
  await tick(30);
  assert.ok(u6Text(h.root).includes("AC2 · 만드는 중"), u6Text(h.root));
  assert.ok(!u6Text(h.root).includes("넣음") && !u6Text(h.root).includes("17/100"), "the memory given stays off the chat screen (user 10-28)");
  assert.ok(!u6Text(h.root).includes("AC2 기억 처리"), "compact: one line");
  assert.strictEqual(h.pending().length, 1, "under way: asked again");
  const reads = h.reads();
  h.state.hud = u6View(false);
  await h.fire();
  assert.strictEqual(h.reads(), reads, "asking again does not read the host chat");
  assert.ok(u6Text(h.root).includes("AC2 · 2개 저장"), u6Text(h.root));
  assert.strictEqual(h.pending().length, 0, "done: no more asking");
  const asks = h.asks.length;
  const payload = [{ role: "user", content: "hi" }];
  let out = payload;
  for (const fn of h.host.handlers.beforeRequest) out = await fn(out, "model");
  await tick(30);
  assert.strictEqual(out, payload);
  assert.ok(h.asks.length > asks, "a request tells the HUD");
  await h.close();
  assert.strictEqual(h.root.children.length, 0, "unload removes the chip");
});

test("turn HUD: the chip opens to the memory given and the stages; a failed turn offers its retry; closing hides it", async () => {
  const h = await u6Host({ hud: u6View(false, true) });
  await tick(30);
  assert.ok(u6Text(h.root).includes("AC2 · 3턴 실패"), u6Text(h.root));
  const chip = u6All(h.root).find((e) => e.tag === "button");
  await chip.click(); await tick(20);
  const t = u6Text(h.root);
  assert.ok(t.includes("기억 만들기 실패") && t.includes("synthetic error") && t.includes("자동 재시도 끝") && t.includes("리롤로 바뀐 답을 반영했어요") && t.includes("이번 턴 탭"), t);
  assert.ok(!t.includes("17/100") && !t.includes("넣은 기억 1개"), "no memory content on the chat screen");
  const retry = u6All(h.root).find((e) => e.tag === "button" && e.text === "다시 시도");
  assert.ok(retry, "a failed turn offers its retry");
  await retry.click(); await tick(20);
  assert.deepStrictEqual(h.posts.find((p) => p.url.endsWith("/hud/retry")).body, { chat_id: "chat-1", turn: 3 });
  assert.ok(!u6All(h.root).some((e) => e.text === "다시 시도"), "after the retry the turn runs again: no retry button");
  await u6All(h.root).find((e) => e.text === "×").click(); await tick(20);
  assert.strictEqual(h.root.children.length, 0, "closed");
  await h.close();
});

test("turn HUD: the wrapper sits below the right midpoint and its expanded panel scrolls above the chip", async () => {
  const h = await u6Host({ hud: u6View(false) });
  try {
    const wrap = h.root.children[0];
    assert.match(wrap.style, /position:fixed;right:clamp\(0px,calc\(12px - 0px\),calc\(100vw - min\(300px,calc\(100vw - 24px\)\)\)\);bottom:clamp\(0px,calc\(35vh \+ env\(safe-area-inset-bottom,0px\) - 0px\),calc\(100vh - 130px\)\)/);
    assert.ok(wrap.style.includes("flex-direction:column-reverse"));
    assert.ok(wrap.style.includes("width:min(300px,calc(100vw - 24px))"), "narrow (user 11-08)");
    await wrap.children[0].click();
    await tick(20);
    const expanded = h.root.children[0];
    assert.strictEqual(expanded.style, wrap.style);
    const panel = expanded.children[1];
    assert.strictEqual(panel.tag, "div");
    assert.ok(panel.style.includes("max-height:max(80px,min(calc(65vh - 24px),calc(100vh - clamp(0px,calc(35vh + env(safe-area-inset-bottom,0px) - 0px),calc(100vh - 130px)) - 50px)));overflow:auto"), panel.style);
    assert.ok(u6Text(panel).includes("AC2 기억 처리"));
  } finally { await h.close(); }
});

test("turn HUD: a refused page permission leaves requests, the chip and the settings working", async () => {
  const h = await u6Host({ hud: u6View(true), permissionError: true });
  await tick(30);
  assert.strictEqual(h.root.children.length, 0);
  const payload = [{ role: "user", content: "hi" }];
  let out = payload;
  for (const fn of h.host.handlers.beforeRequest) out = await fn(out, "model");
  assert.strictEqual(out, payload);
  for (const fn of h.host.handlers.afterRequest) assert.strictEqual(await fn("reply", "model"), "reply");
  assert.ok(h.settings.some((x) => x.id === "ac2-settings"), "the settings stay registered");
  await h.close();
});
test("turn HUD: an unchanged view keeps the same chip, puts it back when the page took it out, and rebuilds when the state changes", async () => {
  // the host never releases an element it handed out (apiV3 registry), so
  // rebuilding an unchanged chip on every poll piles them up
  const h = await u6Host({ hud: u6View(true) });
  try {
    const chip = h.root.children[0];
    assert.ok(chip && u6Text(chip).includes("만드는 중"));
    for (let i = 0; i < 3; i++) await h.fire();
    assert.strictEqual(h.root.children.length, 1);
    assert.strictEqual(h.root.children[0], chip, "an unchanged view must not build a new chip");
    await chip.remove();
    assert.strictEqual(h.root.children.length, 0);
    await h.fire();
    assert.strictEqual(h.root.children.length, 1, "the chip must come back after the page removed it");
    assert.strictEqual(h.root.children[0], chip);
    h.state.hud = u6View(false);
    await h.fire();
    assert.strictEqual(h.root.children.length, 1);
    assert.notStrictEqual(h.root.children[0], chip, "a changed state draws again");
    assert.ok(u6Text(h.root).includes("개 저장"));
  } finally { await h.close(); }
});
// HUD rebuild: failures show red with what happened and where the retry
// stands; a request that never reached the backend is shown; a retry is
// told while it runs.
test("turn HUD: a memory request that does not reach the backend shows 연결 실패 until one does", async () => {
  const h = await u6Host({ hud: u6View(false) });
  try {
    await tick(20);
    assert.ok(u6Text(h.root).includes("AC2 · 2개 저장"), u6Text(h.root));
    h.state.backendUp = false;
    const payload = [{ role: "user", content: "hi" }];
    let out = payload;
    for (const fn of h.host.handlers.beforeRequest) out = await fn(out, "model");
    await tick(30);
    assert.strictEqual(out, payload, "the request goes on unchanged");
    assert.ok(u6Text(h.root).includes("AC2 · 연결 실패"), u6Text(h.root));
    await u6All(h.root).find((e) => e.tag === "button").click(); await tick(20);
    assert.ok(u6Text(h.root).includes("백엔드 연결 실패") && u6Text(h.root).includes("이 요청은 기억 없이 보냈어요") && u6Text(h.root).includes("연결할 수 없음"), u6Text(h.root));
    h.state.backendUp = true;
    for (const fn of h.host.handlers.beforeRequest) out = await fn(payload, "model");
    await tick(30);
    assert.ok(!u6Text(h.root).includes("연결 실패"), "a request that reached the backend clears it: " + u6Text(h.root));
  } finally { await h.close(); }
});
test("turn HUD: a failed call waiting for its retry is red with the try count and next time; a retry running says so", async () => {
  const view = u6View(false, true);
  view.turns[0].stages[1].state = "retry_waiting";
  Object.assign(view.turns[0], { attempt: 2, attempts: 4, next_at: "2026-10-28T00:05:00Z" });
  view.active = true; // Go follows a turn whose generation has not ended
  const h = await u6Host({ hud: view });
  try {
    await tick(20);
    assert.ok(u6Text(h.root).includes("AC2 · 3턴 실패 · 곧 다시"), u6Text(h.root));
    const [bg] = h.root.children[0].children[0].style.match(/background:rgba\([^)]*\)/) || [""];
    assert.ok(bg.includes("197,48,48"), "red, not the amber of waiting: " + bg);
    await u6All(h.root).find((e) => e.tag === "button").click(); await tick(20);
    assert.ok(u6Text(h.root).includes("4번 중 2번째 실패 · 다음 시도") && u6All(h.root).some((e) => e.text === "다시 시도"), u6Text(h.root));
    const running = u6View(true);
    Object.assign(running.turns[0], { attempt: 2, attempts: 4, retried: true });
    h.state.hud = running;
    await h.fire();
    assert.ok(u6Text(h.root).includes("AC2 · 다시 시도 중 2/4") && u6Text(h.root).includes("4번 중 2번째"), u6Text(h.root));
  } finally { await h.close(); }
});
test("turn HUD: the memory choice of a preprocessing mode is shown, and a fallback says why; no memory text is shown", async () => {
  const view = u6View(false);
  view.turns[0].stages.splice(1, 0, { key: "injected", state: "done", elapsed_ms: 300, count: 11 });
  view.turns[0].injection.preprocess = { mode: "jev", ran: "jev", decision_calls: 3, removed: 2, added: 3, ms: 2400 };
  view.settings.compact = false;
  const h = await u6Host({ hud: view });
  try {
    await tick(20);
    const t = u6Text(h.root);
    assert.ok(t.includes("기억 고르기 2.4초") && t.includes("판정 모델 · 판정 3번 · 뺌 2 · 더함 3") && t.includes("기억 넣음 0.3초") && t.includes("11개"), t);
    assert.ok(!t.includes("17/100"), "no memory content");
    const fb = u6View(false);
    fb.settings.compact = false;
    fb.turns[0].injection.preprocess = { mode: "ai", ran: "go", fallback: "ai_failed", ai_calls: 1, removed: 0, added: 0, ms: 900 };
    h.state.hud = fb;
    await h.close();
    const h2 = await u6Host({ hud: fb });
    await tick(20);
    assert.ok(u6Text(h2.root).includes("AI 못 함 → Go 기본 · AI 실패"), u6Text(h2.root));
    await h2.close();
  } finally { }
});
test("turn HUD: narrative support is told — the guide's level and items, the input's result, a failure as a warning; no text is shown", async () => {
  const view = u6View(false);
  view.settings.compact = false;
  view.turns[0].injection.narrative = { level: "strong", guide: "made", kept: 3, dropped: 1, input: "clarify", input_result: "changed", ai_calls: 2, ms: 3100 };
  const h = await u6Host({ hud: view });
  await tick(20);
  const t = u6Text(h.root);
  assert.ok(t.includes("서사 지원 3.1초") && t.includes("가이드 강하게 · 3개 (근거 없어 뺌 1) · 입력 다듬음"), t);
  await h.close();
  const fb = u6View(false);
  fb.settings.compact = false;
  fb.turns[0].injection.narrative = { level: "weak", guide: "ai_failed", input: "blend", input_result: "ai_off", ai_calls: 1, ms: 400 };
  const h2 = await u6Host({ hud: fb });
  await tick(20);
  const t2 = u6Text(h2.root);
  assert.ok(t2.includes("가이드 약하게 · AI 실패 · 입력 원문 그대로 · 출판사 설정 안 됨 (AI 연결 없음)"), t2);
  await h2.close();
  // one reason for both: told once
  const one = u6View(false);
  one.settings.compact = false;
  one.turns[0].injection.narrative = { level: "weak", guide: "ai_off", input: "clarify", input_result: "ai_off", ai_calls: 0, ms: 10 };
  const h4 = await u6Host({ hud: one });
  await tick(20);
  const t4 = u6Text(h4.root);
  assert.ok(t4.includes("가이드 약하게 · 입력 원문 그대로 · 출판사 설정 안 됨 (AI 연결 없음)") && t4.split("출판사 설정 안 됨").length === 2, t4);
  await h4.close();
  const bl = u6View(false);
  bl.settings.compact = false;
  bl.turns[0].injection.narrative = { level: "weak", guide: "made", kept: 1, input: "blend", input_result: "changed", ai_calls: 1, ms: 400 };
  const h3 = await u6Host({ hud: bl });
  await tick(20);
  const t3 = u6Text(h3.root);
  assert.ok(t3.includes("입력에 기억 녹임") && !t3.includes("입력 다듬음"), t3);
  await h3.close();
});
test("turn HUD: an open panel whose elapsed time alone changes rewrites its lines and makes no new elements", async () => {
  const h = await u6Host({ hud: u6View(true) });
  try {
    await u6All(h.root).find((e) => e.tag === "button").click(); await tick(5);
    const wrap = h.root.children[0];
    assert.ok(u6Text(wrap).includes("1.4초"), u6Text(wrap));
    const made = U6Element.made;
    for (let i = 1; i <= 5; i++) {
      h.state.hud.turns[0].stages[1].elapsed_ms = 1400 + i * 2000;
      await h.fire();
      assert.strictEqual(h.root.children[0], wrap, "the same panel stays");
      assert.ok(u6Text(wrap).includes((1.4 + i * 2).toFixed(1) + "초"), "the new time is shown: " + u6Text(wrap));
    }
    assert.strictEqual(U6Element.made, made, "no element is made for a time change");
    h.state.hud.turns[0].stages[1].state = "done";
    await h.fire();
    assert.notStrictEqual(h.root.children[0], wrap, "a changed state still draws again");
  } finally { await h.close(); }
});
test("settings: a late answer of the tab left behind does not land in the tab now shown", async () => {
  const h = await u6Host({ hud: u6View(false) });
  try {
    const fetchNext = h.host.api.nativeFetch;
    let release;
    const gate = new Promise((r) => { release = r; });
    h.host.api.nativeFetch = async (url, init) => { if (url.includes("/status")) await gate; return fetchNext(url, init); };
    const opened = h.settings.find((x) => x.id === "ac2-settings").fn();
    await tick(20);
    // the status tab is still waiting for its answer; move to this turn
    const turnTab = u6All(h.frame).find((e) => e.tag === "button" && u6Text(e) === "이번 턴");
    assert.ok(turnTab);
    await turnTab.click();
    await tick(30);
    release();
    await opened;
    await tick(30);
    const shown = u6Text(h.frame);
    assert.ok(!shown.includes("최근 활동"), "the status tab's late answer must not show under this turn: " + shown.slice(0, 300));
  } finally { await h.close(); }
});
// failures the HUD did not show
async function u6Send(h, type = "model") {
  const payload = [{ role: "user", content: "synthetic input" }];
  let out = payload;
  for (const fn of h.host.handlers.beforeRequest) out = await fn(out, type);
  await tick(25);
  assert.strictEqual(out, payload, "the request goes on unchanged");
}
test("turn HUD (review 19): a connection failure shows before the chat has a turn", async () => {
  const view = u6View(false); view.turns = [];
  const h = await u6Host({ hud: view, backendUp: false });
  try {
    await u6Send(h);
    assert.ok(u6Text(h.root).includes("AC2 · 연결 실패"), u6Text(h.root));
  } finally { await h.close(); }
});
test("turn HUD (review 19): an auxiliary request passed over by the backend keeps the memory request's failure", async () => {
  const h = await u6Host({ hud: u6View(false) });
  try {
    h.state.backendUp = false; await u6Send(h);
    assert.ok(u6Text(h.root).includes("연결 실패"), u6Text(h.root));
    h.state.backendUp = true; h.state.injectReply = { skipped: "request_type" }; await u6Send(h, "translate");
    assert.ok(u6Text(h.root).includes("연결 실패"), "a skipped auxiliary request clears nothing: " + u6Text(h.root));
  } finally { await h.close(); }
});
test("turn HUD (review 19): another chat's success does not clear this chat's connection failure", async () => {
  const h = await u6Host({ hud: u6View(false) });
  try {
    h.state.backendUp = false; await u6Send(h);
    assert.ok(u6Text(h.root).includes("연결 실패"), u6Text(h.root));
    h.state.rereadChat = { ...h.state.rereadChat, id: "chat-2" };
    h.state.hud = { ...u6View(false), chat: "chat-2" };
    h.state.backendUp = true; await u6Send(h);
    assert.ok(!u6Text(h.root).includes("연결 실패"), "chat 2 reached the backend: " + u6Text(h.root));
    h.state.rereadChat = { ...h.state.rereadChat, id: "chat-1" };
    h.state.hud = u6View(false);
    // back in chat 1 by an auxiliary request the backend passes over
    h.state.injectReply = { skipped: "request_type" }; await u6Send(h, "translate");
    assert.ok(u6Text(h.root).includes("연결 실패"), "chat 1's failure is still told: " + u6Text(h.root));
  } finally { await h.close(); }
});
test("turn HUD (review 19): a failed HUD read is told and read again while a turn was under way", async () => {
  const h = await u6Host({ hud: u6View(true) });
  try {
    const fetchNext = h.host.api.nativeFetch;
    h.host.api.nativeFetch = async (url, init) => url.includes("/hud?")
      ? { ok: false, status: 500, text: async () => JSON.stringify({ code: "store_failed" }) }
      : fetchNext(url, init);
    await h.fire();
    assert.ok(u6Text(h.root).includes("AC2 · 상태 못 읽음"), u6Text(h.root));
    assert.equal(h.pending().length, 1, "it is read again");
    h.host.api.nativeFetch = fetchNext;
    await h.fire();
    assert.ok(!u6Text(h.root).includes("상태 못 읽음"), u6Text(h.root));
  } finally { await h.close(); }
});
test("turn HUD (review 19): an earlier turn's failure outside the latest two is told with its retry", async () => {
  const view = u6View(false);
  view.settings.compact = false;
  view.failures = [{ turn: 1, state: "failed", detail: "provider 503 (응답 본문 제외)", attempt: 4, attempts: 4, next_at: null, retry: true }];
  const h = await u6Host({ hud: view });
  try {
    await tick(20);
    const text = u6Text(h.root);
    assert.ok(text.includes("AC2 · 1턴 실패") && text.includes("1턴 · 이전") && text.includes("4번 중 4번째 · 자동 재시도 끝"), text);
    assert.ok(u6All(h.root).some((e) => e.tag === "button" && e.text === "다시 시도"), text);
  } finally { await h.close(); }
});
test("turn HUD (review 19): an auxiliary request's failure is not told as the memory request's", async () => {
  const h = await u6Host({ hud: u6View(false) });
  try {
    h.state.backendUp = false; await u6Send(h, "translate");
    await u6All(h.root).find((e) => e.tag === "button").click(); await tick(10);
    const text = u6Text(h.root);
    assert.ok(text.includes("연결 실패") && text.includes("보조 요청") && !text.includes("이 요청은 기억 없이 보냈어요"), text);
    await u6Send(h); // the memory request fails too: it is told as such
    assert.ok(u6Text(h.root).includes("이 요청은 기억 없이 보냈어요"), u6Text(h.root));
  } finally { await h.close(); }
});
test("turn HUD (review 19): the same failed HUD read again keeps the open panel's elements", async () => {
  const v = u6View(true); v.settings.compact = false;
  const h = await u6Host({ hud: v });
  try {
    const original = h.host.api.nativeFetch;
    h.host.api.nativeFetch = async (url, init) => url.includes("/hud?") ? { ok: false, status: 500, text: async () => JSON.stringify({ code: "store_failed" }) } : original(url, init);
    await h.fire();
    assert.ok(u6Text(h.root).includes("상태 못 읽음"), u6Text(h.root));
    const made = U6Element.made;
    await h.fire(); await h.fire();
    assert.equal(U6Element.made, made, "no element made again");
    assert.equal(h.pending().length, 1);
  } finally { await h.close(); }
});
test("turn HUD (review 19): a retry is told on the chip from its first call", async () => {
  const v = u6View(true); Object.assign(v.turns[0], { attempt: 1, attempts: 1, retried: true });
  const h = await u6Host({ hud: v });
  try { assert.ok(u6Text(h.root).includes("AC2 · 다시 시도 중 1/1"), u6Text(h.root)); } finally { await h.close(); }
});

// errors and logs are not hidden; only credential values are.
test("errors: a code without a text of its own shows the backend's code and message; only credential values are masked", () => {
  const a = SRC.indexOf("  function errText(res) {"), b = SRC.indexOf("  function el(tag, attrs", a);
  const m = SRC.indexOf("  function maskCredentials(text, token) {"), n = SRC.indexOf("\n  }", m) + 4;
  assert.ok(a > 0 && b > a && m > 0 && n > m);
  const ctx = vm.createContext({ ERR_TEXT: { known_code: "아는 오류예요." } });
  vm.runInContext(SRC.slice(a, b) + SRC.slice(m, n) + "\nglobalThis.errText = errText; globalThis.maskCredentials = maskCredentials;", ctx);
  assert.strictEqual(ctx.errText({ ok: false, status: 400, code: "known_code", json: { detail: "x" } }), "아는 오류예요.");
  const shown = ctx.errText({ ok: false, status: 400, code: "weird_code", json: { code: "weird_code", detail: "model field is required in task-queue-12345678" } });
  assert.ok(shown.includes("400 · weird_code") && shown.includes("model field is required in task-queue-12345678"), shown);
  const keyed = ctx.errText({ ok: false, status: 401, code: "provider_failed", json: { message: "Incorrect API key provided: sk-abcdEFGH12345678." } });
  assert.ok(keyed.includes("Incorrect API key provided: [접속 정보 가림].") && !keyed.includes("abcdEFGH"), keyed);
  assert.strictEqual(ctx.errText({ ok: false, status: 500, kind: "http_error" }), "요청이 실패했어요 (500).");
  assert.strictEqual(ctx.maskCredentials("read disk-usage-report.log and risk-assessment-2026: denied", ""), "read disk-usage-report.log and risk-assessment-2026: denied");
});
