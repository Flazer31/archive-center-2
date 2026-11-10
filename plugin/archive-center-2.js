//@name archive_center_2
//@display-name Archive Center 2 1.0.0
//@api 3.0
//@version 1.0.0
//@arg ac2_url string Backend URL until this device saves its own in the settings screen (default http://127.0.0.1:28190)
//@arg ac2_token string Access token until this device saves its own (token.txt in the backend's shared data directory)
//@arg ac2_hash_only int 1 = send only hash and length (no memories can be made), 0 = send message text (default)

// A bridge to the Go backend: it observes and records, and puts the memory
// the backend chooses into the request. It never changes the response or
// the chat. Decisions belong to the backend.
(async () => {
  const PLUGIN_VERSION = "1.0.0";
  const CONTRACT = "observation.v1";
  const FETCH_TIMEOUT_MS = 8000;
  const api = Risuai;
  let runtime = null; // only the runtime information already observed at load
  const pluginErrors = [];
  function recordPluginError(error) {
    pluginErrors.push({ message: String(error && (error.stack || error.message) || error).slice(0, 8000), time: new Date().toISOString() });
    if (pluginErrors.length > 40) pluginErrors.splice(0, pluginErrors.length - 40);
  }

  // Each plugin load gets a boot id; (boot_id, seq) orders observations.
  const BOOT_ID = Date.now().toString(36) + "-" + Math.random().toString(36).slice(2, 8);
  let OUTBOX_KEY = "ac2_outbox_v1";
  const OUTBOX_MAX = 2000;
  // a request carries at most this much JSON (the backend accepts 256 MiB);
  // a single larger event still goes alone
  const BATCH_BYTES = 8 << 20;
  const BATCH_EVENTS = 200;
  const RETRY_MS = 10000;
  let seq = 0;
  let queue = [];
  let dropped = 0;
  let flushing = false;
  let inflight = new Set(); // ids of the batch being sent
  // ids sent at least once and not yet acknowledged: their fate is unknown
  // (a timed-out request may still be applied), so overflow never drops them
  const attempted = new Set();
  let retryTimer = null;
  let unloaded = false;
  const cleanups = [];
  // the turn HUD's two entry points, set by hudStart (declared here: the
  // hooks may run before the HUD's part of the file)
  let hudSee = () => {};
  let hudSettingsChanged = () => {};
  // the last memory request's reach to the backend (host-local: only the
  // host sees a request that never arrived); the HUD shows a failed one
  let hudLink = () => {};
  let hudChat = null; // the chat the last request was for

  // Device-local storage (not the save file), so unsent observations survive
  // a reload without causing save-file writes on the host.
  let store = null;
  try { store = await api.getLocalPluginStorage(); } catch (_) { store = null; }
  // PocketRisu keeps plugin storage on its server, shared by every device
  // on it (pluginSafeClass.ts:101-161): the outbox key names this device,
  // whose id lives in the browser (safeLocalStorage is localStorage).
  try {
    let device = await api.safeLocalStorage.getItem("ac2_device_id");
    if (!device) {
      device = Date.now().toString(36) + "-" + Math.random().toString(36).slice(2, 10);
      await api.safeLocalStorage.setItem("ac2_device_id", device);
    }
    OUTBOX_KEY += ":" + device;
  } catch (_) {}

  // Saves run one after another (an older save never lands last) and the
  // hooks do not wait for them, so slow storage never holds the host.
  let saving = Promise.resolve();
  function persistOutbox() {
    if (!store) return saving;
    saving = saving.then(async () => {
      try { await store.setItem(OUTBOX_KEY, { queue, dropped, attempted: [...attempted] }); } catch (_) {}
    });
    return saving;
  }
  const eventId = (e) => e.boot_id + ":" + e.seq;

  async function restoreOutbox() {
    if (!store) return;
    try {
      const saved = await store.getItem(OUTBOX_KEY);
      if (saved && Array.isArray(saved.queue)) {
        queue = saved.queue.concat(queue);
        dropped += Number(saved.dropped || 0);
        // sent before the reload with unknown fate: still protected
        const ids = new Set(saved.queue.map(eventId));
        for (const id of Array.isArray(saved.attempted) ? saved.attempted : []) if (ids.has(id)) attempted.add(id);
      }
    } catch (_) {}
  }

  // Restore before any hook can record, so a new record never overwrites
  // the saved outbox.
  await restoreOutbox();

  async function arg(key, fallback) {
    try {
      const v = await api.getArgument(key);
      return v === undefined || v === null || v === "" ? fallback : v;
    } catch (_) {
      return fallback;
    }
  }

  // The address is kept per device (each device may reach the backend by
  // another address). The access token is the host's plugin storage, which
  // PocketRisu keeps on its server for every device on it (user 10-28: a
  // phone on the same PocketRisu must not be asked again); this device's own
  // copy and the plugin arguments serve when it has none. The backend checks
  // the token on every request.
  const CONN_KEY = "ac2_conn_v1";
  const SHARED_TOKEN_KEY = "ac2_token_v1";
  const TOKEN_HEADER = "X-AC2-Token";
  let connCache = null;
  async function conn() {
    if (connCache) return connCache;
    let saved = null, shared = null;
    try { saved = JSON.parse((await api.safeLocalStorage.getItem(CONN_KEY)) || "null"); } catch (_) { saved = null; }
    try { shared = store ? await store.getItem(SHARED_TOKEN_KEY) : null; } catch (_) { shared = null; }
    connCache = {
      url: String((saved && saved.url) || (await arg("ac2_url", "http://127.0.0.1:28190"))).replace(/\/+$/, ""),
      token: String((typeof shared === "string" ? shared : null) ?? ((saved && saved.token) || (await arg("ac2_token", "")))),
    };
    // a token this device kept before the shared one existed is shared once
    if (typeof shared !== "string" && saved && saved.token && store) try { await store.setItem(SHARED_TOKEN_KEY, saved.token); } catch (_) {}
    return connCache;
  }
  // Returns whether it was kept; the values are used for this session
  // either way.
  async function saveConn(next) {
    connCache = { url: String(next.url || "").trim().replace(/\/+$/, ""), token: String(next.token || "").trim() };
    let kept = false;
    try { if (store) { await store.setItem(SHARED_TOKEN_KEY, connCache.token); kept = true; } } catch (_) {}
    try {
      await api.safeLocalStorage.setItem(CONN_KEY, JSON.stringify(connCache));
      return true;
    } catch (_) {
      return kept;
    }
  }

  // Short content fingerprint for comparing rows across hooks without sending text.
  // Text as the backend will read it: a lone surrogate cannot cross JSON
  // intact (it arrives as U+FFFD), so it is replaced here first.
  function wellFormed(text) {
    const s = typeof text === "string" ? text : JSON.stringify(text ?? "");
    if (typeof s.isWellFormed === "function" && s.isWellFormed()) return s;
    return s.replace(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/g, "\uFFFD");
  }

  // 64-bit fingerprint (djb2 and FNV-1a over UTF-16 code units); the Go
  // backend computes the same (ingest.Fingerprint).
  function fingerprint(text) {
    const s = wellFormed(text);
    let a = 5381;
    let b = 0x811c9dc5;
    for (let i = 0; i < s.length; i++) {
      const c = s.charCodeAt(i);
      a = (Math.imul(a, 33) + c) >>> 0;
      b = Math.imul(b ^ c, 0x01000193) >>> 0;
    }
    return { len: s.length, h: a.toString(16).padStart(8, "0") + b.toString(16).padStart(8, "0") };
  }

  // The capture setting is read once per hook, not per row (each host call
  // is a round trip out of the plugin sandbox).
  // the hosts set an int argument to 0 until the user changes it, so 0 is
  // the default: the text is sent
  async function captureText() {
    return Number(await arg("ac2_hash_only", 0)) !== 1;
  }

  function textField(text, capture) {
    const fp = fingerprint(text);
    return capture ? { ...fp, text: wellFormed(text) } : fp;
  }

  // UTF-8 size of a string (the request size limit is in bytes).
  function utf8Bytes(s) {
    if (typeof TextEncoder === "function") return new TextEncoder().encode(s).length;
    let n = 0;
    for (let i = 0; i < s.length; i++) {
      const c = s.charCodeAt(i);
      if (c < 0x80) n += 1;
      else if (c < 0x800) n += 2;
      else if (c >= 0xd800 && c < 0xdc00 && i + 1 < s.length) { n += 4; i++; }
      else n += 3;
    }
    return n;
  }

  // A chat row's (or translation record's) text is sent only when it differs
  // from the text the backend last acknowledged for that row (compared
  // exactly); otherwise only the fingerprint goes. The backend keeps the texts
  // it received and asks for a full resend when it does not have one
  // (resend_text).
  const lastSent = new Map(); // "<chat>|<row or var>" -> { raw, len, h } acknowledged
  const pendingSent = new Map(); // event id -> [[key, { raw, len, h }]]
  // Acknowledged texts are kept for the current and the previous chat only;
  // a chat visited again sends its texts once more.
  let recentChats = [];
  function keepTextsOf(chatKey) {
    if (recentChats[0] === chatKey) return;
    recentChats = [chatKey, recentChats[0]].filter((c) => c !== undefined);
    for (const k of lastSent.keys()) {
      if (!recentChats.includes(k.slice(0, k.lastIndexOf("|")))) lastSent.delete(k);
    }
  }

  function chatText(key, text, pending, capture) {
    const raw = typeof text === "string" ? text : JSON.stringify(text ?? "");
    const last = lastSent.get(key);
    // unchanged since acknowledged: the stored fingerprint, no rehash
    if (capture && last && last.raw === raw) return { len: last.len, h: last.h };
    // already carried by this same event (event chat and re-read): once
    const carried = pending.find(([k, r]) => k === key && r.raw === raw);
    if (capture && carried) return { len: carried[1].len, h: carried[1].h };
    const field = textField(raw, capture);
    if (field.text !== undefined) pending.push([key, { raw, len: field.len, h: field.h }]);
    return field;
  }

  async function coords() {
    const out = {};
    try { out.character_index = await api.getCurrentCharacterIndex(); } catch (e) { recordPluginError(e); out.character_index_error = String(e); }
    try { out.chat_index = await api.getCurrentChatIndex(); } catch (e) { recordPluginError(e); out.chat_index_error = String(e); }
    return out;
  }

  // Stored chat snapshot, read now by coordinates (not an event snapshot).
  async function chatSnapshot(charIndex, chatIndex, pending, capture) {
    let chat;
    try {
      chat = await api.getChatFromIndex(charIndex, chatIndex);
    } catch (e) { recordPluginError(e);
      return { error: String(e) };
    }
    return snapshotFromChat(chat, pending, capture, charIndex);
  }

  // The character's name for the chat list, read only for the open
  // character ({{char}} names that one); null when it cannot be read.
  async function charNameAt(charIndex) {
    try {
      if (charIndex === undefined || charIndex !== await api.getCurrentCharacterIndex()) return null;
      const v = await api.parseRisuChat("{{char}}", { runVar: false, processRegex: false });
      return typeof v === "string" && v ? v : null;
    } catch (_) {
      return null;
    }
  }

  // Chat variable values are sent like row texts (only when changed); the
  // backend reads translation originals (Yumi records) and date values from
  // them.
  const TRANSLATION_VAR_PREFIX = "$__yumi_tr.";

  // Every row with its observable fields; all fields are optional on the host.
  async function snapshotFromChat(chat, pending, capture, charIndex) {
    if (!chat) return { missing: true };
    const rows = [];
    const messages = Array.isArray(chat.message) ? chat.message : [];
    const chatKey = String(chat.id ?? "");
    keepTextsOf(chatKey);
    for (let i = 0; i < messages.length; i++) {
      const m = messages[i] || {};
      rows.push({
        i,
        role: m.role,
        chat_id: m.chatId ?? null,
        time: m.time ?? null,
        disabled: m.disabled ?? null,
        is_comment: m.isComment ?? null,
        name: m.name ?? null,
        saying: m.saying ?? null,
        other_user: m.otherUser ?? null,
        generation_info: m.generationInfo ? Object.keys(m.generationInfo) : null,
        generation_id: m.generationInfo && m.generationInfo.generationId ? m.generationInfo.generationId : null,
        generation_info_values: m.generationInfo ? fingerprint(m.generationInfo) : null,
        prompt_info: m.promptInfo ? m.promptInfo : null,
        data: await chatText(chatKey + "|" + (m.chatId ? "c:" + m.chatId : "i:" + i), m.data, pending, capture),
      });
    }
    const vars = {};
    const ss = chat.scriptstate || {};
    for (const k of Object.keys(ss)) {
      vars[k] = { type: typeof ss[k], ...(k.startsWith(TRANSLATION_VAR_PREFIX) ? { translation_record: true } : {}), ...chatText(chatKey + "|v:" + k, ss[k], pending, capture) };
    }
    const gl = chat.GLGlobalVariables || {};
    const glVars = {};
    for (const k of Object.keys(gl)) glVars[k] = fingerprint(gl[k]);
    return {
      chat_id: chat.id ?? null,
      name: typeof chat.name === "string" ? chat.name : null,
      char_name: await charNameAt(charIndex),
      fm_index: chat.fmIndex ?? null,
      binded_persona: chat.bindedPersona ?? null,
      is_streaming: chat.isStreaming ?? null,
      last_date: chat.lastDate ?? null,
      modules: chat.modules ?? null,
      chat_keys: Object.keys(chat),
      message_count: messages.length,
      rows,
      scriptstate: vars,
      gl_global_variables: glVars,
    };
  }

  // Official parser reads (current active chat at call time, not a fixed coordinate).
  const PARSER_PROBES = ["{{user}}", "{{getglobalvar::toggle_lang}}"];
  async function parserReads(capture) {
    const out = {};
    for (const expr of PARSER_PROBES) {
      try {
        const v = await api.parseRisuChat(expr, { runVar: false, processRegex: false });
        out[expr] = textField(v, capture);
      } catch (e) { recordPluginError(e);
        out[expr] = { error: String(e) };
      }
    }
    return out;
  }

  async function record(hook, extra, pending) {
    seq += 1;
    const ev = { boot_id: BOOT_ID, seq, hook, host_time_ms: Date.now(), plugin_version: PLUGIN_VERSION, ...extra };
    if (pending && pending.length) pendingSent.set(eventId(ev), pending);
    queue.push(ev);
    if (queue.length > OUTBOX_MAX) {
      // drop the oldest events not sent yet; the gap is reported as a count.
      // Output events go last: a later snapshot shows the chat again, but an
      // output is the only signal that a reply completed.
      let excess = queue.length - OUTBOX_MAX;
      for (const keepOutputs of [true, false]) {
        queue = queue.filter((e) => {
          if (excess > 0 && !attempted.has(eventId(e)) && !(keepOutputs && e.hook === "output")) {
            excess--;
            dropped++;
            pendingSent.delete(eventId(e));
            return false;
          }
          return true;
        });
      }
    }
    persistOutbox();
    flush();
    return ev;
  }

  // over: an address and token to try before saving them (settings screen)
  async function bridge(path, init, over) {
    const c = over || (await conn());
    const base = c.url;
    const { timeoutMs = FETCH_TIMEOUT_MS, ...request } = init || {};
    const ctrl = typeof AbortController === "function" ? new AbortController() : null;
    let timer;
    const timeout = timeoutMs ? new Promise((_, reject) => {
      timer = setTimeout(() => { if (ctrl) ctrl.abort(); reject(new Error("timeout")); }, timeoutMs);
    }) : null;
    try {
      const run = (async () => {
        let res;
        try {
          res = await api.nativeFetch(base + path, {
            ...request,
            headers: { "Content-Type": "application/json", [TOKEN_HEADER]: c.token, ...request.headers },
            signal: ctrl ? ctrl.signal : undefined,
          });
        } catch (e) {
          recordPluginError(e);
          return { ok: false, kind: "connect_failed", detail: String(e) };
        }
        let text;
        try { text = await res.text(); } catch (e) { recordPluginError(e); return { ok: false, kind: "body_read_failed", status: res.status, detail: String(e) }; }
        let json = null;
        try { json = text ? JSON.parse(text) : null; } catch (_) {
          return { ok: false, kind: "decode_failed", status: res.status };
        }
        if (!res.ok) return { ok: false, kind: "http_error", status: res.status, code: json && json.code, json };
        return { ok: true, status: res.status, json };
      })();
      return await (timeout ? Promise.race([run, timeout]) : run);
    } catch (e) {
      recordPluginError(e);
      return { ok: false, kind: "timeout", detail: String(e) };
    } finally {
      clearTimeout(timer);
    }
  }

  // Sending never blocks or alters the host flow. Unsent observations stay
  // in order in the outbox and are retried; the host output is never held.
  async function flush() {
    if (flushing || queue.length === 0) return;
    flushing = true;
    const gap = dropped;
    const envelope = (events) => ({ contract: CONTRACT, plugin_version: PLUGIN_VERSION, dropped_before: gap, events });
    const batch = [];
    let bytes = utf8Bytes(JSON.stringify(envelope([]))); // the request around the events
    for (const e of queue) {
      const size = utf8Bytes(JSON.stringify(e)) + (batch.length ? 1 : 0);
      if (batch.length && (batch.length >= BATCH_EVENTS || bytes + size > BATCH_BYTES)) break;
      batch.push(e);
      bytes += size;
    }
    inflight = new Set(batch.map(eventId));
    for (const id of inflight) attempted.add(id);
    // the mark is saved before the request leaves (this runs in the
    // background, not in a hook), so a reload never forgets a sent event
    await persistOutbox();
    const res = await bridge("/observe", {
      method: "POST",
      body: JSON.stringify(envelope(batch)),
    });
    if (res.ok) {
      // remove exactly the events that were sent (the queue may have
      // changed while sending)
      queue = queue.filter((e) => !inflight.has(eventId(e)));
      dropped -= gap;
      if (res.json && res.json.resend_text) {
        lastSent.clear();
      } else {
        for (const id of inflight) {
          // only for chats still kept (an acknowledgement may come after
          // the user moved on)
          for (const [k, t] of pendingSent.get(id) || []) {
            if (recentChats.includes(k.slice(0, k.lastIndexOf("|")))) lastSent.set(k, t);
          }
        }
      }
      for (const id of inflight) {
        pendingSent.delete(id);
        attempted.delete(id);
      }
      persistOutbox();
    } else {
      console.warn("[AC2] observe send failed", res);
    }
    inflight = new Set();
    flushing = false;
    // a reply observed may start a turn's generation: the chip asks again
    // once the backend has applied it (the chip is set up later in the load)
    if (res.ok && batch.some((e) => e.hook === "output")) { try { refreshChip(); } catch (_) {} }
    if (queue.length) {
      if (res.ok) flush();
      else if (!retryTimer && !unloaded) retryTimer = setTimeout(() => { retryTimer = null; flush(); }, RETRY_MS);
    }
  }

  // ---- hooks ----

  const onInput = async (content) => {
    const c = await coords();
    await record("input", { coords: c, content: textField(content, await captureText()) });
    return content;
  };

  // The backend chooses the memory of this request (step 7); it goes in as
  // one system message before the latest user message. Without an answer
  // (backend down, timeout, nothing chosen) the request goes on unchanged.
  // The backend's memory text opens with its title and the fingerprint of
  // the text after it; a request handed back to the hook again (the host
  // copies it across the sandbox) keeps one, not two. A message whose
  // fingerprint does not match is not this plugin's and is never taken out.
  const BLOCK_TITLE = "[Archive Center: records of earlier turns of this story | ";
  const GUIDE_TITLE = "[Archive Center: narrative guide | "; // the backend's narrative guide (E03)
  const pdfRequests = new Map();
  let lastAppliedPDF = null;
  let pdfSequence = 0;
  let pdfHook = null;
  const PDF_TOKEN = /(\[Archive Center: records of earlier turns of this story \| [0-9a-f]{16}) \| ac2-pdf:([a-z0-9-]+)(\]\n)/g;
  function isDelivered(m) {
    if (!m || m.role !== "system" || typeof m.content !== "string") return false;
    let text = m.content;
    if (text === "<pm-pdf>\n\n</pm-pdf>") return true;
    if (text.startsWith("<pm-pdf>\n") && text.endsWith("\n</pm-pdf>")) text = text.slice(9, -10);
    const title = text.startsWith(BLOCK_TITLE) ? BLOCK_TITLE : text.startsWith(GUIDE_TITLE) ? GUIDE_TITLE : "";
    if (!title) return false;
    const nl = text.indexOf("\n");
    if (nl < 1 || text[nl - 1] !== "]") return false;
    return fingerprint(text.slice(nl + 1)).h === text.slice(title.length, nl - 1).replace(/ \| ac2-pdf:[a-z0-9-]+$/, "");
  }

  // Only the provider body exposes the final parts (Google, or OpenAI-compatible
  // for llm_gateway_pdf). The header token
  // correlates it without using a chat/turn identity or the latest request.
  function onPDFBody(body, type) {
    if (String(type).startsWith("meta_")) return body;
    let value;
    try { value = JSON.parse(typeof body === "string" ? body : JSON.stringify(body)); }
    catch (_) { console.warn("[AC2] PDF body decode failed; text retained"); return body; }
    if (!value || typeof value !== "object") return body;
    const fields = [];
    const parts = (owner) => {
      if (!owner) return;
      if (typeof owner.parts === "string") fields.push([owner, "parts"]);
      else if (Array.isArray(owner.parts)) for (const part of owner.parts) if (part && typeof part.text === "string") fields.push([part, "text"]);
    };
    parts(value.systemInstruction);
    const contents = Array.isArray(value.contents) ? value.contents : [];
    for (const content of contents) parts(content);
    // E11 the OpenAI-compatible form (AC1's LLM Gateway PDF): messages whose
    // content is a text or a list of parts
    const messages = Array.isArray(value.messages) ? value.messages : [];
    for (const m of messages) {
      if (!m) continue;
      if (typeof m.content === "string") fields.push([m, "content"]);
      else if (Array.isArray(m.content)) for (const part of m.content) if (part && typeof part.text === "string") fields.push([part, "text"]);
    }
    const ids = new Set();
    const allFields = [];
    const collect = (node) => {
      if (!node || typeof node !== "object") return;
      for (const key of Object.keys(node)) {
        if (typeof node[key] === "string") {
          allFields.push([node, key]);
          for (const match of node[key].matchAll(PDF_TOKEN)) ids.add(match[2]);
        } else collect(node[key]);
      }
    };
    collect(value);
    if (!ids.size) return body;
    const geminiRoute = type === "gemini_base" || type === "gemini_base_stream";
    const openaiRoute = type === "openai_basic" || type === "openai_streaming";
    for (const id of ids) {
      const transport = pdfRequests.get(id) || (lastAppliedPDF && lastAppliedPDF.id === id ? lastAppliedPDF.transport : null);
      pdfRequests.delete(id);
      const openai = !!transport && transport.mode === "llm_gateway_pdf";
      const route = openai ? openaiRoute : geminiRoute;
      const user = [...(openai ? messages : contents)].reverse().find((c) => c && c.role === "user");
      if (!transport || !route || !user) {
        console.warn("[AC2] PDF text retained:", !transport ? "request unavailable" : !route ? "unsupported route" : "current user absent");
        for (const [owner, key] of allFields) owner[key] = transport
          ? owner[key].split(transport.markedText).join(transport.text)
          : owner[key].replace(PDF_TOKEN, (whole, header, token, end) => token === id ? header + end : whole);
        continue;
      }
      let found = false;
      for (const [owner, key] of fields) {
        if (owner[key].includes(transport.markedText)) { owner[key] = owner[key].split(transport.markedText).join(""); found = true; }
      }
      if (!found) {
        console.warn("[AC2] PDF text retained: selected text absent");
        for (const [owner, key] of allFields) owner[key] = owner[key].split(transport.markedText).join(transport.text);
        continue;
      }
      if (openai) {
        const fileData = "data:application/pdf;base64," + transport.pdf_base64;
        if (typeof user.content === "string") user.content = [{ type: "text", text: user.content }];
        if (!Array.isArray(user.content)) user.content = [];
        user.content = user.content.filter((p) => !(p && p.type === "file" && p.file && p.file.file_data === fileData));
        user.content.push({ type: "file", file: { filename: "archive-center-memory.pdf", file_data: fileData } });
      } else {
        if (typeof user.parts === "string") user.parts = [{ text: user.parts }];
        if (!Array.isArray(user.parts)) user.parts = [];
        user.parts = user.parts.filter((p) => !(p && p.inlineData && p.inlineData.mimeType === "application/pdf" && p.inlineData.data === transport.pdf_base64));
        user.parts.push({ inlineData: { mimeType: "application/pdf", data: transport.pdf_base64 } });
      }
      // Body callbacks have no request ID. Keep only the last applied exact
      // marker for raw-body retries, independently of completed-turn identity.
      lastAppliedPDF = { id, transport };
      console.info("[AC2] PDF transport", { mode: transport.mode, status: "applied", page_count: transport.page_count, pdf_bytes: transport.pdf_bytes, pdf_sha256: transport.pdf_sha256 });
    }
    return typeof body === "string" ? JSON.stringify(value) : value;
  }

  // The unsent observations up to ev, as one batch within the send limits
  // (null when ev was sent already or they do not fit one batch).
  function unsentThrough(ev) {
    const at = ev ? queue.findIndex((e) => eventId(e) === eventId(ev)) : -1;
    if (at < 0 || at >= BATCH_EVENTS) return null;
    const events = queue.slice(0, at + 1);
    const batch = { contract: CONTRACT, plugin_version: PLUGIN_VERSION, dropped_before: 0, events: [] };
    let bytes = utf8Bytes(JSON.stringify(batch)); // the batch around the events, as flush counts it
    for (let i = 0; i < events.length; i++) {
      bytes += utf8Bytes(JSON.stringify(events[i])) + (i ? 1 : 0);
      if (bytes > BATCH_BYTES) return null;
    }
    batch.events = events;
    return batch;
  }

  // The anchor marker the backend places memory after (its setting, told
  // with each delivery and by the budget tab); each message is sent as
  // whether it holds it, never as text. Until told (the first request after
  // loading) no message is marked and the backend's fallback applies.
  let placementAnchor = "";
  let lastRewrite = null; // the last input rewrite applied here: { from, to, result }
  async function deliver(messages, chatId, shape, capture, ev, type) {
    if (!Array.isArray(messages) || chatId === null || chatId === undefined) return messages;
    const anchor = placementAnchor;
    let at = messages.length;
    let input = "";
    for (let i = messages.length - 1; i >= 0; i--) {
      if (messages[i] && messages[i].role === "user") {
        at = i;
        input = typeof messages[i].content === "string" ? messages[i].content : "";
        break;
      }
    }
    const res = await bridge("/memory/inject", {
      method: "POST",
      timeoutMs: 100000, // a preprocessing mode may take up to 90 s in the backend (review 18 B-01)
      // this request's observation goes along, after the older ones still
      // unsent and in their order (the backend stores them in plugin order):
      // the backend applies them before choosing, so the choice reads the
      // chat as it is now
      // with text capture on, each message's text goes along: the backend
      // reads in it whether a recent turn's text reached the request whole
      // (a host regex can cut a reply on the way); it is not stored
      body: JSON.stringify({ chat_id: String(chatId), type: type ?? "", input: capture ? wellFormed(input) : "",
        payload: shape.map((m) => ({ ...m.content, role: m.role, ...(capture && messages[m.i] && typeof messages[m.i].content === "string" ? { text: wellFormed(messages[m.i].content) } : {}) })), observation: unsentThrough(ev),
        placement_messages: messages.map((m) => ({ role: m && m.role, marker: !!anchor && !!m && typeof m.content === "string" && m.content.includes(anchor), cachePoint: m && m.cachePoint, cache_control: m && m.cache_control, isCache: m && m.isCache })) }),
    });
    hudLink(chatId, res, type != null && type !== "" && type !== "model");
    if (res.ok && res.json && typeof res.json.anchor === "string") placementAnchor = res.json.anchor;
    if (res.ok && res.json && res.json.resend_text) lastSent.clear();
    const block = res.ok && res.json && typeof res.json.block === "string" ? res.json.block : "";
    const transport = res.ok && res.json && res.json.transport;
    if (transport) console.info("[AC2] memory transport", {
      mode: transport.mode, status: transport.status, original_sha256: transport.original_sha256,
      pdf_text_sha256: transport.pdf_text_sha256, pdf_sha256: transport.pdf_sha256,
      page_count: transport.page_count, pdf_bytes: transport.pdf_bytes, base64_chars: transport.base64_chars,
      cache_hit: transport.cache_hit,
    });
    // narrative support (E03): the backend's improved input replaces the
    // user's text in this request only (the chat is unchanged), and its guide
    // goes after the memory and, at the strong level, last
    const rw = res.ok && res.json && res.json.input_rewrite;
    const guide = (res.ok && res.json && res.json.guide) || {};
    // only this turn's user messages (after the last reply) are looked at, the
    // one that is the input itself first; a request handed back already holds
    // the improved text and is left as it is (review 20 B-01, B-02)
    if (rw && typeof rw.from === "string" && rw.from && typeof rw.to === "string") {
      let after = messages.length;
      while (after > 0 && !(messages[after - 1] && messages[after - 1].role === "assistant")) after--;
      const turn = [];
      for (let i = after; i < messages.length; i++) if (messages[i] && messages[i].role === "user" && typeof messages[i].content === "string") turn.push(i);
      const i = turn.find((j) => messages[j].content === rw.from) ?? [...turn].reverse().find((j) => messages[j].content.includes(rw.from));
      // already applied: a message holds `to` (when `to` is a part of `from`, holding `from` is no sign of that)
      const part = !!rw.to && rw.from.includes(rw.to);
      const mine = lastRewrite && lastRewrite.from === rw.from && lastRewrite.to === rw.to; // handed back: a message is this plugin's own result
      if (i !== undefined && !(mine && turn.some((j) => messages[j].content === lastRewrite.result)) && !turn.some((j) => messages[j].content.includes(rw.to) && !(part && messages[j].content.includes(rw.from)))) {
        const m = messages[i], k = m.content.lastIndexOf(rw.from);
        messages = messages.slice();
        messages[i] = { ...m, content: m.content.slice(0, k) + rw.to + m.content.slice(k + rw.from.length) };
        lastRewrite = { from: rw.from, to: rw.to, result: messages[i].content };
      }
    }
    const tail = typeof guide.before_reply === "string" && guide.before_reply ? [{ role: "system", content: guide.before_reply }] : [];
    if (!block && !(transport && transport.mode === "provider_manager_pdf" && transport.status === "delegated")) {
      let out = messages;
      if (typeof guide.after_memory === "string" && guide.after_memory) {
        out = messages.slice();
        out.splice(res.json.placement ? res.json.placement.index : at, 0, { role: "system", content: guide.after_memory });
      }
      return tail.length ? out.concat(tail) : out;
    }
    let content = block;
    let retained = "";
    if (transport && transport.mode === "provider_manager_pdf" && transport.status === "delegated") {
      content = "<pm-pdf>\n" + transport.pdf_text + "\n</pm-pdf>";
      retained = transport.retained_text || "";
    }
    else if (transport && (transport.mode === "google_pdf" || transport.mode === "llm_gateway_pdf")) {
      if (transport.status === "ready" && pdfHook) {
        const id = BOOT_ID + "-" + (++pdfSequence).toString(36);
        const text = transport.pdf_text;
        const nl = text.indexOf("\n");
        content = text.slice(0, nl - 1) + " | ac2-pdf:" + id + text.slice(nl - 1);
        retained = transport.retained_text || "";
        pdfRequests.set(id, { ...transport, text, markedText: content });
      } else if (transport.status !== "text_only") console.warn("[AC2] PDF text retained:", transport.error || "body hook unavailable");
    }
    const out = messages.slice();
    out.splice(res.json.placement ? res.json.placement.index : at, 0, { role: "system", content }, ...(retained ? [{ role: "system", content: retained }] : []),
      ...(typeof guide.after_memory === "string" && guide.after_memory ? [{ role: "system", content: guide.after_memory }] : []));
    return out.concat(tail);
  }

  const onBeforeRequest = async (messages, type) => {
    if (Array.isArray(messages) && messages.some(isDelivered)) messages = messages.filter((m) => {
      if (!isDelivered(m)) return true;
      const header = m.content.slice(0, m.content.indexOf("\n") + 1);
      for (const match of header.matchAll(PDF_TOKEN)) pdfRequests.delete(match[2]);
      return false;
    });
    let ev = null;
    let chatId = null;
    let shape = [];
    let capture = false;
    try {
      const c = await coords();
      for (let i = 0; i < (messages || []).length; i++) {
        const m = messages[i] || {};
        // the prompt itself is not stored; its shape and fingerprints are enough here
        shape.push({ i, role: m.role, name: m.name ?? null, keys: Object.keys(m), content: fingerprint(m.content) });
      }
      const pending = [];
      capture = await captureText();
      let chat;
      let stored;
      try {
        chat = await api.getChatFromIndex(c.character_index, c.chat_index);
        stored = await snapshotFromChat(chat, pending, capture, c.character_index);
      } catch (e) { recordPluginError(e);
        stored = { error: String(e) };
      }
      chatId = chat ? chat.id ?? null : null;
      hudChat = chatId;
      ev = await record("beforeRequest", {
        type: type ?? null,
        coords: c,
        payload: { is_array: Array.isArray(messages), length: (messages || []).length, messages: shape },
        stored_chat: stored,
        parser_reads: await parserReads(capture),
      }, pending);
    } catch (e) { recordPluginError(e);
      console.warn("[AC2] beforeRequest observe failed", e);
    }
    try {
      return await deliver(messages, chatId, shape, capture, ev, type);
    } catch (e) { recordPluginError(e);
      console.warn("[AC2] memory delivery failed", e);
      return messages;
    } finally {
      refreshChip(); // the request may have started a turn's generation
      hudSee(chatId);
    }
  };

  const onAfterRequest = async (content, type) => {
    try {
      const c = await coords();
      await record("afterRequest", { type: type ?? null, coords: c, content: textField(content, await captureText()) });
    } catch (e) { recordPluginError(e);
      console.warn("[AC2] afterRequest observe failed", e);
    }
    hudSee(hudChat);
    return content;
  };

  const onOutput = async (arg0) => {
    try {
      const { characterIndex, chatIndex, messageIndex, chat } = arg0 || {};
      // The listener's own chat argument is the event snapshot; the later
      // re-read by coordinates is a different source and is labelled so.
      const pending = [];
      const capture = await captureText();
      const eventSnapshot = await snapshotFromChat(chat, pending, capture, characterIndex);
      await record("output", {
        listener: { character_index: characterIndex, chat_index: chatIndex, message_index: messageIndex },
        current: await coords(),
        event_chat: eventSnapshot,
        reread_chat: await chatSnapshot(characterIndex, chatIndex, pending, capture),
      }, pending);
      hudSee(chat && chat.id);
    } catch (e) { recordPluginError(e);
      console.warn("[AC2] output observe failed", e);
    }
  };

  async function register(name, fn, undo) {
    try {
      await fn();
      cleanups.push(undo);
      await record("register", { target: name, ok: true });
    } catch (e) { recordPluginError(e);
      await record("register", { target: name, ok: false, error: String(e) });
    }
  }

  await register("input", () => api.addRisuScriptHandler("input", onInput), () => api.removeRisuScriptHandler("input", onInput));
  await register("pdfBody", async () => {
    if (typeof api.registerBodyIntercepter !== "function") return;
    pdfHook = await api.registerBodyIntercepter(onPDFBody);
  }, () => pdfHook && api.unregisterBodyIntercepter(pdfHook.id));
  await register("beforeRequest", () => api.addRisuReplacer("beforeRequest", onBeforeRequest), () => api.removeRisuReplacer("beforeRequest", onBeforeRequest));
  await register("afterRequest", () => api.addRisuReplacer("afterRequest", onAfterRequest), () => api.removeRisuReplacer("afterRequest", onAfterRequest));
  await register("output", () => api.addRisuChatListener("output", onOutput), () => api.removeRisuChatListener("output", onOutput));

  api.onUnload(async () => {
    for (const undo of cleanups) { try { await undo(); } catch (_) {} }
    // unsent observations stay in the outbox for the next load
    unloaded = true;
    pdfRequests.clear();
    lastAppliedPDF = null;
    if (retryTimer) clearTimeout(retryTimer);
    retryTimer = null;
    await record("unload", {});
    await flush();
    await saving;
  });

  // ---- settings screen (step 9) ----
  // The screen is drawn in this plugin's own frame (RisuAI shows it full
  // screen); values, checks and stable error codes come from the backend.
  const ERR_TEXT = {
    token_required: "접속 토큰을 넣어 주세요.",
    token_invalid: "토큰이 맞지 않아요. 백엔드 창에 표시된 토큰을 다시 복사해 붙여 넣어 주세요.",
    invalid_budget: "0 이상의 숫자를 넣어 주세요.",
    bad_request: "보낸 값을 읽지 못했어요.",
    hypa_empty: "가져올 HypaMemory 요약이 없어요.",
    hypa_too_many: "HypaMemory 요약은 한 번에 500개까지 가져올 수 있어요.",
    store_failed: "백엔드가 값을 저장하지 못했어요.",
    unmerge_no_record: "이 병합을 되돌릴 기록을 찾지 못했어요.",
    unknown_provider: "알 수 없는 제공자예요.",
    model_required: "모델 이름을 넣어 주세요.",
    url_required: "이 제공자는 주소가 필요해요.",
    key_required: "이 제공자는 API 키가 필요해요.",
    bad_reasoning: "이 제공자가 받지 않는 추론 설정이에요.",
    bad_retries: "다시 시도 횟수는 0~10회예요.",
    bad_known_chars: "평론가 요청 최대 크기는 0 이상의 숫자예요.",
    memory_not_found: "이 기억을 찾지 못했어요. 이미 지워졌거나 다른 채팅의 기억이에요.",
    inherited_read_only: "부모 채팅에서 물려받은 기억이에요. 부모 채팅에서 고쳐 주세요.",
    empty_body: "본문을 비울 수는 없어요.",
    bad_importance: "중요도는 0~10이에요.",
    chat_required: "열린 채팅을 읽지 못했어요.",
    pick_name: "남길 이름을 골라 주세요.",
  };
  function errText(res) {
    if (res.json && res.json.code === "database_not_ready") return String(res.json.message || "");
    if (res.code && ERR_TEXT[res.code]) return ERR_TEXT[res.code];
    if (res.kind === "connect_failed" || res.kind === "timeout") return "백엔드에 연결할 수 없어요. 주소와 백엔드가 켜져 있는지 확인해 주세요.";
    return "요청이 실패했어요 (" + (res.status || res.kind) + ").";
  }
  function el(tag, attrs, ...kids) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (k === "on") for (const [ev, fn] of Object.entries(v)) e.addEventListener(ev, fn);
      else if (k === "class") e.className = v;
      else e.setAttribute(k, v);
    }
    for (const k of kids) if (k !== null && k !== undefined) e.append(k);
    return e;
  }
  const PANEL_CSS = `
    :root{--bg:#F6F5F1;--panel:#FFF;--soft:#FBFAF7;--line:#E3E0D8;--line2:#EEEBE4;--ink:#1E1D1A;--ink2:#3B3A35;--mute:#5E5C55;--acc:#0E6B63;--onacc:#FFF;--accbg:#E3F1EE;--accink:#09504A;--bad:#9B2C2C;--badbg:#FBEAEA;--iline:#D6D2C8;--warnbg:#FBEFDD;--warnink:#7A4100;--vio:#3E2F7A;--viobg:#EEEAF8;--blu:#2F3E5C;--blubg:#E7ECF5;--gray:#4A4944;--graybg:#EEEBE4;--inv:#1E1D1A;--oninv:#FFF}
    @media (prefers-color-scheme: dark){:root{--bg:#141413;--panel:#1C1B19;--soft:#211F1D;--line:#33312C;--line2:#2A2825;--ink:#ECEAE4;--ink2:#D2CFC6;--mute:#A6A298;--acc:#46BFAE;--onacc:#0B1F1C;--accbg:#163631;--accink:#9FE3D7;--bad:#F2A7A7;--badbg:#3A1B1B;--iline:#3A3833;--warnbg:#3A2A12;--warnink:#F2C98A;--vio:#CFC5F5;--viobg:#2C2547;--blu:#C3D1EE;--blubg:#232C3D;--gray:#D2CFC6;--graybg:#2A2825;--inv:#ECEAE4;--oninv:#141413}}
    *{box-sizing:border-box} [hidden]{display:none!important} body{margin:0;background:var(--bg);color:var(--ink);font-family:"IBM Plex Sans KR",system-ui,-apple-system,"Malgun Gothic",sans-serif}
    .ac2-wrap{max-width:860px;margin:0 auto;padding:16px} .ac2-card{background:var(--panel);border:1px solid var(--line);border-radius:14px;overflow:hidden}
    .ac2-head{display:flex;align-items:center;gap:12px;padding:14px 18px;border-bottom:1px solid var(--line)}
    .ac2-viewbar{display:flex;align-items:center;flex-wrap:wrap;gap:10px;padding:8px 18px;border-bottom:1px solid var(--line)}
    .ac2-logo{width:32px;height:32px;border-radius:8px;background:var(--acc);color:var(--onacc);display:flex;align-items:center;justify-content:center;font-weight:600;font-size:14px}
    .ac2-title{font-weight:600;font-size:16px;flex:1} .ac2-tabs{display:flex;flex-wrap:wrap;gap:4px;padding:8px 12px;border-bottom:1px solid var(--line)}
    .ac2-tab{min-height:40px;padding:0 14px;border:1px solid transparent;border-radius:8px;background:transparent;color:var(--mute);font-size:14px;cursor:pointer}
    .ac2-tab[aria-pressed=true]{border-color:var(--inv);background:var(--inv);color:var(--oninv);font-weight:600}
    .ac2-body{padding:18px;display:flex;flex-direction:column;gap:14px} .ac2-box{border:1px solid var(--line);border-radius:12px;padding:14px;display:flex;flex-direction:column;gap:8px}
    .ac2-label{font-size:14px;font-weight:500} .ac2-help{font-size:12px;line-height:1.6;color:var(--mute)}
    .ac2-row{display:flex;gap:8px;flex-wrap:wrap} .ac2-row>input{flex:1;min-width:180px}
    input{height:42px;border:1px solid var(--iline);border-radius:8px;padding:0 12px;font-size:14px;background:var(--panel);color:var(--ink);font-family:inherit}
    button.ac2-btn{min-height:42px;padding:0 14px;border:1px solid var(--iline);border-radius:8px;background:var(--panel);color:var(--ink);font-size:14px;cursor:pointer;font-family:inherit}
    button.ac2-primary{border-color:var(--acc);background:var(--acc);color:var(--onacc)} button.ac2-danger{border-color:var(--bad);color:var(--bad);background:transparent}
    .ac2-msg{font-size:13px;line-height:1.6;border-radius:8px;padding:8px 10px} .ac2-ok{color:var(--accink);background:var(--accbg)} .ac2-err{color:var(--bad);background:var(--badbg)}
    select,textarea.ac2-area{min-height:42px;border:1px solid var(--iline);border-radius:8px;padding:0 10px;font-size:14px;background:var(--panel);color:var(--ink);font-family:inherit}
    textarea.ac2-area{padding:8px 10px;font-family:ui-monospace,Consolas,monospace;resize:vertical}
    .ac2-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px} .ac2-field{display:flex;flex-direction:column;gap:6px;min-width:0}
    .ac2-overlay{position:fixed;inset:0;background:rgba(0,0,0,.45);display:flex;align-items:center;justify-content:center;padding:16px;z-index:10}
    .ac2-dialog{width:100%;max-width:440px;background:var(--panel);border:1px solid var(--line);border-radius:14px;padding:16px;display:flex;flex-direction:column;gap:10px}
    .ac2-dhead{display:flex;align-items:center;justify-content:space-between} .ac2-tokfield{width:100%;font-family:ui-monospace,Consolas,monospace}
    button.ac2-x{width:40px;height:40px;border:1px solid var(--iline);border-radius:8px;background:var(--panel);color:var(--ink);font-size:20px;cursor:pointer}
    .ac2-kv{display:flex;justify-content:space-between;gap:12px;font-size:14px} .ac2-chip{display:flex;align-items:center;gap:10px}
    .ac2-dot{width:14px;height:14px;border-radius:50%;background:#7A776E;flex:none}
    .ac2-dot[data-chip=done]{background:#2F8F5B} .ac2-dot[data-chip=generating]{background:#0E6B63} .ac2-dot[data-chip=retrying]{background:#B7791F}
    .ac2-dot[data-chip=embedding_failed],.ac2-dot[data-chip=given_up]{background:#C53030}
    .ac2-act{display:flex;flex-wrap:wrap;gap:4px 10px;align-items:baseline;font-size:14px;padding:6px 0;border-top:1px solid var(--line)} .ac2-wide{flex-basis:100%}
    .ac2-mlist{display:flex;flex-direction:column;gap:10px} .ac2-mhead{align-items:center} .ac2-mbody{font-size:14px;line-height:1.7;white-space:pre-wrap;word-break:break-word}
    .ac2-badge{font-size:12px;padding:2px 8px;border-radius:999px;border:1px solid var(--line);color:var(--mute)} .ac2-bsecret{color:var(--bad);border-color:var(--bad)}
    input[type=search]{flex:1;min-width:180px} textarea.ac2-wide{width:100%}
    .ac2-map{position:relative;height:60vh;min-height:320px;border:1px solid var(--line);border-radius:12px;overflow:hidden;touch-action:none;cursor:grab;background:var(--bg)}
    .ac2-wl{display:block;width:100%;height:100%;user-select:none} .ac2-wl text{font-size:13px;fill:var(--ink)} .ac2-wl text.wl-p{font-size:12px;fill:var(--mute)}
    .wl-trunk{stroke:var(--acc);stroke-width:3} .wl-trunk.wl-tin{stroke:var(--mute)} .wl-dash,.wl-bline{stroke:var(--mute);stroke-width:2;stroke-dasharray:4 3;fill:none}
    .wl-dot{fill:var(--panel);stroke:var(--acc);stroke-width:3} .wl-in .wl-dot{stroke:var(--mute)} .wl-off .wl-dot{stroke-dasharray:3 3} .wl-fork .wl-dot{stroke:#D97706}
    .wl-sel .wl-dot{fill:var(--acc)} .wl-ver{fill:var(--panel);stroke:var(--mute);stroke-width:1.5;stroke-dasharray:3 2} .ac2-wl text.wl-vt{font-size:11px;fill:var(--mute)}
    .wl-fold rect{fill:var(--panel);stroke:var(--iline);stroke-width:1.5;stroke-dasharray:5 4} .wl-branch rect{fill:var(--panel);stroke:#D97706;stroke-width:1.5}
    .wl-node,.wl-fold,.wl-branch{cursor:pointer;outline:none} .wl-node:focus-visible .wl-dot{stroke-width:5} .wl-fold:focus-visible rect,.wl-branch:focus-visible rect{stroke-width:3}
    .ac2-ctl{position:absolute;right:10px;bottom:10px;display:flex;flex-wrap:wrap;gap:6px;justify-content:flex-end} .ac2-ctl button.ac2-btn{min-height:38px;min-width:42px;padding:0 10px}
    .ac2-pre{white-space:pre-wrap;word-break:break-word;font-size:14px;line-height:1.7;max-height:320px;overflow:auto} .ac2-ver summary{cursor:pointer;font-size:13px;color:var(--mute);min-height:32px}
    /* lists, rows, small buttons, tags and inline confirmations (UI design 10-27) */
    .ac2-h1{margin:0;font-size:20px;font-weight:600} .ac2-h2{margin:0;font-size:15px;font-weight:600;color:var(--ink2)} .ac2-num{font-family:ui-monospace,"IBM Plex Mono",Consolas,monospace}
    .ac2-list{border:1px solid var(--line);border-radius:12px;overflow:hidden;min-width:0} .ac2-item{padding:12px 16px;border-bottom:1px solid var(--line2);display:flex;flex-direction:column;gap:8px;min-width:0} .ac2-item:last-child{border-bottom:0}
    .ac2-item[aria-current=true]{background:var(--soft)} .ac2-top{display:flex;flex-wrap:wrap;gap:8px 12px;align-items:flex-start;min-width:0} .ac2-main{flex:1 1 14em;min-width:0}
    .ac2-text{font-size:15px;line-height:1.6;overflow-wrap:anywhere;white-space:pre-wrap} .ac2-title2{font-size:15px;font-weight:500;overflow-wrap:anywhere} .ac2-meta{font-size:12px;line-height:1.6;color:var(--mute);margin-top:2px;overflow-wrap:anywhere}
    .ac2-tags{display:flex;flex-wrap:wrap;gap:6px;align-items:center} .ac2-tag{font-size:12px;font-weight:500;padding:3px 10px;border-radius:6px;white-space:nowrap;color:var(--gray);background:var(--graybg)}
    .ac2-tag[data-tone=acc]{color:var(--accink);background:var(--accbg)} .ac2-tag[data-tone=warn]{color:var(--warnink);background:var(--warnbg)} .ac2-tag[data-tone=vio]{color:var(--vio);background:var(--viobg)}
    .ac2-tag[data-tone=blu]{color:var(--blu);background:var(--blubg)} .ac2-tag[data-tone=bad]{color:var(--bad);background:var(--badbg)}
    .ac2-acts{display:flex;flex-wrap:wrap;gap:6px;align-items:center} button.ac2-sbtn{min-height:32px;padding:0 10px;border:1px solid var(--iline);border-radius:6px;background:var(--panel);color:var(--ink);font-size:12px;cursor:pointer;font-family:inherit}
    button.ac2-sbtn.ac2-danger{border-color:var(--bad);color:var(--bad);background:transparent} button.ac2-sbtn.ac2-primary{border-color:var(--acc);background:var(--acc);color:var(--onacc)} button:disabled{opacity:.55;cursor:default}
    .ac2-confirm{display:flex;flex-wrap:wrap;gap:8px;align-items:center;background:var(--badbg);color:var(--bad);border-radius:8px;padding:10px 12px;font-size:13px;line-height:1.6} .ac2-confirm.ac2-calm{background:var(--soft);color:var(--ink)} .ac2-confirm>.ac2-say{flex:1 1 220px;min-width:0}
    .ac2-confirm button.ac2-btn{min-height:36px;font-size:13px} .ac2-panel{display:flex;flex-direction:column;gap:8px;background:var(--soft);border-radius:8px;padding:10px 12px;font-size:13px;line-height:1.7;color:var(--ink2);min-width:0}
    .ac2-sub{display:flex;flex-wrap:wrap;gap:4px 18px;border-bottom:1px solid var(--line);margin:-18px -18px 0;padding:0 18px} button.ac2-subtab{min-height:44px;padding:0 2px;border:0;border-bottom:2px solid transparent;border-radius:0;background:transparent;color:var(--mute);font-size:14px;cursor:pointer;font-family:inherit}
    button.ac2-subtab[aria-pressed=true]{border-bottom-color:var(--acc);color:var(--ink);font-weight:600}
    .ac2-kinds{display:flex;flex-wrap:wrap;gap:6px} button.ac2-kind{min-height:36px;padding:0 12px;border:1px solid var(--iline);border-radius:999px;background:var(--panel);color:var(--ink2);font-size:13px;cursor:pointer;font-family:inherit}
    button.ac2-kind[aria-pressed=true]{border-color:var(--inv);background:var(--inv);color:var(--oninv);font-weight:600} .ac2-kind>b{margin-left:6px;font-weight:600;font-family:ui-monospace,"IBM Plex Mono",Consolas,monospace}
    .ac2-group{padding:8px 16px;background:var(--soft);border-bottom:1px solid var(--line2);font-size:12px;font-weight:600;color:var(--ink2)}
    .ac2-meter{height:6px;border-radius:999px;background:var(--line2);overflow:hidden;margin-top:6px} .ac2-meter>i{display:block;height:100%;background:var(--acc)} .ac2-meter>i[data-tone=vio]{background:var(--vio)}
    .ac2-stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(120px,1fr));gap:10px} .ac2-stat{background:var(--soft);border:1px solid var(--line2);border-radius:10px;padding:10px 14px;min-width:0} .ac2-stat>b{display:block;font-size:20px;font-weight:600;font-family:ui-monospace,"IBM Plex Mono",Consolas,monospace}
    .ac2-warn{background:var(--warnbg);color:var(--warnink);border-radius:12px;padding:12px 16px;font-size:13px;line-height:1.6} .ac2-dangerzone{border-color:var(--bad)}
    button.ac2-switch{position:relative;flex:none;width:52px;height:30px;border-radius:999px;border:1px solid var(--iline);background:var(--line2);cursor:pointer;padding:0}
    button.ac2-switch[aria-checked=true]{background:var(--acc);border-color:var(--acc)} .ac2-knob{position:absolute;top:3px;left:3px;width:22px;height:22px;border-radius:50%;background:var(--panel);box-shadow:0 1px 3px rgba(0,0,0,.25);transition:left .15s}
    button.ac2-switch[aria-checked=true] .ac2-knob{left:25px}
    .ac2-clamp{display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden} .ac2-check{width:20px;height:20px;flex:none;margin-top:3px}`;
  let settingsTab = "status";

  function msgBox() {
    const box = el("div", { class: "ac2-msg", role: "status" });
    box.hidden = true;
    return {
      node: box,
      show(text, ok) { box.textContent = text; box.className = "ac2-msg " + (ok ? "ac2-ok" : "ac2-err"); box.hidden = false; },
      clear() { box.hidden = true; },
    };
  }

  // UI parts (design 10-27): a tag, a small button, and a confirmation shown
  // in a slot under the row it is about. run() returns a bridge result; a
  // failure stays in the confirmation, keep: true leaves the slot to run.
  function nodes(...list) {
    return list.filter((n) => n !== null && n !== undefined);
  }
  function tag(text, tone) {
    return el("span", { class: "ac2-tag", ...(tone ? { "data-tone": tone } : {}) }, text);
  }
  // a switch: a button that is on or off (aria-checked), its label beside it
  function toggle(id, label, on, onChange) {
    const b = el("button", { type: "button", id, role: "switch", class: "ac2-switch", "aria-checked": String(!!on) }, el("span", { class: "ac2-knob", "aria-hidden": "true" }));
    b.addEventListener("click", async () => {
      const next = b.getAttribute("aria-checked") !== "true";
      b.setAttribute("aria-checked", String(next));
      if (await onChange(next) === false) b.setAttribute("aria-checked", String(!next));
    });
    return el("div", { class: "ac2-top", style: "align-items:center" }, el("label", { class: "ac2-main ac2-label", for: id }, label), b);
  }
  function sbtn(label, onClick, cls) {
    return el("button", { type: "button", class: "ac2-sbtn" + (cls ? " " + cls : ""), on: { click: onClick } }, label);
  }
  // One write at a time from the settings screen (review 06): while a
  // confirmed action or a memory save runs, no other write opens or is sent;
  // the running confirmation itself stays where it is. A finished write reads
  // again the views shown now (refreshShown), not the view it started from,
  // which may have been drawn again or left meanwhile.
  let writing = false, activeBox = null;
  const BUSY_TEXT = "다른 작업을 처리하는 중이에요. 끝난 뒤 다시 눌러 주세요.";
  const WRITING_TEXT = "처리 중인 작업이 있어요. 끝나면 보고 있는 화면을 다시 읽어요.";
  const liveViews = new Set();
  const liveView = (root, refresh) => {
    for (const v of [...liveViews]) if (!v.root.isConnected) liveViews.delete(v); // views no longer shown
    liveViews.add({ root, refresh });
  };
  // the settings screen's one line for the write under way: shown on every
  // view, and where a failure is told when the place that would tell it (its
  // confirmation or editor) is no longer shown; kept when settings open again
  let writeBar = null, writeFailure = "", failureHome = null;
  const showWrite = (text, ok) => {
    if (!writeBar) return;
    writeBar.hidden = !text;
    writeBar.className = "ac2-msg " + (ok ? "ac2-ok" : "ac2-err");
    writeBar.replaceChildren(text || "");
  };
  const refreshShown = () => Promise.all([...liveViews].map((v) => {
    if (!v.root.isConnected) { liveViews.delete(v); return null; }
    return Promise.resolve().then(v.refresh).catch(recordPluginError);
  }));
  // home: the element that tells this write's result (review 07)
  async function oneWrite(run, box, home) {
    writing = true; activeBox = box; writeFailure = ""; failureHome = null;
    showWrite(WRITING_TEXT, true);
    let r;
    try { r = await run(); return r; } catch (e) { r = { ok: false, kind: "failed" }; throw e; } finally {
      writing = false; activeBox = null;
      // kept with where it was told, so settings opened again can tell it
      if (r && r.ok === false) { writeFailure = "앞의 작업이 끝나지 못했어요: " + errText(r); failureHome = home; }
      showWrite(failureHome && !failureHome.isConnected ? writeFailure : "", false);
    }
  }
  // true while a write runs: an empty slot shows why nothing opens; what a
  // slot already shows (a confirmation and its inputs, an editor and its
  // typing) stays, the line above telling the write under way (review 07, 08)
  const busyHere = (slot) => {
    if (!writing) return false;
    if (!slot.children.length) slot.replaceChildren(el("div", { class: "ac2-help" }, BUSY_TEXT));
    return true;
  };
  function askHere(slot, say, label, run, calm) {
    if (busyHere(slot)) return false;
    const note = el("span", { class: "ac2-say" }, say); // a caller may put its inputs here
    const tell = el("span", { class: "ac2-say", role: "status" }); // busy or failure, beside them
    const yes = el("button", { type: "button", class: "ac2-btn " + (calm ? "ac2-primary" : "ac2-danger") }, label);
    const no = el("button", { type: "button", class: "ac2-btn", on: { click: () => slot.replaceChildren() } }, "취소");
    yes.addEventListener("click", async () => {
      if (writing) { tell.replaceChildren(BUSY_TEXT); return; }
      tell.replaceChildren();
      yes.disabled = no.disabled = true;
      yes.replaceChildren("처리 중…");
      let r;
      try { r = await oneWrite(run, box, box); } catch (_) { r = { ok: false, kind: "failed" }; }
      yes.replaceChildren(label);
      if (r && r.ok === false) { tell.replaceChildren(errText(r)); yes.disabled = no.disabled = false; return; }
      if (!(r && r.keep)) slot.replaceChildren();
    });
    const box = el("div", { class: "ac2-confirm" + (calm ? " ac2-calm" : ""), role: "group" }, note, tell, yes, no);
    slot.replaceChildren(box);
    return true;
  }

  async function connTab(body) {
    const c = await conn();
    const url = el("input", { id: "ac2-url", value: c.url, autocomplete: "off", "aria-label": "백엔드 주소" });
    const token = el("input", { id: "ac2-token", type: "password", value: c.token, autocomplete: "off", "aria-label": "접속 토큰" });
    const status = msgBox();
    const addresses = el("div", { class: "ac2-help" });
    // connects by itself: when the tab opens and when typing stops; an
    // address and token the backend accepts are kept on this device
    let asked = 0;
    let typing = null;
    const check = async () => {
      const mine = ++asked;
      status.clear();
      addresses.textContent = "";
      const next = { url: url.value.trim().replace(/\/+$/, ""), token: token.value.trim() };
      if (!next.url) return status.show("백엔드 주소를 넣어 주세요.", false);
      // an empty token is tried too: the backend may have its check off
      let res = await bridge("/version", { method: "GET" }, next);
      if (mine !== asked) return;
      let assigned = false;
      let assignmentKept = false;
      if (!next.token && res.code === "token_required") {
        const claim = await bridge("/settings/token/claim", { method: "POST", body: "{}" }, next);
        if (!claim.ok) {
          if (mine !== asked) return;
          if (claim.code === "token_claim_closed") {
            const info = claim.json || {};
            return status.show("첫 토큰이 이미 다른 연결에 할당됐어요. 할당 시각: " + (info.claimed_at ? new Date(info.claimed_at).toLocaleString() : "기록 없음")
              + " · 요청 주소: " + (info.address || "기록 없음")
              + ". 백엔드 PC의 데이터 폴더 token.txt를 확인해 주세요. token.txt를 지우고 백엔드를 다시 켜면 새 토큰이 만들어지고 첫 할당이 다시 열려요.", false);
          }
          return status.show(errText(claim), false);
        }
        if (claim.json && claim.json.token) {
          next.token = String(claim.json.token);
          // Keep a one-time assignment as soon as it arrives, using the
          // existing shared plugin storage and device-local connection path.
          assignmentKept = await saveConn(next);
          assigned = true;
          if (mine === asked) token.value = next.token;
        }
        if (mine !== asked) return;
        res = await bridge("/version", { method: "GET" }, next);
      }
      if (mine !== asked) return; // a newer value is being checked
      if (!res.ok) return status.show(errText(res), false);
      const was = await conn();
      const kept = assigned ? assignmentKept : was.url === next.url && was.token === next.token ? true : await saveConn(next);
      const ready = await bridge("/ready", { method: "GET" }, next);
      if (mine !== asked) return;
      if (!ready.ok) return status.show(errText(ready), false);
      const ver = res.json && res.json.version ? res.json.version : "";
      addresses.textContent = res.json && res.json.connection ? res.json.connection.remote_hint || "" : "";
      if (kept) status.show("연결됨 · " + ver, true);
      else status.show("연결됨 · " + ver + " · 이 기기에 저장하지 못했어요. 다시 열면 이전 주소와 토큰으로 돌아갑니다.", false);
      loadTokenInfo();
    };
    for (const i of [url, token]) i.addEventListener("input", () => { clearTimeout(typing); typing = setTimeout(check, 800); });
    const tokenInfo = el("div", { class: "ac2-help" }, "연결하면 토큰 정보가 여기에 나와요.");
    const rotateMsg = msgBox();
    const rotateArea = el("div", { class: "ac2-row" });
    // the token check can be turned off (user decision 10-28, on by default)
    const useTok = el("input", { type: "checkbox", id: "ac2-tokcheck", style: "width:20px;height:20px" });
    const useMsg = msgBox();
    async function loadTokenInfo() {
      const res = await bridge("/settings/token", { method: "GET" });
      if (!res.ok || !res.json) return;
      tokenInfo.textContent = "끝 4자리 " + res.json.last4 + " · 만든 때 " + new Date(res.json.created_at).toLocaleString();
      useTok.checked = res.json.checked !== false;
    }
    useTok.addEventListener("change", async () => {
      useMsg.clear();
      const on = useTok.checked;
      const res = await bridge("/settings/token/check", { method: "POST", body: JSON.stringify({ checked: on }) });
      if (!res.ok) { useTok.checked = !on; return useMsg.show(errText(res), false); }
      if (!on) return useMsg.show("토큰 확인을 껐어요. 이제 토큰 없이도 접속돼요. 백엔드를 이 기기 밖에서 받게 해 두었다면 같은 망의 다른 기기도 접속할 수 있어요.", true);
      // turned on: this device keeps the token the backend checks from now
      const cur = await conn();
      const nu = String(res.json.token || "");
      const kept = await saveConn({ url: cur.url, token: nu });
      token.value = nu;
      useMsg.show(kept ? "토큰 확인을 켰어요. 토큰을 저장했어요. 같은 PocketRisu 서버를 쓰는 다른 기기도 이 토큰으로 접속돼요. 따로 쓰는 RisuAI에는 토큰을 넣어야 해요."
        : "토큰 확인을 켰지만 이 기기에 토큰을 저장하지 못했어요. 백엔드 시작 창에 표시된 데이터 폴더의 token.txt에 있는 토큰을 넣어 주세요.", kept);
    });
    const askRotate = () => {
      rotateArea.replaceChildren(
        el("span", { class: "ac2-help", style: "flex:1;min-width:200px" }, "새로 만들면 지금 토큰은 바로 쓸 수 없어요. 다른 기기에는 새 토큰을 다시 넣어야 해요."),
        el("button", { class: "ac2-btn ac2-danger", on: { click: doRotate } }, "새로 만들기"),
        el("button", { class: "ac2-btn", on: { click: () => rotateArea.replaceChildren(rotateBtn) } }, "취소"));
    };
    const doRotate = async () => {
      const res = await bridge("/settings/token/rotate", { method: "POST", body: "{}" });
      rotateArea.replaceChildren(rotateBtn);
      if (!res.ok || !res.json || !res.json.token) return rotateMsg.show(errText(res), false);
      const cur = await conn();
      const nu = String(res.json.token);
      // the new token takes effect here only once the backend accepts it as
      // the token it keeps (user decision 10-27)
      const probe = await bridge("/settings/token", { method: "GET" }, { url: cur.url, token: nu });
      if (!probe.ok || !probe.json || probe.json.last4 !== nu.slice(-4)) {
        return rotateMsg.show("새 토큰이 백엔드에 저장된 토큰과 맞는지 확인하지 못했어요. 백엔드 시작 창에 표시된 데이터 폴더의 token.txt에 있는 토큰을 직접 넣어 주세요.", false);
      }
      const kept = await saveConn({ url: cur.url, token: nu });
      token.value = nu;
      if (kept) rotateMsg.show("새 토큰을 확인하고 저장했어요. 같은 PocketRisu 서버를 쓰는 다른 기기도 새 토큰을 써요.", true);
      else rotateMsg.show("새 토큰을 확인했지만 이 기기에 저장하지 못했어요. 창을 닫기 전에 반드시 복사해 두세요.", false);
      showTokenOnce(nu);
      loadTokenInfo();
    };
    // The new token is shown this once, to copy to the other devices; after
    // closing, the screen never shows it again (token.txt in the shared data directory still has it).
    // User decision 10-27; removable as a whole if users object.
    function showTokenOnce(value) {
      const field = el("input", { class: "ac2-tokfield", readonly: "readonly", value, "aria-label": "새 접속 토큰" });
      const note = el("div", { class: "ac2-help" },
        el("strong", {}, "지금 반드시 복사하세요. "),
        "이 토큰은 이번 한 번만 보여요. 다른 기기의 설정 → 연결에 붙여 넣고, 다른 사람이 볼 수 없는 곳에 보관하세요. 창을 닫으면 다시 보이지 않아요.");
      const copied = el("div", { class: "ac2-msg ac2-ok", role: "status" });
      copied.hidden = true;
      const copy = async () => {
        field.focus();
        field.select();
        let ok = false;
        try { await navigator.clipboard.writeText(value); ok = true; } catch (_) {
          try { ok = document.execCommand("copy"); } catch (_) { ok = false; }
        }
        copied.textContent = ok ? "복사했어요." : "자동 복사가 막혀 있어요. 선택된 토큰을 길게 눌러 복사해 주세요.";
        copied.className = "ac2-msg " + (ok ? "ac2-ok" : "ac2-err");
        copied.hidden = false;
      };
      const close = () => { field.value = ""; overlay.remove(); };
      const overlay = el("div", { class: "ac2-overlay", role: "dialog", "aria-modal": "true", "aria-label": "새 접속 토큰" },
        el("div", { class: "ac2-dialog" },
          el("div", { class: "ac2-dhead" }, el("div", { class: "ac2-label" }, "새 접속 토큰"), el("button", { class: "ac2-x", "aria-label": "닫기", on: { click: close } }, "×")),
          field, note,
          el("div", { class: "ac2-row" }, el("button", { class: "ac2-btn ac2-primary", on: { click: copy } }, "복사")),
          copied));
      document.body.append(overlay);
      field.select();
    }
    const rotateBtn = el("button", { class: "ac2-btn ac2-danger", on: { click: askRotate } }, "토큰 새로 만들기");
    rotateArea.append(rotateBtn);
    body.append(
      el("div", { class: "ac2-box" },
        el("label", { class: "ac2-label", for: "ac2-url" }, "이 기기에서 백엔드에 접속할 주소"), url,
        el("label", { class: "ac2-label", for: "ac2-token" }, "접속 토큰"),
        token,
        status.node,
        addresses,
        el("div", { class: "ac2-help" }, "백엔드가 토큰을 처음 만든 뒤 첫 연결에는 토큰을 자동으로 받아 저장해요. 토큰은 백엔드 창과 데이터 폴더 token.txt에서도 확인할 수 있고, 여기에 직접 붙여 넣어도 연결됩니다. 토큰 사용이 켜져 있으면 받을 주소가 ‘이 기기만’이어도 모든 요청에서 토큰을 확인합니다. 주소는 기기마다 따로 저장돼요. 토큰은 플러그인 저장소에 저장돼서, 같은 PocketRisu 서버를 쓰는 기기라면 한 번만 넣으면 돼요.")),
      el("div", { class: "ac2-box" }, el("div", { class: "ac2-label" }, "백엔드 토큰"),
        el("label", { class: "ac2-chip", for: "ac2-tokcheck" }, useTok, "접속 토큰 사용"),
        el("div", { class: "ac2-help" }, "끄면 토큰 없이도 접속돼요. 처음에는 켜져 있어요."), useMsg.node,
        tokenInfo, rotateArea, rotateMsg.node));
    check();
  }

  async function budgetTab(body) {
    const status = msgBox();
    const mem = el("input", { id: "ac2-mem", type: "number", min: "0", step: "100", "aria-label": "기억 예산" });
    const sec = el("input", { id: "ac2-sec", type: "number", min: "0", step: "100", "aria-label": "비밀 예산" });
    const placement = el("select", { id: "ac2-placement", "aria-label": "기억 주입 위치" });
    const transport = el("select", { id: "ac2-transport", "aria-label": "기억 전달 방식" },
      el("option", { value: "text" }, "텍스트 (기본)"), el("option", { value: "google_pdf" }, "Google 직접 연결 PDF"), el("option", { value: "llm_gateway_pdf" }, "OpenAI 호환 PDF (LLM Gateway 등)"),
      el("option", { value: "provider_manager_pdf" }, "Yumi Provider PDF (실험)"));
    const anchor = el("input", { id: "ac2-anchor", "aria-label": "앵커 마커", placeholder: "<ARCHIVE_CENTER_MEMORY_ANCHOR>" });
    const anchorField = el("div", { class: "ac2-field" }, el("label", { class: "ac2-label", for: "ac2-anchor" }, "앵커 마커"), anchor,
      el("div", { class: "ac2-help" }, "프롬프트 안에 이 글자가 든 마지막 메시지 뒤에 넣어요."));
    const showAnchor = () => { anchorField.hidden = placement.value !== "auto" && placement.value !== "after_anchor_marker"; };
    placement.addEventListener("change", showAnchor);
    const save = async () => {
      status.clear();
      const m = mem.value.trim(), s = sec.value.trim();
      if (!/^\d+$/.test(m) || !/^\d+$/.test(s)) return status.show(ERR_TEXT.invalid_budget, false);
      const res = await bridge("/settings/memory", { method: "POST", body: JSON.stringify({ memory_chars: Number(m), secret_chars: Number(s), placement: placement.value, anchor: anchor.value, transport_mode: transport.value }) });
      if (!res.ok) return status.show(errText(res), false);
      mem.value = res.json.memory_chars; sec.value = res.json.secret_chars;
      transport.value = res.json.transport_mode || "text";
      placement.value = res.json.placement; anchor.value = res.json.anchor; placementAnchor = res.json.anchor || "";
      status.show("저장했어요. 다음 응답부터 적용돼요.", true);
    };
    for (const i of [mem, sec]) i.addEventListener("input", () => status.clear());
    body.append(
      el("div", { class: "ac2-help" }, "두 예산은 따로 계산되는 상한입니다. 비밀 기억은 비밀 예산으로만 들어갑니다."),
      el("div", { class: "ac2-grid" },
        el("div", { class: "ac2-box" }, el("label", { class: "ac2-label", for: "ac2-mem" }, "기억 예산 (글자)"), mem),
        el("div", { class: "ac2-box" }, el("label", { class: "ac2-label", for: "ac2-sec" }, "비밀 예산 (글자)"), sec)),
      el("div", { class: "ac2-box" }, el("label", { class: "ac2-label", for: "ac2-transport" }, "기억 전달 방식"), transport,
        el("div", { class: "ac2-help" }, "이미 선택된 기억의 전달 형식만 바꿔요. Google PDF는 최신 사용자 메시지에 첨부되며, 변환할 수 없으면 텍스트를 유지해요. Yumi 실험 모드는 모델별 ‘텍스트 변환: Gemini PDF’와 전체 설정의 ‘수동 지정 기능 사용’을 모두 켜야 해요. 수동 지정이 꺼져 있으면 Yumi가 프롬프트 전체를 변환할 수 있으며 AC2가 제어할 수 없어요. PDF 생성과 오류 처리는 Yumi에 맡겨요.")),
      el("div", { class: "ac2-box" }, el("label", { class: "ac2-label", for: "ac2-placement" }, "기억 주입 위치"), placement, anchorField,
        el("div", { class: "ac2-help" }, "자동은 앵커 마커가 있으면 그 메시지 뒤, 없으면 최신 사용자 입력 앞에 넣어요. 찾는 자리가 없으면 최신 사용자 입력 앞을 써요. 프롬프트 원문은 보내지 않고, 메시지마다 마커가 있는지만 알려요.")),
      status.node,
      el("div", { class: "ac2-row" }, el("button", { class: "ac2-btn ac2-primary", on: { click: save } }, "저장")));
    const res = await bridge("/settings/memory", { method: "GET" });
    if (!res.ok) return status.show(errText(res), false);
    mem.value = res.json.memory_chars; sec.value = res.json.secret_chars;
    transport.value = res.json.transport_mode || "text";
    const labels = { auto: "자동", before_latest_user: "최신 사용자 입력 앞", after_anchor_marker: "앵커 마커 뒤", after_last_cache_point: "마지막 캐시 지점 뒤", after_first_system: "첫 system 뒤", end: "요청 메시지 끝" };
    placement.replaceChildren(...(res.json.placements || []).map((p) => el("option", { value: p }, labels[p] || p)));
    placement.value = res.json.placement; anchor.value = res.json.anchor; placementAnchor = res.json.anchor || "";
    showAnchor();
  }

  // Critic (memory generation) and embedding connections. The choices, what
  // each provider needs, and the checks come from the backend's view.
  const REASONING_TEXT = { "": "기본", off: "끔", low: "낮음", medium: "보통", high: "높음", max: "최대" };
  function u5Error(res) {
    return ({ bad_extra_headers_json: "Extra Headers는 JSON 객체로 넣어 주세요.", bad_extra_body_json: "Extra Body는 JSON 객체로 넣어 주세요." })[res.code] || errText(res);
  }
  // the critic's prompt, or the publisher's (user 11-09, like AC1): Go keeps
  // the text and its default
  const PROMPTS = {
    critic: { path: "/settings/prompt", id: "ac2-prompt", who: "평론가", warn: "기억을 만드는 지시문과 JSON 형식 설명이 함께 들어 있어요. 잘못 고치면 기억 만들기가 실패할 수 있어요. 그럴 때는 ‘기본값 복원’을 누르세요." },
    publisher: { path: "/settings/publisher-prompt", id: "ac2-pub-prompt", who: "출판사", warn: "서사 가이드의 규칙 부분이에요. 단계별 항목과 출력 형식은 자동으로 붙어요. 근거(refs)를 적으라는 규칙을 빼면 항목이 버려질 수 있어요. 이상하면 ‘기본값 복원’을 누르세요." } };
  async function promptBox(which = "critic") {
    const P = PROMPTS[which];
    const note = msgBox();
    const content = el("textarea", { id: P.id, rows: "16", class: "ac2-area ac2-wide", "aria-label": P.who + " 지시문" });
    let loaded = "";
    const apply = (v) => { content.value = loaded = v.content || ""; };
    const save = async (reset) => {
      const r = await bridge(P.path, { method: "POST", body: JSON.stringify(reset ? { reset: true } : { content: content.value }) });
      if (!r.ok) return note.show(errText(r), false);
      apply(r.json); note.show(reset ? "기본값으로 돌아갔어요." : "지시문을 저장했어요.", true);
    };
    const box = el("details", { class: "ac2-box" }, el("summary", { class: "ac2-label", style: "cursor:pointer;min-height:32px" }, P.who + " 프롬프트 편집"),
      el("div", { class: "ac2-warn" }, P.warn),
      el("label", { class: "ac2-help", for: P.id }, P.who + " 지시문"), content,
      el("div", { class: "ac2-acts" }, el("button", { type: "button", class: "ac2-btn ac2-primary", on: { click: () => save(false) } }, "지시문 저장"),
        el("button", { type: "button", class: "ac2-btn", on: { click: () => save(true) } }, "기본값 복원")), note.node);
    const r = await bridge(P.path, { method: "GET" });
    if (r.ok) apply(r.json); else note.show(errText(r), false);
    box.dirty = () => content.value !== loaded; // unsaved edits
    return box;
  }
  async function modelsTab(body, keep) {
    let tail; // the publisher and prompt boxes; kept as they are when the critic/embedding save redraws
    const status = msgBox();
    const res = await bridge("/settings/models", { method: "GET" });
    if (!res.ok) { body.append(status.node); return status.show(errText(res), false); }
    const view = res.json;
    let provider = view.critic.provider || "";
    const providers = view.providers || [];
    const info = () => providers.find((p) => p.id === provider) || null;
    // a field: its label over its input, with a help line when given
    const field = (id, label, input, ...help) => el("div", { class: "ac2-field" }, el("label", { class: "ac2-label", for: id }, label), input, ...help);
    // the providers as one drop-down, "" first when given
    const choose = (id, label, list, none, cur, onChange) => {
      let was = cur; // the same choice again changes nothing
      const s = el("select", { id, "aria-label": label, on: { change: () => { if (s.value !== was) { was = s.value; onChange(s.value); } } } },
        ...(none ? [{ id: "", label: none }] : []).concat(list).map((p) => {
          const o = el("option", { value: p.id }, p.label);
          if (p.id === cur) o.selected = true;
          return o;
        }));
      return s;
    };
    const pick = choose("ac2-cprov", "평론가 제공자", providers, "사용 안 함", provider, (id) => {
      provider = id;
      keyIn.value = ""; keyJson.value = ""; // a key typed for another provider never goes to this one
      draw();
    });
    const model = el("input", { id: "ac2-model", value: view.critic.model || "", autocomplete: "off", "aria-label": "모델" });
    const url = el("input", { id: "ac2-curl", value: view.critic.url || "", autocomplete: "off", "aria-label": "주소" });
    const keyIn = el("input", { id: "ac2-ckey", type: "password", autocomplete: "off", "aria-label": "API 키" });
    const keyJson = el("textarea", { id: "ac2-ckeyjson", rows: "4", "aria-label": "서비스 계정 JSON", class: "ac2-area" });
    const reasoning = el("select", { id: "ac2-reason", "aria-label": "추론" });
    const retries = el("input", { id: "ac2-retries", type: "number", min: "0", max: "10", step: "1", value: String(view.critic.retries ?? 3), "aria-label": "다시 시도 횟수" });
    const knownChars = el("input", { id: "ac2-known-chars", type: "number", min: "0", step: "1000", value: String(view.critic.known_chars ?? 50000), "aria-label": "평론가 요청 최대 크기" });
    const number = (id, name, value, step) => el("input", { id, type: "number", step, value: value ?? "", placeholder: "기존 기본값", "aria-label": name });
    const timeout = number("ac2-timeout", "응답 대기 시간(초)", view.critic.timeout_ms == null ? "" : view.critic.timeout_ms / 1000, "1");
    const temperature = number("ac2-temperature", "Temperature", view.critic.temperature, "0.1");
    const maxTokens = number("ac2-max-tokens", "Max Tokens", view.critic.max_tokens, "1");
    const headers = el("textarea", { id: "ac2-extra-headers", rows: "3", class: "ac2-area", "aria-label": "Extra Headers JSON" });
    const extra = el("textarea", { id: "ac2-extra-body", rows: "3", class: "ac2-area", "aria-label": "Extra Body JSON" });
    headers.value = view.critic.extra_headers_json || ""; extra.value = view.critic.extra_body_json || "";
    const tier = el("select", { id: "ac2-tier", "aria-label": "Service Tier" }, ...["", "standard", "flex", "priority"].map((v) => el("option", { value: v }, v || "기본")));
    tier.value = view.critic.service_tier || "";
    const cache = el("select", { id: "ac2-claude-cache", "aria-label": "Claude 프롬프트 캐시" }, ...[["", "기본"], ["off", "끔"], ["ephemeral_5m", "Automatic 5 min"], ["ephemeral_1h", "Automatic 1 hour"]].map(([value, label]) => el("option", { value }, label)));
    cache.value = view.critic.claude_prompt_cache || "";
    const tierField = field("ac2-tier", "Service Tier", tier);
    const cacheField = field("ac2-claude-cache", "Claude 프롬프트 캐시", cache);
    const urlHelp = el("div", { class: "ac2-help" });
    const keyHelp = el("div", { class: "ac2-help" });
    const keyLabel = el("label", { class: "ac2-label", for: "ac2-ckey" }, "API 키");
    const keyJsonLabel = el("label", { class: "ac2-label", for: "ac2-ckeyjson" }, "서비스 계정 JSON");
    const keyField = el("div", { class: "ac2-field" }, keyLabel, keyIn, keyJsonLabel, keyJson, keyHelp,
      el("div", { class: "ac2-help" }, "키는 백엔드에 암호화해 저장하고, 화면에는 끝 4자리만 보여요."));
    const fields = el("div", { class: "ac2-field", style: "gap:12px" },
      el("div", { class: "ac2-grid" },
        field("ac2-model", "모델", model), field("ac2-curl", "주소", url, urlHelp),
        field("ac2-reason", "추론", reasoning),
        field("ac2-retries", "실패 시 다시 시도 (회)", retries, el("div", { class: "ac2-help" }, "1분, 5분, 30분 뒤 자동으로 다시 시도해요.")),
        field("ac2-known-chars", "평론가 요청 최대 크기 (자)", knownChars, el("div", { class: "ac2-help" }, "기억을 만들 때 평론가에게 보내는 요청 전체(지침·이번 턴 원문·기존 기억)의 크기예요. 지침과 이번 턴 원문이 쓰고 남은 만큼만 기존 기억을 읽어서, 응답이 긴 턴일수록 기존 기억을 덜 읽어요. 넘으면 덜 필요한 기억부터 이번 호출에서만 빼고, 저장된 기억은 그대로예요. 줄이면 입력 토큰이 줄지만 너무 작으면 이전 기억과 잇는 힘이 약해져요. 약속·비밀처럼 꼭 남기는 기억은 넘어도 남아요. 한국어는 모델마다 다르지만 대략 1자에 0.6토큰이에요. 0은 상한 없음."))),
      el("details", { class: "ac2-panel" }, el("summary", { class: "ac2-label", style: "cursor:pointer;min-height:32px" }, "고급 설정 · 비워 두면 기본값"),
        el("div", { class: "ac2-grid" },
          field("ac2-timeout", "응답 대기 시간(초)", timeout, el("div", { class: "ac2-help" }, "비우면 600초(10분)")), field("ac2-temperature", "Temperature", temperature), field("ac2-max-tokens", "Max Tokens", maxTokens),
          tierField, cacheField),
        field("ac2-extra-headers", "Extra Headers (JSON)", headers), field("ac2-extra-body", "Extra Body (JSON)", extra)));
    const draw = () => {
      const p = info();
      fields.hidden = keyField.hidden = !p;
      tierField.hidden = !(view.service_tier_providers || []).includes(provider);
      cacheField.hidden = provider !== "claude";
      if (!p) return;
      url.placeholder = p.default_url || "https://…/v1";
      urlHelp.textContent = p.needs_url ? "이 제공자는 주소를 꼭 넣어야 해요." : "비워 두면 기본 주소를 써요.";
      const keep = view.critic.provider === provider && view.critic.key_set;
      keyHelp.textContent = keep ? "저장된 키 끝 4자리 " + view.critic.key_last4 + " · 바꿀 때만 새로 넣으세요."
        : provider === "copilot" ? "Copilot 앱(VS Code 등)으로 GitHub에 로그인해 받은 토큰을 넣어 주세요. GitHub 설정에서 만드는 일반 개인 토큰은 받아 주지 않을 수 있어요."
        : p.needs_key ? "API 키를 넣어 주세요." : "키가 필요 없으면 비워 두세요.";
      keyIn.hidden = keyLabel.hidden = provider === "vertex";
      keyJson.hidden = keyJsonLabel.hidden = provider !== "vertex";
      const cur = view.critic.provider === provider ? view.critic.reasoning || "" : "";
      reasoning.replaceChildren(...[""].concat(p.reasoning || []).map((r) => {
        const o = el("option", { value: r }, REASONING_TEXT[r] || r);
        if (r === cur) o.selected = true;
        return o;
      }));
    };

    // Embedding: AC1's providers; the choices, defaults and suggestions come
    // from the backend's view.
    const ev = view.embedding;
    const eproviders = view.embed_providers || [];
    let eprovider = ev.provider || "";
    const epick = choose("ac2-eprov", "임베딩 제공자", eproviders, "", eprovider, (id) => {
      eprovider = id;
      ekey.value = ""; ekeyJson.value = ""; // a key typed for another provider never goes to this one
      const p = eproviders.find((x) => x.id === id) || {};
      const back = ev.provider === id;
      emodel.value = back ? ev.model || "" : p.default_model || "";
      eurl.value = back ? ev.url || "" : "";
      edraw();
    });
    // the model: the provider's known models in a drop-down, the last choice
    // "직접 입력" (or no list) opens the text field; the text field holds
    // the value that is saved
    const OWN = "\u0000own";
    const emodel = el("input", { id: "ac2-emodel", value: ev.model || "", autocomplete: "off", "aria-label": "임베딩 모델 이름", placeholder: "모델 이름" });
    const emodelPick = el("select", { id: "ac2-emodelpick", "aria-label": "임베딩 모델", on: { change: () => {
      if (emodelPick.value !== OWN) emodel.value = emodelPick.value;
      emodel.hidden = emodelPick.value !== OWN;
    } } });
    const eurl = el("input", { id: "ac2-eurl", value: ev.url || "", autocomplete: "off", "aria-label": "임베딩 주소" });
    const ekey = el("input", { id: "ac2-ekey", type: "password", autocomplete: "off", "aria-label": "임베딩 API 키" });
    const ekeyJson = el("textarea", { id: "ac2-ekeyjson", rows: "4", "aria-label": "임베딩 서비스 계정 JSON", class: "ac2-area" });
    const ekeyLabel = el("label", { class: "ac2-label", for: "ac2-ekey" }, "API 키");
    const ekeyJsonLabel = el("label", { class: "ac2-label", for: "ac2-ekeyjson" }, "서비스 계정 JSON");
    const eurlHelp = el("div", { class: "ac2-help" });
    const ekeyHelp = el("div", { class: "ac2-help" });
    const edraw = () => {
      const p = eproviders.find((x) => x.id === eprovider) || {};
      const known = (ev.suggestions || {})[eprovider] || [];
      const own = !known.includes(emodel.value);
      emodelPick.replaceChildren(...known.map((m) => el("option", { value: m }, m)).concat(el("option", { value: OWN }, "직접 입력")));
      emodelPick.value = own ? OWN : emodel.value;
      emodelPick.hidden = !known.length;
      emodel.hidden = known.length > 0 && !own;
      eurl.placeholder = p.default_url || "https://…/v1";
      eurlHelp.textContent = p.needs_url ? "이 제공자는 주소를 꼭 넣어야 해요." : "비워 두면 기본 주소를 써요.";
      const same = ev.provider === eprovider;
      ekeyHelp.textContent = same && ev.key_source === "saved" ? "저장된 키 끝 4자리 " + ev.key_last4 + " · 바꿀 때만 새로 넣으세요."
        : same && ev.key_source === "env" ? "환경변수(VOYAGE_API_KEY)의 키를 쓰는 중 · 여기에 넣으면 이 키를 써요."
        : p.needs_key ? "키가 없으면 임베딩 없이 단어와 연결로만 기억을 골라요." : "키가 필요 없으면 비워 두세요.";
      ekey.hidden = ekeyLabel.hidden = eprovider === "vertex";
      ekeyJson.hidden = ekeyJsonLabel.hidden = eprovider !== "vertex";
    };

    const save = async () => {
      status.clear();
      const n = retries.value.trim();
      if (!/^\d+$/.test(n)) return status.show(ERR_TEXT.bad_retries, false);
      const kc = knownChars.value.trim();
      if (!/^\d+$/.test(kc)) return status.show(ERR_TEXT.bad_known_chars, false);
      const critic = { provider, model: model.value.trim(), url: url.value.trim(), reasoning: reasoning.value || "", retries: Number(n), known_chars: Number(kc) };
      critic.timeout_ms = timeout.value.trim() === "" ? null : Math.round(Number(timeout.value) * 1000);
      critic.temperature = temperature.value.trim() === "" ? null : Number(temperature.value);
      critic.max_tokens = maxTokens.value.trim() === "" ? null : Number(maxTokens.value);
      critic.extra_headers_json = headers.value; critic.extra_body_json = extra.value;
      critic.service_tier = tier.value; critic.claude_prompt_cache = cache.value;
      const k = provider === "vertex" ? keyJson.value.trim() : keyIn.value.trim();
      if (k) critic.key = k;
      if (!provider) { critic.model = ""; critic.url = ""; critic.reasoning = ""; }
      const embedding = { provider: eprovider, model: emodel.value.trim(), url: eurl.value.trim() };
      const ek = eprovider === "vertex" ? ekeyJson.value.trim() : ekey.value.trim();
      if (ek) embedding.key = ek;
      const r = await bridge("/settings/models", { method: "POST", body: JSON.stringify({ critic, embedding }) });
      if (!r.ok) return status.show(u5Error(r), false);
      keyIn.value = ""; keyJson.value = ""; ekey.value = ""; ekeyJson.value = "";
      refreshChip(); // a new critic or embedding may start work
      const keepers = (tail || []).map((b) => (b.dirty && b.dirty() ? b : null));
      body.replaceChildren();
      await modelsTab(body, keepers); // a box with unsaved edits below is kept; the others are read again
      const after = body.querySelector("[role=status]");
      if (after) { after.textContent = "저장했어요. 다음 기억 생성과 다음 요청부터 적용돼요."; after.className = "ac2-msg ac2-ok"; after.hidden = false; }
    };
    const problem = view.critic.problem ? el("div", { class: "ac2-msg ac2-err" }, "지금 설정으로는 기억을 만들 수 없어요: " + (ERR_TEXT[view.critic.problem] || view.critic.problem)) : null;
    const sourceNote = view.critic.source === "flags" ? el("div", { class: "ac2-help" }, "지금은 백엔드 실행 옵션의 설정을 쓰는 중이에요. 여기서 저장하면 이 설정을 써요.") : null;
    body.append(
      el("div", { class: "ac2-box" }, el("div", { class: "ac2-label" }, "평론가 · 대화를 읽고 기억을 만듭니다"),
        field("ac2-cprov", "제공자", pick), sourceNote, problem, fields, keyField),
      el("div", { class: "ac2-box" }, el("div", { class: "ac2-label" }, "임베딩 · 뜻이 비슷한 기억 찾기"),
        el("div", { class: "ac2-grid" }, field("ac2-eprov", "제공자", epick), field("ac2-emodelpick", "모델", emodelPick, emodel)),
        field("ac2-eurl", "주소", eurl, eurlHelp),
        el("div", { class: "ac2-field" }, ekeyLabel, ekey, ekeyJsonLabel, ekeyJson, ekeyHelp),
        el("div", { class: "ac2-help" }, "임베딩을 바꾸면 그 모델의 벡터를 새로 만들어요. 그동안 기억은 단어와 연결로 골라요.")),
      status.node,
      el("div", { class: "ac2-row" }, el("button", { class: "ac2-btn ac2-primary", on: { click: save } }, "저장")));
    draw();
    edraw();
    tail = [(keep && keep[0]) || await narrativeBox(), (keep && keep[1]) || await promptBox()];
    body.append(...tail);
  }

  // Shared parts of the preprocessing tab and the publisher box: a field, a
  // help line, a drop-down of the backend's choices, and an AI connection of
  // its own (the critic's is used unless "따로 설정").
  function setField(id, label, input, ...help) { return el("div", { class: "ac2-field" }, el("label", { class: "ac2-label", for: id }, label), input, ...help); }
  function setHelp(t) { return el("div", { class: "ac2-help" }, t); }
  function setSelect(id, label, list, cur) {
    const s = el("select", { id, "aria-label": label }, ...list.map((o) => el("option", { value: o.id }, o.label + (o.recommended ? " (권장)" : "") + (o.detail ? " · " + o.detail : ""))));
    s.value = cur || "";
    return s;
  }
  // tiers: the providers that take a service tier; limit: the seconds the
  // whole call may take at most, told beside the wait time
  function ownConn(id, title, own, ai, providers, tiers, limit) {
    const on = el("input", { type: "checkbox", id: id + "-own" });
    on.checked = !!own;
    const prov = setSelect(id + "-prov", title + " 제공자", [{ id: "", label: "선택" }].concat(providers || []), ai.provider);
    const model = el("input", { id: id + "-model", value: ai.model || "", autocomplete: "off", "aria-label": title + " 모델" });
    const url = el("input", { id: id + "-url", value: ai.url || "", autocomplete: "off", "aria-label": title + " 주소", placeholder: "비워 두면 기본 주소" });
    const reasoning = el("select", { id: id + "-reason", "aria-label": title + " 추론" });
    const key = el("input", { id: id + "-key", type: "password", autocomplete: "off", "aria-label": title + " API 키" });
    const keyHelp = setHelp("");
    // the critic's advanced options (user 11-08 "출판사도 온도 설정이나 max token
    // 같은 평론가랑 같은 설정을 가져야 하지 않을까?")
    const number = (sub, name, value, step) => el("input", { id: id + "-" + sub, type: "number", step, value: value ?? "", placeholder: "기존 기본값", "aria-label": title + " " + name });
    const timeout = number("timeout", "응답 대기 시간(초)", ai.timeout_ms == null ? "" : ai.timeout_ms / 1000, "1");
    const temperature = number("temperature", "Temperature", ai.temperature, "0.1");
    const maxTokens = number("max-tokens", "Max Tokens", ai.max_tokens, "1");
    const headers = el("textarea", { id: id + "-headers", rows: "3", class: "ac2-area", "aria-label": title + " Extra Headers JSON" });
    const extra = el("textarea", { id: id + "-body", rows: "3", class: "ac2-area", "aria-label": title + " Extra Body JSON" });
    headers.value = ai.extra_headers_json || ""; extra.value = ai.extra_body_json || "";
    const tier = el("select", { id: id + "-tier", "aria-label": title + " Service Tier" }, ...["", "standard", "flex", "priority"].map((v) => el("option", { value: v }, v || "기본")));
    tier.value = ai.service_tier || "";
    const cache = el("select", { id: id + "-cache", "aria-label": title + " Claude 프롬프트 캐시" }, ...[["", "기본"], ["off", "끔"], ["ephemeral_5m", "Automatic 5 min"], ["ephemeral_1h", "Automatic 1 hour"]].map(([value, label]) => el("option", { value }, label)));
    cache.value = ai.claude_prompt_cache || "";
    const tierField = setField(id + "-tier", "Service Tier", tier);
    const cacheField = setField(id + "-cache", "Claude 프롬프트 캐시", cache);
    const fields = el("div", { class: "ac2-field", style: "gap:12px" },
      el("div", { class: "ac2-grid" }, setField(id + "-prov", "제공자", prov), setField(id + "-model", "모델", model), setField(id + "-url", "주소", url),
        setField(id + "-reason", "추론", reasoning), setField(id + "-key", "API 키", key, keyHelp)),
      el("details", { class: "ac2-panel" }, el("summary", { class: "ac2-label", style: "cursor:pointer;min-height:32px" }, "고급 설정 · 비워 두면 기본값"),
        el("div", { class: "ac2-grid" },
          setField(id + "-timeout", "응답 대기 시간(초)", timeout, ...(limit ? [setHelp("한 요청은 길어도 " + limit + "초에서 끊겨요.")] : [])),
          setField(id + "-temperature", "Temperature", temperature), setField(id + "-max-tokens", "Max Tokens", maxTokens), tierField, cacheField),
        setField(id + "-headers", "Extra Headers (JSON)", headers), setField(id + "-body", "Extra Body (JSON)", extra)));
    let chosen = ai.reasoning || "";
    const draw = () => {
      fields.hidden = !on.checked;
      tierField.hidden = !(tiers || []).includes(prov.value);
      cacheField.hidden = prov.value !== "claude";
      const p = (providers || []).find((x) => x.id === prov.value) || {};
      reasoning.replaceChildren(...[""].concat(p.reasoning || []).map((r) => el("option", { value: r }, REASONING_TEXT[r] || r)));
      reasoning.value = chosen; // review 20 C-02
      keyHelp.textContent = ai.provider === prov.value && ai.key_set ? "저장된 키 끝 4자리 " + ai.key_last4 + " · 바꿀 때만 새로 넣으세요." : p.needs_key ? "API 키를 넣어 주세요." : "키가 필요 없으면 비워 두세요.";
    };
    on.addEventListener("change", draw);
    reasoning.addEventListener("change", () => { chosen = reasoning.value; });
    prov.addEventListener("change", () => { key.value = ""; chosen = ai.provider === prov.value ? ai.reasoning || "" : ""; draw(); });
    draw();
    return {
      node: el("div", { class: "ac2-field" }, el("div", { class: "ac2-top", style: "align-items:center" }, on, el("label", { class: "ac2-label", for: id + "-own" }, "평론가 연결 대신 따로 설정")), fields),
      own: () => on.checked,
      get: () => ({ timeout_ms: timeout.value.trim() === "" ? null : Math.round(Number(timeout.value) * 1000), temperature: temperature.value.trim() === "" ? null : Number(temperature.value),
        max_tokens: maxTokens.value.trim() === "" ? null : Number(maxTokens.value), extra_headers_json: headers.value, extra_body_json: extra.value, service_tier: tier.value, claude_prompt_cache: cache.value,
        provider: prov.value, model: model.value.trim(), url: url.value.trim(), reasoning: reasoning.value || "", ...(key.value.trim() ? { key: key.value.trim() } : {}) }),
    };
  }

  // The publisher (E03, AC1's "출판사 설정" and "입력 개선 LLM"): its
  // connection, the narrative guide and the input improvement, on the models
  // tab beside the critic and the embedding (user 11-08 "출판사와 입력 개선은
  // AC1처럼 설정에 넣는 것이 좋을 거 같네"). Its own save.
  async function narrativeBox() {
    const box = el("div", { class: "ac2-box" });
    await fillNarrative(box);
    return box;
  }
  // fills the publisher box; false when its settings could not be read
  async function fillNarrative(box) {
    const note = msgBox();
    box.dirty = null;
    box.append(el("div", { class: "ac2-label" }, "출판사 · 서사 가이드와 입력 개선 (기억을 고른 뒤 한 번 더 불러요)"));
    const r = await bridge("/settings/narrative", { method: "GET" });
    if (!r.ok) { box.append(note.node); note.show(errText(r), false); return false; }
    const nv = r.json;
    const level = setSelect("ac2-guide-level", "서사 가이드", nv.levels || [], nv.level);
    const chars = el("input", { id: "ac2-guide-chars", type: "number", min: "0", step: "500", value: String(nv.chars ?? 3000), "aria-label": "서사 가이드 예산" });
    const input = setSelect("ac2-input-mode", "입력 개선", nv.inputs || [], nv.input);
    const pubAI = ownConn("ac2-pub-ai", "출판사 AI", nv.ai_own, nv.ai || {}, nv.providers || [], nv.service_tier_providers, nv.deadline_s);
    const save = async () => {
      note.clear();
      const c = chars.value.trim();
      if (!/^\d+$/.test(c)) return note.show("서사 가이드 예산은 0 이상의 정수로 넣어 주세요.", false);
      const w = await bridge("/settings/narrative", { method: "POST", body: JSON.stringify({ level: level.value, chars: Number(c), input: input.value, ai_own: pubAI.own(), ai: pubAI.get() }) });
      if (!w.ok) return note.show("출판사 설정을 저장하지 못했어요: " + u5Error(w), false);
      // a failed read of the saved settings stays shown as it is (review 20 B-04)
      box.replaceChildren();
      if (!(await fillNarrative(box))) return;
      const done = box.querySelector("[role=status]");
      if (done) { done.textContent = "출판사 설정을 저장했어요. 다음 요청부터 적용돼요."; done.className = "ac2-msg ac2-ok"; done.hidden = false; }
    };
    const pbox = await promptBox("publisher");
    box.append(
      el("div", { class: "ac2-grid" }, setField("ac2-guide-level", "서사 가이드", level), setField("ac2-guide-chars", "가이드 예산 (자)", chars, setHelp("상한이에요. 채울 목표가 아니에요."))),
      setHelp("약하게: 고른 기억에서 이어지는 점만 참고로 · 중간: 인물별 반응 후보 더함 · 강하게: 이번 응답 전개 계획을 더하고 응답 바로 앞에 넣어 따르게 해요. 근거 없는 항목은 빼요."),
      setField("ac2-input-mode", "입력 개선", input,
        setHelp("이번 요청에만 적용돼요. 저장된 채팅은 그대로예요.")),
      el("div", { class: "ac2-label" }, "출판사 AI"), pubAI.node,
      setHelp("비밀 기억 줄은 출판사 AI에 보내지 않아요. 다만 최근 대화는 그대로 보내므로, 대화에 나온 비밀 내용은 출판사 AI도 봐요. 실패하면 가이드 없이, 입력은 원문 그대로 보내요."),
      note.node,
      el("div", { class: "ac2-row" }, el("button", { type: "button", class: "ac2-btn ac2-primary", on: { click: save } }, "출판사 설정 저장")),
      pbox);
    const sig = () => JSON.stringify([level.value, chars.value, input.value, pubAI.own(), pubAI.get()]);
    const first = sig();
    box.dirty = () => sig() !== first || pbox.dirty(); // unsaved edits
    return true;
  }

  // Preprocessing (E01~E03): how memory is chosen and the decision model.
  // Every choice, its names and checks are the backend's; this tab only shows
  // and sends them.
  async function prepTab(body) {
    const status = msgBox();
    const [pr, dr] = await Promise.all(["/settings/preprocess", "/settings/decision"].map((p) => bridge(p, { method: "GET" })));
    const bad = [pr, dr].find((r) => !r.ok);
    if (bad) { body.append(status.node); status.show(errText(bad), false); return false; }
    const pv = pr.json, dv = dr.json;
    const field = setField, help = setHelp, select = setSelect;
    // how memory is chosen
    const mode = select("ac2-prep-mode", "기억 고르기 방식", pv.modes || [], pv.mode);
    const prepAI = ownConn("ac2-prep-ai", "전처리 AI", pv.ai_own, pv.ai || {}, pv.providers || [], pv.service_tier_providers, pv.deadline_s);
    // the decision model (JEV)
    const dprov = select("ac2-dec-prov", "판정 모델", [{ id: "", label: "사용 안 함" }].concat(dv.providers || []), dv.provider);
    const durl = el("input", { id: "ac2-dec-url", value: dv.url || "", autocomplete: "off", "aria-label": "판정 모델 주소" });
    const dmodel = el("input", { id: "ac2-dec-model", value: dv.model || "", autocomplete: "off", "aria-label": "판정 모델 이름" });
    const dkey = el("input", { id: "ac2-dec-key", type: "password", autocomplete: "off", "aria-label": "판정 모델 API 키" });
    const dkeyHelp = help("");
    const dtest = msgBox();
    const dgrid = el("div", { class: "ac2-grid" }, field("ac2-dec-url", "주소", durl), field("ac2-dec-model", "모델", dmodel), field("ac2-dec-key", "API 키", dkey, dkeyHelp));
    const ddraw = () => {
      const p = (dv.providers || []).find((x) => x.id === dprov.value);
      dgrid.hidden = !p;
      if (!p) return;
      const back = dprov.value === dv.provider; // review 20 A-03
      durl.value = back ? dv.url || "" : p.default_url || ""; dmodel.value = back ? dv.model || "" : p.default_model || "";
      dkeyHelp.textContent = dv.provider === dprov.value && dv.key_set ? "저장된 키 끝 4자리 " + dv.key_last4 + " · 바꿀 때만 새로 넣으세요." : p.key_optional ? "이 PC의 Ollama가 ollama.com에 로그인되어 있으면 키 없이 써요." : "API 키를 넣어 주세요.";
    };
    dprov.addEventListener("change", () => { dkey.value = ""; ddraw(); });
    const test = async () => {
      dtest.clear();
      const r = await bridge("/settings/decision/test", { method: "POST", body: "{}" });
      if (!r.ok) return dtest.show(errText(r), false);
      dtest.show(r.json.ok ? "답을 받았어요 · " + r.json.ms + "ms" : "답을 받지 못했어요" + (r.json.error ? " · " + r.json.error : ""), !!r.json.ok);
    };
    const save = async () => {
      status.clear();
      const dec = { provider: dprov.value, url: durl.value.trim(), model: dmodel.value.trim(), ...(dkey.value.trim() ? { key: dkey.value.trim() } : {}) };
      const steps = [["판정 모델", "/settings/decision", dec],
        ["기억 고르기", "/settings/preprocess", { mode: mode.value, ai_own: prepAI.own(), ai: prepAI.get() }]];
      // each part is its own save; a failure says which parts went (review 20 B-04)
      const saved = [];
      for (const [name, path, req] of steps) {
        const r = await bridge(path, { method: "POST", body: JSON.stringify(req) });
        if (!r.ok) return status.show((saved.length ? saved.join("·") + " 설정은 저장했고, " : "") + name + " 설정을 저장하지 못했어요: " + u5Error(r), false);
        saved.push(name);
      }
      body.replaceChildren();
      // a failed read of the saved settings stays shown as it is
      if (!(await prepTab(body))) return;
      const after = body.querySelector("[role=status]");
      if (after) { after.textContent = "저장했어요. 다음 요청부터 적용돼요."; after.className = "ac2-msg ac2-ok"; after.hidden = false; }
    };
    // only the connections the chosen mode uses are shown (user 11-08 "AI +
    // JEV일때 AI설정과 JEV설정을 따로"); a hidden one keeps its saved values
    const warn = el("div", { class: "ac2-warn" }, "켜면 후보 기억 원문과 최근 대화가 판정 모델과 AI로 나가요.");
    const aiBox = el("div", { class: "ac2-box", id: "ac2-prep-ai-box" }, el("div", { class: "ac2-label" }, "AI 설정 · 관계없는 줄을 고르는 AI"), prepAI.node);
    const jevBox = el("div", { class: "ac2-box", id: "ac2-prep-jev-box" }, el("div", { class: "ac2-label" }, "판정 모델 설정 · Ollama(clef 등)나 TypeSafe Jev 중 하나"),
      field("ac2-dec-prov", "서비스", dprov),
      dgrid,
      el("div", { class: "ac2-row" }, sbtn("연결 시험 (저장된 설정으로 1회 호출)", test)), dtest.node);
    const showModes = () => {
      aiBox.hidden = !["ai", "ai_jev"].includes(mode.value);
      jevBox.hidden = !["jev", "ai_jev"].includes(mode.value);
      warn.hidden = mode.value === "";
    };
    mode.addEventListener("change", showModes);
    body.append(
      el("div", { class: "ac2-box" }, el("div", { class: "ac2-label" }, "기억 고르기 · Go가 고른 기억을 모델이 한 번 더 고릅니다"),
        field("ac2-prep-mode", "방식", mode,
          help("판정 모델: 관계없는 기억을 빼고 이어진 기억을 더해요(턴마다 판정 요청 평균 3~4개). 판정 모델은 Ollama(clef 등)나 TypeSafe Jev 중 고를 수 있어요. AI: AI가 관계없는 줄을 빼요(턴마다 1회). AI + 판정 모델: 둘 다 관계없다고 한 줄만 빼요."),
          help("넣는 기억은 저장된 원문 그대로예요. 실패하면 Go가 고른 그대로 넣고 응답은 막지 않아요.")),
        warn),
      aiBox,
      jevBox,
      setHelp("서사 가이드와 입력 개선(출판사)은 '모델' 탭에 있어요."),
      status.node,
      el("div", { class: "ac2-row" }, el("button", { class: "ac2-btn ac2-primary", on: { click: save } }, "저장")));
    ddraw();
    showModes();
    return true;
  }

  // Status (9-3): every state and count is the backend's /status view; the
  // texts below only name its codes.
  const CHIP_TEXT = {
    done: "기억 정리 완료", generating: "기억 만드는 중", retrying: "실패한 기억 다시 시도 대기 중",
    embedding_failed: "임베딩 실패 · 최근 순서로만 고르는 중", given_up: "다시 시도를 다 쓴 턴이 있어요",
    no_critic: "평론가가 설정되지 않았어요", offline: "백엔드에 연결할 수 없어요",
  };
  const CHIP_COLOR = { done: "#2F8F5B", generating: "#0E6B63", retrying: "#B7791F", embedding_failed: "#C53030", given_up: "#C53030", no_critic: "#7A776E", offline: "#7A776E" };
  const ACTIVITY_TEXT = {
    injected: "기억을 넣음", made: "기억을 만듦", failed: "기억 만들기 실패", given_up: "다시 시도를 다 씀",
    index_failed: "기억 임베딩 실패", query_failed: "입력 임베딩 실패", rearmed: "평론가가 바뀌어 다시 시도하는 턴",
    memory_edited: "기억을 고침", memory_deleted: "기억을 지움", memory_held: "기억 고정·억제를 바꿈",
  };
  const VECTOR_TEXT = { off: "꺼짐 (키 없음)", ok: "정상", failing: "실패 중", unknown: "아직 호출 없음" };
  const WARNING_TEXT = { query_vector_off: "임베딩 꺼짐 · 최근 순서", query_vector_failed: "임베딩 실패 · 최근 순서", recent_query_vector_failed: "직전 턴 질의 실패 · 입력 유사도와 최근 순서", no_input_text: "입력 없음 · 최근 순서" };
  const RECENT_QUERY_TEXT = { true: "사용", false: "사용 안 함" };
  const SOURCE_TEXT = { saved: "설정 화면", flags: "실행 옵션", off: "꺼 둠", none: "없음" };
  function clock(at) {
    const d = new Date(at);
    return isNaN(d) ? "" : d.toLocaleTimeString();
  }
  // The chat now open on the host ("" when it cannot be read); the status is
  // asked for this chat.
  async function currentChatId() {
    return (await currentChat()).id;
  }
  // The chat the memories and worldline tabs show: the one chosen in the
  // chats tab, else the open chat (user decisions 10-28). open is the open
  // chat's id; isOpen tells whether the chat shown is it.
  let viewChat = null; // { id, name } or null
  async function chatInView() {
    const open = await currentChat();
    const v = viewChat && viewChat.id ? viewChat : open;
    return { id: v.id, name: v.name || "", open: open.id, isOpen: !!v.id && v.id === open.id };
  }
  // chooses the chat the memories and worldline tabs show (null = the open chat)
  function showChat(chat) {
    viewChat = chat && chat.id ? { id: chat.id, name: chat.name || "" } : null;
  }
  // the open chat's id and the name the host shows for it
  async function currentChat() {
    try {
      const c = await coords();
      const chat = await api.getChatFromIndex(c.character_index, c.chat_index);
      return chat && chat.id != null ? { id: String(chat.id), name: String(chat.name || "") } : { id: "", name: "" };
    } catch (_) {
      return { id: "", name: "" };
    }
  }
  function statusPath(chatId) {
    return "/status" + (chatId ? "?chat_id=" + encodeURIComponent(chatId) : "");
  }
  function line(label, value) {
    return el("div", { class: "ac2-kv" }, el("span", { class: "ac2-help" }, label), el("span", {}, String(value)));
  }

  async function updateTab(body) {
    const box = el("section", { class: "ac2-box" });
    const actions = el("div", { class: "ac2-acts" });
    const note = msgBox();
    const states = { idle: "대기", ok: "확인 완료", accepted: "재시작 요청됨", pending_next_start: "다음 시작에 적용",
      applying: "적용 중", applied_pending_health: "재시작 확인 중", committed: "업데이트 완료", rolled_back: "이전 프로그램으로 복원됨" };
    const draw = (v) => {
      box.replaceChildren(el("h2", { class: "ac2-h2" }, "프로그램 업데이트"));
      actions.replaceChildren();
      if (v.status === "not_configured") {
        box.append(el("div", { class: "ac2-help" }, v.message || "업데이트 위치가 설정되지 않았어요."));
        return;
      }
      if (v.status) box.append(line("상태", states[v.status] || v.status));
      if (v.current_version) box.append(line("현재 버전", v.current_version));
      if (v.latest_version) box.append(line("새 버전", v.latest_version));
      if (v.target_version) box.append(line("적용 대상", v.target_version));
      if (v.message) box.append(el("div", { class: "ac2-help" }, v.message));
      actions.append(sbtn("업데이트 확인", () => run("/update/check")), sbtn("상태 새로고침", () => run("/update/status")));
      if (v.update_available && v.apply_supported) actions.append(sbtn("지금 업데이트", () => run("/update/apply"), "ac2-primary"));
      box.append(el("div", { class: "ac2-help" }, "백엔드 프로그램을 바꾸고 다시 시작해요. RisuAI 플러그인은 새 파일을 다시 가져와 주세요."));
    };
    const run = async (path) => {
      for (const b of actions.children) b.disabled = true;
      note.show(path === "/update/apply" ? "업데이트를 내려받고 확인하고 있어요." : "확인 중이에요.", true);
      // The Go request owns release lookup, download, verification and staging.
      // Like AC1, these two operations wait for its result without the short
      // observation timeout. A manual status refresh reads the restart result.
      const res = await bridge(path, path === "/update/apply" ? { method: "POST", body: "{}", timeoutMs: 0 }
        : { method: "GET", ...(path === "/update/check" ? { timeoutMs: 0 } : {}) });
      for (const b of actions.children) b.disabled = false;
      if (!res.ok) return note.show((res.json && res.json.message) || errText(res), false);
      draw(res.json || {});
      note.clear();
    };
    draw({});
    body.append(box, actions, note.node);
    await run("/update/status");
  }

  // Go masks credential values in the combined report; everything else goes as
  // it is (user 11-08). Offline the plugin's own errors go as recorded.
  // Offline the backend cannot mask, so credential values are masked here
  // the way Go does (keys, tokens, passwords, DSN and URL credentials, the
  // saved connection token); everything else stays (user 11-08).
  function maskCredentials(text, token) {
    let t = String(text);
    if (token) for (const v of new Set([token, encodeURIComponent(token), encodeURI(token)])) t = t.split(v).join("[가림]");
    // a DSN: the password alone when it has no space or "@", else the whole
    // connection part (as Go, review 22 A-2)
    t = t.replace(/([^\s"':@]+):[^\s@]+@((?:tcp|unix)\([^)]*\))/g, "$1:[가림]@$2")
      .replace(/[^\s"':@/]+:(?!\s|\[가림\]@)(?:(?!\[가림\])[^\r\n@/])*@(?:tcp|unix)\([^)]*\)/g, "[접속 정보 가림]")
      .replace(/(IDENTIFIED\s+BY\s+['"])[^'"]*/gi, "$1[가림]");
    return t.replace(/(bearer\s+)[^\s"',;]+/gi, "$1[가림]")
      .replace(/((?:api[_ -]?key|access[_ -]?token|authorization|[a-z_]*password|passwd|secret|token)["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s"',;&}]+)/gi, "$1[가림]")
      .replace(/(https?:\/\/)[^/\s:@]+:[^/\s@]+@/gi, "$1[접속 정보 가림]@")
      .replace(/(?:ac2-(?:[a-z2-7]{6}-){3}[a-z2-7]{6}|sk-[A-Za-z0-9_-]{8,}|AIza[A-Za-z0-9_-]{20,})/g, "[접속 정보 가림]");
  }
  async function collectDiagnosticReport() {
    const c = await conn();
    let hostname = "";
    try { hostname = new URL(c.url).hostname; } catch (_) {}
    const plugin = { version: PLUGIN_VERSION, connection_hostname: maskCredentials(hostname, c.token), recent_errors: pluginErrors.slice(-40) };
    if (runtime && typeof runtime.platform === "string") plugin.host_type = runtime.platform;
    // getRuntimeInfo exposes the API version, not the host application's version.
    const res = await bridge("/diagnostics/report", { method: "POST", body: JSON.stringify({ plugin }) });
    if (res.ok) return res.json;
    const kind = ["connect_failed", "body_read_failed", "decode_failed", "http_error", "timeout"].includes(res.kind) ? res.kind : "report_unavailable";
    const code = typeof res.code === "string" && Object.prototype.hasOwnProperty.call(ERR_TEXT, res.code) ? res.code : "";
    const mask = (x) => maskCredentials(x, c.token);
    return { contract_version: "ac2.diagnostics.v1", generated_at: new Date().toISOString(), backend_report_available: false,
      plugin: { ...plugin, recent_errors: plugin.recent_errors.map((e) => ({ ...e, message: mask(e.message) })) },
      backend_report_error: mask([res.kind || kind, Number.isFinite(res.status) ? res.status : "", typeof res.code === "string" ? res.code : code, res.detail || (res.json && res.json.message) || ""].filter(Boolean).join(" · ")) };
  }
  function diagnosticsBox() {
    const note = msgBox();
    const save = el("button", { class: "ac2-btn", on: { click: async () => {
      save.disabled = true;
      let url;
      try {
        const report = await collectDiagnosticReport();
        url = URL.createObjectURL(new Blob([JSON.stringify(report, null, 2)], { type: "application/json" }));
        const link = el("a", { href: url, download: "AC2-diagnostics-" + new Date().toISOString().replace(/[:.]/g, "-") + ".json" });
        document.body.append(link); link.click(); link.remove();
        note.show(report.backend_report_available === false ? "연결 실패 정보와 플러그인 정보를 저장했어요." : "진단 보고서를 저장했어요.", true);
      } catch (e) {
        recordPluginError(e);
        note.show("파일을 저장하지 못했어요. 다시 시도해 주세요.", false);
      } finally {
        if (url) URL.revokeObjectURL(url);
        save.disabled = false;
      }
    } } }, "진단 보고서 저장");
    return el("div", { class: "ac2-box" }, el("div", { class: "ac2-row" }, save),
      el("div", { class: "ac2-help" }, "오류 원문과 로그가 그대로 들어 있어요(대화·기억 내용이 섞일 수 있어요). API 키와 접속 토큰 값만 가려요. 다른 사람에게 보내기 전에 한 번 확인해 주세요."), note.node);
  }

  async function statusTab(body) {
    const msg = msgBox();
    const res = await bridge(statusPath(await currentChatId()), { method: "GET" });
    const refresh = el("button", { class: "ac2-btn", on: { click: async () => { body.replaceChildren(); await statusTab(body); } } }, "새로고침");
    if (!res.ok) {
      setChip("offline");
      body.append(msg.node, diagnosticsBox(), el("div", { class: "ac2-row" }, refresh));
      return msg.show(errText(res), false);
    }
    const v = res.json;
    setChip(v.chip);
    const c = await conn();
    const g = v.generation, e = v.embedding, inj = v.injection;
    const head = el("div", { class: "ac2-box" },
      el("div", { class: "ac2-chip" }, el("span", { class: "ac2-dot", "data-chip": v.chip }), el("span", { class: "ac2-label" }, CHIP_TEXT[v.chip] || v.chip)),
      line("백엔드", c.url + " · " + v.version));
    const yn = (b) => (b ? "예" : "아니요");
    const gen = el("div", { class: "ac2-box" }, el("div", { class: "ac2-label" }, "기억 생성"),
      line("평론가", g.critic.provider ? g.critic.provider + " · " + g.critic.model + " (" + (SOURCE_TEXT[g.critic.source] || g.critic.source) + ")" : "없음"),
      ...(v.chat ? [el("div", { class: "ac2-help" }, "이 채팅"), line("만드는 중", yn(g.chat.running || g.chat.waiting)),
        line("다시 시도 대기", yn(g.chat.retry_waiting)), line("다시 시도를 다 쓴 턴", g.chat.given_up)] : []),
      el("div", { class: "ac2-help" }, "백엔드 전체"),
      line("지금 기억을 만드는 채팅", g.running_chats || 0), line("기다리는 채팅", g.waiting_chats), line("다시 시도 대기 채팅", g.retry_waiting_chats), line("다시 시도를 다 쓴 턴", g.given_up_turns),
      line("이번 실행에서 만든 턴", g.made), line("실패", g.failures),
      g.critic.problem ? el("div", { class: "ac2-msg ac2-err" }, ERR_TEXT[g.critic.problem] || g.critic.problem) : null,
      g.last_error ? el("div", { class: "ac2-help" }, "마지막 오류: " + g.last_error) : null);
    const injBox = el("div", { class: "ac2-box" }, el("div", { class: "ac2-label" }, "이 채팅의 마지막 기억 넣기"),
      ...(inj ? [line("시각", clock(inj.at)), line("기억", inj.items + "개"), line("기억 예산", inj.chars + " / " + inj.chars_max + "자"),
        line("비밀 예산", inj.secret_chars + " / " + inj.secret_max + "자"), line("걸린 시간", inj.total_ms + "ms"),
        line("직전 턴 질의", RECENT_QUERY_TEXT[inj.recent_query_used] || "미관측"),
        inj.warning ? line("고른 방식", WARNING_TEXT[inj.warning] || inj.warning) : null]
        : [el("div", { class: "ac2-help" }, v.chat ? "백엔드가 켜진 뒤 이 채팅의 요청이 아직 없어요." : "열린 채팅을 읽지 못했어요.")]));
    const vec = el("div", { class: "ac2-box" }, el("div", { class: "ac2-label" }, "임베딩"),
      line("상태", VECTOR_TEXT[e.state] || e.state), line("모델", e.model || "없음"), line("지금 벡터를 만드는 채팅", e.running_chats || 0), line("벡터 저장을 기다리는 기억", e.waiting_vectors), line("벡터를 다시 만들 채팅", e.backlog_chats),
      e.index_failing ? el("div", { class: "ac2-msg ac2-err" }, "기억 임베딩이 실패하고 있어요. 다시 연결되면 빠진 벡터를 자동으로 채워요." + (e.index_detail ? " (" + e.index_detail + ")" : "")) : null,
      e.query_failing ? el("div", { class: "ac2-msg ac2-err" }, "입력 임베딩이 실패하고 있어요. 그동안 기억은 최근 순서로 골라요." + (e.query_detail ? " (" + e.query_detail + ")" : "")) : null);
    const list = el("div", { class: "ac2-box" }, el("div", { class: "ac2-label" }, "최근 활동"),
      ...(v.activity.length ? v.activity.map((a) => el("div", { class: "ac2-act" },
        el("span", { class: "ac2-help" }, clock(a.at)),
        el("span", {}, (ACTIVITY_TEXT[a.kind] || a.kind) + (a.turn ? " · " + a.turn + "턴" : "") + (a.kind === "rearmed" && a.detail ? " " + a.detail + "개" : "")),
        a.chat ? el("span", { class: "ac2-help" }, a.chat.slice(0, 8)) : null,
        a.detail && a.kind !== "rearmed" ? el("div", { class: "ac2-help ac2-wide" }, WARNING_TEXT[a.detail] || a.detail) : null))
        : [el("div", { class: "ac2-help" }, "백엔드가 켜진 뒤 아직 활동이 없어요. 목록은 최근 20개만 백엔드 메모리에 둡니다.")]));
    body.append(head, el("div", { class: "ac2-grid" }, gen, injBox, vec), list, maintenanceBox(), diagnosticsBox(), msg.node, el("div", { class: "ac2-row" }, refresh));
  }

  // Cold start (user decision 10-28; AC1's session normalize panel): the
  // open chat's earlier turns without memories (history from before the
  // plugin) are made on the backend, in the background; which turns is the
  // backend's choice. The panel shows the run's progress and can cancel it.
  const COLD_POLL_MS = 5000;
  // A fresh snapshot of the open chat for the cold start, as an observation
  // batch (null for a chat not open: the host is read by coordinates). With
  // again, the texts the plugin marked sent go once more.
  async function coldSnapshot(chat, again) {
    const c = await coords();
    let cur;
    try { cur = await api.getChatFromIndex(c.character_index, c.chat_index); } catch (_) { return null; }
    if (!cur || String(cur.id ?? "") !== chat) return null;
    if (again) for (const k of [...lastSent.keys()]) if (k.startsWith(chat + "|")) lastSent.delete(k);
    const pending = [];
    const ev = await record("load", { coords: c, reason: "cold_start", stored_chat: await snapshotFromChat(cur, pending, await captureText(), c.character_index) }, pending);
    return unsentThrough(ev);
  }
  function coldStart(chat, name) {
    const n = el("input", { id: "ac2-coldn", type: "number", min: "1", step: "1", placeholder: "전체", "aria-label": "최근 몇 턴", style: "width:120px" });
    const out = msgBox();
    const slot = el("div");
    const progress = el("div", { class: "ac2-field" });
    const panel = el("section", { class: "ac2-box" });
    let timer = null;
    let reads = 0; // only the latest read draws (review 07)
    const show = async () => {
      const mine = ++reads;
      clearTimeout(timer);
      const res = await bridge("/memory/backfill?chat_id=" + encodeURIComponent(chat), { method: "GET" });
      if (mine !== reads) return;
      if (!res.ok) { progress.replaceChildren(); return out.show(errText(res), false); }
      const p = res.json;
      if (!p.total) { progress.replaceChildren(); return; }
      const fails = p.failures || [];
      const left = p.waiting > 0;
      const doneShare = Math.round(100 * (p.done + p.failed) / p.total);
      progress.replaceChildren(...nodes(
        el("div", { class: "ac2-top" }, el("div", { class: "ac2-main ac2-label" }, left ? "진행 중" : p.failed ? "오류를 포함해 끝남" : p.cancelled ? "취소됨" : "끝남"),
          el("span", { class: "ac2-num" }, (p.done + p.failed) + " / " + p.total + "턴")),
        el("div", { style: "height:8px;border-radius:4px;background:var(--line2);overflow:hidden" }, el("div", { style: "height:8px;background:var(--acc);width:" + doneShare + "%" })),
        el("div", { class: "ac2-stats" }, el("div", { class: "ac2-stat" }, "만듦", el("b", {}, String(p.done))), el("div", { class: "ac2-stat" }, "실패", el("b", {}, String(p.failed))),
          el("div", { class: "ac2-stat" }, "남음", el("b", {}, String(p.waiting))), p.cancelled ? el("div", { class: "ac2-stat" }, "취소됨", el("b", {}, String(p.cancelled))) : null),
        ...fails.slice(0, 3).map((f) => el("div", { class: "ac2-warn" }, f.turn + "턴 실패" + (f.error ? ": " + f.error : ""))),
        fails.length > 3 ? el("div", { class: "ac2-help" }, "실패한 턴 " + (fails.length - 3) + "개 더") : null,
        left ? el("div", { class: "ac2-help" }, "이 화면을 닫아도 백엔드에서 계속 진행돼요. 만들어진 기억부터 바로 쓰여요.") : null,
        el("div", { class: "ac2-acts" }, sbtn("새로고침", show), left ? sbtn("작업 취소", stop, "ac2-danger") : null)));
      // while turns are left, asked again as long as the panel is shown
      if (left && panel.isConnected) timer = setTimeout(show, COLD_POLL_MS);
    };
    const stop = async () => {
      const res = await bridge("/memory/backfill/cancel", { method: "POST", body: JSON.stringify({ chat_id: chat }) });
      if (!res.ok) return out.show(errText(res), false);
      out.show("남은 턴을 취소했어요. 지금 만드는 턴은 끝까지 만들어요.", true);
      await show();
    };
    const ask = () => {
      out.clear();
      const how = n.value.trim() ? "최근 " + n.value.trim() + "턴" : "아직 기억이 없는 지난 턴 전체";
      askHere(slot, "대상 채팅: " + (name || chat.slice(0, 8)) + " · " + how + "의 기억을 만듭니다. 턴마다 평론가를 한 번씩 부르므로 그만큼 비용이 들어요.", "실행", async () => {
        const max = Number(n.value.trim() || 0);
        const ask = async (fresh) => bridge("/memory/backfill", { method: "POST", body: JSON.stringify({ chat_id: chat, max_turns: max, order: max ? "recent" : "oldest", observation: fresh }) });
        // the open chat goes along as the host has it now, texts included,
        // so a chat the backend lost (a reset) or never saw is read first;
        // texts the backend asks for again go once more
        let res = await ask(await coldSnapshot(chat, false));
        if (res.ok && res.json && res.json.resend_text) res = await ask(await coldSnapshot(chat, true));
        if (!res.ok) return res;
        const turns = (res.json && res.json.turns) || [];
        if (!turns.length) out.show("만들 지난 턴이 없어요. 이미 기억을 만들었거나 대화 중에 만들어진 턴이에요.", true);
        else { out.show(turns.length + "개 턴의 기억을 만들기 시작했어요.", true); refreshChip(); await refreshShown(); }
        return { ok: true };
      }, true);
    };
    panel.append(el("h2", { class: "ac2-h2" }, "지난 대화로 기억 만들기"),
      el("div", { class: "ac2-help" }, "플러그인을 넣기 전에 진행한 대화처럼, 이 채팅에서 기억이 없는 지난 턴의 기억을 만들어요. 몇 턴만 하려면 수를 넣으세요(최근 턴부터). 비우면 전체를 오래된 순으로 만들어요."),
      el("div", { class: "ac2-acts" }, el("label", { class: "ac2-help", for: "ac2-coldn" }, "최근 몇 턴"), n, el("button", { type: "button", class: "ac2-btn ac2-primary", on: { click: ask } }, "콜드 스타트 실행")),
      slot, out.node, progress);
    show();
    liveView(panel, show);
    return panel;
  }

  // E12 HypaMemory import (user 11-07: when the chat has HypaMemory
  // summaries and the button is pressed, all of them, duplicates or not).
  // The host's summaries go to Go as they are; Go makes them into AC2
  // memory and attaches them before this chat's turns.
  async function hostHypaSummaries(chat) {
    const c = await coords();
    const cur = await api.getChatFromIndex(c.character_index, c.chat_index);
    if (!cur || String(cur.id ?? "") !== chat) return null; // only the open chat is read from the host
    const sums = cur.hypaV3Data && Array.isArray(cur.hypaV3Data.summaries) ? cur.hypaV3Data.summaries : [];
    return sums.map((x) => ({ text: String((x && x.text) || ""), important: !!(x && x.isImportant), tags: Array.isArray(x && x.tags) ? x.tags.map(String) : [], category: String((x && x.categoryId) || "") }));
  }
  function hypaImport(chat, name) {
    const out = msgBox();
    const slot = el("div");
    const ask = async () => {
      out.clear();
      if (busyHere(slot)) return;
      let sums;
      try { sums = await hostHypaSummaries(chat); } catch (e) { recordPluginError(e); return out.show("RisuAI에서 채팅을 읽지 못했어요.", false); }
      if (sums === null) return out.show("지금 열려 있는 채팅에서만 가져올 수 있어요. 이 채팅을 연 뒤 다시 눌러 주세요.", false);
      if (!sums.some((x) => x.text.trim())) return out.show("이 채팅에는 HypaMemory 요약이 없어요.", false);
      const body = { chat_id: chat, chat_name: name || "", summaries: sums };
      const p = await bridge("/memory/hypa", { method: "POST", body: JSON.stringify({ ...body, dry_run: true }) });
      if (!p.ok) return out.show(errText(p), false);
      askHere(slot, "HypaMemory 요약 " + p.json.summaries + "개를 가져와 AC2 기억으로 다시 만들어요. 이 중 " + p.json.to_make + "개는 새로 만들어 요약마다 평론가를 한 번씩 불러 비용이 들어요. 가져온 요약은 이 채팅 앞의 연결 기록으로 붙고, 같은 내용의 기억이 이미 있어도 그대로 가져와요.", "가져오기", async () => {
        // waits for the backend to finish (it may wait for a turn being made), as attaching does (review 15 #2)
        const r = await bridge("/memory/hypa", { method: "POST", body: JSON.stringify(body), timeoutMs: 0 });
        if (!r.ok) return r;
        out.show("요약 " + r.json.summaries + "개를 가져왔어요. " + ((r.json.turns || []).length ? (r.json.turns || []).length + "개의 기억을 백엔드에서 만들고 있어요." : "새로 만들 요약은 없었어요."), true);
        refreshChip(); await refreshShown();
        return { ok: true };
      }, true);
    };
    return el("section", { class: "ac2-box" }, el("h2", { class: "ac2-h2" }, "HypaMemory 가져오기"),
      el("div", { class: "ac2-help" }, "채팅 본문이 없어 지난 대화로 만들 수 없을 때, RisuAI HypaMemory 요약을 읽어 AC2 기억으로 다시 만들어요. 다시 가져오면 바뀌거나 새로 생긴 요약만 새로 만들어요."),
      el("div", { class: "ac2-acts" }, el("button", { type: "button", class: "ac2-btn ac2-primary", on: { click: ask } }, "HypaMemory 가져오기")), slot, out.node);
  }

  // The chat chip: a floating action button (the host shows its icon only)
  // whose dot is the backend's chip state; it opens the status tab.
  let chipState = "";
  function chipIcon(state) {
    const outline = '<rect x="2" y="3" width="14" height="14" rx="4"/><path d="M2 7h14M6 11h6"/>';
    const svg = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20"><g fill="none" stroke="#fff" stroke-opacity=".6" stroke-width="3">' + outline + '</g><g fill="none" stroke="#3A3A3A" stroke-width="1.6">' + outline + '</g><circle cx="15.5" cy="15.5" r="3.5" fill="' + (CHIP_COLOR[state] || "#7A776E") + '" stroke="#fff" stroke-width="1.5"/></svg>';
    return "data:image/svg+xml," + encodeURIComponent(svg);
  }
  function setChip(state) {
    if (!state || state === chipState || unloaded) return;
    chipState = state;
    // the same id replaces the button in place; a failure is tried on the
    // next poll
    (async () => api.registerButton({ name: "Archive Center 2 · " + (CHIP_TEXT[state] || state), icon: chipIcon(state), iconType: "img", location: "action", id: "ac2-chip" },
      () => { settingsTab = "status"; openSettings(); }))().catch((e) => { recordPluginError(e); chipState = ""; console.warn("[AC2] chip", e); });
  }
  // The chip is asked for after each memory request and, while its state is
  // one that changes by itself, again every CHIP_POLL_MS.
  const CHIP_POLL_MS = 15000;
  const CHIP_CHANGING = new Set(["generating", "retrying", "embedding_failed", "offline"]);
  let chipTimer = null;
  let chipAsking = false;
  let chipAgain = false;
  async function pollChip() {
    chipTimer = null;
    if (unloaded) return;
    // asked while a status is on its way: that answer may predate what was
    // just sent, so the chip asks once more after it
    if (chipAsking) { chipAgain = true; return; }
    chipAsking = true;
    chipAgain = false;
    let state = "offline";
    try {
      const res = await bridge(statusPath(await currentChatId()), { method: "GET" });
      if (res.ok) state = res.json && res.json.chip;
    } finally {
      chipAsking = false;
    }
    setChip(state);
    if (chipAgain) return pollChip();
    if (!unloaded && !chipTimer && CHIP_CHANGING.has(state)) chipTimer = setTimeout(pollChip, CHIP_POLL_MS);
  }
  function refreshChip() {
    if (chipTimer) { clearTimeout(chipTimer); chipTimer = null; }
    pollChip().catch((e) => { recordPluginError(e); console.warn("[AC2] chip", e); });
  }

  // Memories (9-4): the open chat's memories from the backend; a memory of
  // the chat's own turns can have its body and importance edited or be
  // deleted. A branch's memories from its parent are read only here.
  const KIND_TEXT = { event: "사건", fact: "사실", view: "주관 기억" };
  const MEM_PAGE = 30;
  // A memory as a card, with edit and delete for the chat's own memory.
  // A memory as a row: its body, who/what it is about, its kind; details
  // (effects, who knows) open under it, and editing and deleting happen in
  // place. A branch's memory read from its parent is read only.
  const KIND_TONE = { event: "blu", fact: "", view: "vio" };
  const ASPECT_TEXT = { belief: "믿음", memory: "회상", interpretation: "해석", emotion: "감정", motive: "동기", goal: "목표", intent: "의도" };
  const OUTCOME_TEXT = { attempt: "시도", success: "성공", partial: "일부 성공", failed: "실패", interrupted: "중단됨", cancelled: "취소됨" };
  const DIFFERS_TEXT = { yes: "사실과 달라요", no: "사실과 같아요", unknown: "사실과 같은지 몰라요" };
  const ROLE_TEXT = [["actor", "한 사람"], ["target", "받은 사람"], ["object", "물건"], ["place", "장소"], ["speaker", "말한 사람"], ["listener", "들은 사람"]];
  // the stored details of a memory as labelled lines, only those it has
  function memoryDetails(m) {
    const join = (v) => (Array.isArray(v) ? v : [v]).filter(Boolean).join(", ");
    const rows = [];
    const add = (label, v) => { if (v) rows.push([label, v]); };
    add("근거 원문", m.q ? "“" + m.q + "”" : "");
    // when it happened (C12): its own time, the time of the event it comes from, or a recorded date
    add("일어난 때", m.when ? m.when + ({ at: " (근거 사건의 때)", turn: " (기록된 날로 채움)" }[m.when_from] || "") : m.time);
    add("근거 사건", m.at);
    add("기록된 날", m.date);
    add("결과", OUTCOME_TEXT[m.outcome] || m.outcome);
    add("상황", m.setting);
    for (const [k, label] of ROLE_TEXT) add(label, m.roles && join(m.roles[k]));
    add("본 사람", join(m.seen_by));
    add("대상", join(m.to));
    add("범위", m.scope);
    add("조건", m.terms);
    add("상태", m.status);
    if (m.basis) add("아는 경로", [m.basis.via, m.basis.from ? m.basis.from + "에게서" : "", m.basis.confidence ? "확신 " + m.basis.confidence : ""].filter(Boolean).join(" · "));
    if (m.affect) add("감정의 세기", [m.affect.intensity, m.affect.persistence].filter(Boolean).join(" · "));
    add("겉으로 드러냄", m.expressed);
    add("사실과 비교", DIFFERS_TEXT[m.differs] || m.differs);
    for (const e of m.effects || []) add("효과", [e.about, e.slot, [e.before, e.value].filter(Boolean).join(" → "), e.delta, e.status].filter(Boolean).join(" "));
    add("앎", (m.awareness || []).map((a) => ({ knows: "앎", unaware: "모름", hide_from: "숨김", allow_to: "알려도 됨" }[a.kind] || a.kind) + ": " + a.who).join(" · "));
    if (m.imp != null) add("중요도", String(m.imp));
    return rows;
  }
  // C18 flags as Go stores them (pin: put in first; mute: never put in)
  const HOLD_TEXT = { pin: "고정", mute: "억제" };
  const HOLD_DONE = { pin: "고정했어요. 다음 응답부터 예산 안에서 먼저 넣어요(지금 대화에 보이는 턴이나 새 값으로 바뀐 기억은 넣지 않아요). 이 턴을 다시 만들면 고정이 풀려요.",
    mute: "억제했어요. 다음 응답부터 이 기억은 넣지 않아요. 이 턴을 다시 만들면 억제가 풀려요.", "": "풀었어요. 다음 응답부터 평소처럼 골라요." };
  // a memory's body, turn and who, and its tags
  function memoryHead(m, pick) {
    const who = [m.owner ? m.owner + "의 기억" : "", m.about, m.slot].filter(Boolean).join(" · ");
    const sub = m.kind === "view" ? ASPECT_TEXT[m.aspect] || m.aspect : m.mode;
    return el("div", { class: "ac2-top" }, pick || null,
      el("div", { class: "ac2-main" }, el("div", { class: "ac2-text" }, m.body),
        el("div", { class: "ac2-meta" }, [m.turn + "턴", who].filter(Boolean).join(" · "))),
      el("div", { class: "ac2-tags" }, tag(KIND_TEXT[m.kind] || m.kind, KIND_TONE[m.kind]), sub ? tag(sub) : null, m.secret ? tag("비밀", "vio") : null, m.replaced ? tag("바뀐 값") : null, Array.isArray(m.changed) && m.changed.length ? tag("일부 바뀜: " + m.changed.join(", ")) : null, m.hold ? tag(HOLD_TEXT[m.hold] || m.hold, m.hold === "pin" ? "acc" : "warn") : null, m.inherited ? tag("물려받음") : null));
  }
  // One memory's own turn text and what it points to (C15), read from Go
  // only when asked; related records are one step away, a page at a time.
  const REF_TEXT = { at: "근거 사건", "basis.ref": "생각의 근거", scene: "장면", effects: "바뀐 값" };
  const REL_TEXT = { causes: "원인", condition_of: "조건", changes: "바꿈", fulfills: "이룸", recalls: "떠올림", contradicts: "어긋남", same_fact: "같은 사실" };
  const AWARE_TEXT = { knows: "앎", hide_from: "숨김", unaware: "모름", allow_to: "알려도 됨" };
  const STATUS_TEXT = { missing: "기록 없음", deleted: "지워짐", out_of_scope: "이 채팅 범위 밖" };
  function evidenceBox(chatId, m) {
    const head = el("div", { class: "ac2-field" });
    const list = el("div", { class: "ac2-list" });
    const more = el("div", { class: "ac2-row" });
    let cursor = "", first = true;
    const load = async () => {
      more.replaceChildren();
      const r = await bridge("/memory/detail?chat_id=" + encodeURIComponent(chatId) + "&turn_id=" + m.turn_id + "&id=" + encodeURIComponent(m.id) + "&limit=20" + (cursor ? "&cursor=" + encodeURIComponent(cursor) : ""), { method: "GET" });
      if (!r.ok) {
        const why = r.code === "memory_not_found" ? "이 기억을 찾지 못했어요. 지워졌거나 이 채팅 범위 밖이에요." : r.code === "store_failed" ? "원문과 근거를 읽지 못했어요." : errText(r);
        more.append(el("span", { class: "ac2-help" }, why), sbtn("다시 시도", load));
        return;
      }
      const d = r.json, name = (x) => (d.labels && d.labels[x]) || x;
      if (first) {
        first = false;
        const src = d.source || {};
        head.append(...nodes(
          // the place in this chat and where it was made are told apart: an
          // inherited memory was made in the parent chat
          el("div", { class: "ac2-help" }, d.inherited ? "이 채팅 " + d.turn + "턴 자리 · 부모 채팅의 기록에서 물려받음" : d.turn + "턴에서 만든 기억"),
          ...(src.members || []).map((x) => el("div", {}, el("div", { class: "ac2-label" }, "입력"), el("div", { class: "ac2-pre" }, x.data))),
          ...(src.responses || []).map((x) => el("div", {}, el("div", { class: "ac2-label" }, "응답"), el("div", { class: "ac2-pre" }, x.data))),
          el("div", { class: "ac2-label" }, "가리키는 기록")));
      }
      for (const x of d.related || []) {
        let what = "";
        if (x.status !== "available") what = STATUS_TEXT[x.status] || x.status;
        else if (x.memory) what = x.memory.body || "";
        else if (x.effect) what = [x.effect.slot, [x.effect.before, x.effect.value].filter((v) => v != null && v !== "").join(" → ")].filter(Boolean).join(": ");
        else if (x.link) what = (REL_TEXT[x.link.rel] || x.link.rel) + " · " + x.link.from + " → " + x.link.to +
          (x.endpoints || []).filter((e) => e.status !== "available").map((e) => " (" + e.id + " " + (STATUS_TEXT[e.status] || e.status) + ")").join("");
        else if (x.awareness) what = name(x.awareness.who) + " " + (AWARE_TEXT[x.awareness.kind] || x.awareness.kind);
        const label = x.kind === "link" ? "연결" : x.kind === "awareness" ? "누가 아는가" : REF_TEXT[x.field] || (/^(effects\.\d+\.)?was\./.test(x.field || "") ? "이전 값" : x.field || x.kind);
        list.append(el("div", { class: "ac2-item" }, el("div", { class: "ac2-top", style: "align-items:center" },
          el("div", { class: "ac2-main" }, el("span", { class: "ac2-help" }, label + " · " + x.id + " · "), what), x.secret ? tag("비밀", "vio") : null)));
      }
      if (!list.children.length) list.append(el("div", { class: "ac2-help" }, "가리키는 기록이 없어요."));
      if (d.next_cursor != null) { cursor = d.next_cursor; more.append(sbtn("더 보기", load)); }
    };
    const box = el("div", { class: "ac2-panel" }, head, list, more);
    load();
    return box;
  }
  function memoryCard(chatId, m, pick) {
    const item = el("div", { class: "ac2-item" });
    const slot = el("div");
    let detail = false;
    const draw = () => {
      const rows = memoryDetails(m);
      item.replaceChildren(...nodes(
        memoryHead(m, pick),
        el("div", { class: "ac2-acts" },
          rows.length ? sbtn(detail ? "자세히 닫기" : "자세히", () => { detail = !detail; draw(); }) : null,
          m.turn_id ? sbtn(evidence ? "원문·근거 닫기" : "원문·근거", () => { evidence = evidence ? null : evidenceBox(chatId, m); draw(); }) : null,
          m.inherited ? null : sbtn("수정", edit),
          m.inherited ? null : sbtn(m.hold === "pin" ? "고정 풀기" : "고정", () => holdIt(m.hold === "pin" ? "" : "pin")),
          m.inherited ? null : sbtn(m.hold === "mute" ? "억제 풀기" : "억제", () => holdIt(m.hold === "mute" ? "" : "mute")),
          m.inherited ? null : sbtn("삭제", remove, "ac2-danger")),
        detail ? el("div", { class: "ac2-panel" }, ...rows.map(([label, v]) => el("div", {}, el("span", { class: "ac2-help" }, label + " · "), v))) : null,
        evidence, slot));
    };
    let evidence = null;
    // C18: one flag at a time, written like any other change
    const holdIt = async (hold) => {
      if (writing) { if (!slot.children.length) slot.replaceChildren(el("div", { class: "ac2-help" }, BUSY_TEXT)); return; }
      const note = msgBox();
      slot.replaceChildren(note.node);
      const r = await oneWrite(() => bridge("/memory/hold", { method: "POST", body: JSON.stringify({ chat_id: chatId, id: m.id, hold }) }), null, note.node);
      if (!r.ok) return note.show(errText(r), false);
      m.hold = hold;
      if (!item.isConnected) return refreshShown(); // the list was drawn again meanwhile
      draw();
      note.show(HOLD_DONE[hold], true);
    };
    const edit = () => {
      if (writing) { if (!slot.children.length) slot.replaceChildren(el("div", { class: "ac2-help" }, BUSY_TEXT)); return; }
      const text = el("textarea", { class: "ac2-area ac2-wide", rows: "3", "aria-label": "기억 내용" });
      text.value = m.body;
      const imp = el("input", { type: "number", min: "0", max: "10", step: "0.5", "aria-label": "중요도", style: "width:120px" });
      imp.value = m.imp != null ? String(m.imp) : "";
      // what the editor showed: only what the user changes from it is sent, so
      // a card drawn before another save ended cannot write old values back
      let bodyWas = text.value, impWas = imp.value, sent = false, mine = false;
      const note = msgBox();
      // more of the record (C16): the stored values by ID, read when editing
      // opens; only fields the user changed are sent, an emptied one clears
      const more = el("div", { class: "ac2-field" });
      const fields = [];
      if (m.turn_id && !m.inherited) (async () => {
        const namesRead = bridge("/names?chat_id=" + encodeURIComponent(chatId), { method: "GET" });
        const d = await bridge("/memory/detail?chat_id=" + encodeURIComponent(chatId) + "&turn_id=" + m.turn_id + "&id=" + encodeURIComponent(m.id) + "&limit=1", { method: "GET" });
        // show the stored body and importance unless the user already typed
        // a read answered after this editor already saved is older than that save
        if (d.ok && d.json.memory && !sent) {
          const raw = d.json.memory;
          if (text.value === bodyWas && typeof raw.body === "string") text.value = bodyWas = m.body = raw.body;
          if (imp.value === impWas) { m.imp = raw.imp; imp.value = impWas = raw.imp != null ? String(raw.imp) : ""; }
        }
        const ns = await namesRead;
        if (!d.ok || !ns.ok) { more.replaceChildren(el("div", { class: "ac2-help" }, "다른 칸은 읽지 못해 내용과 중요도만 고칠 수 있어요.")); return; }
        const raw = d.json.memory || {}, names = (ns.json.names || []).filter((n) => !n.inherited || raw.about === n.id || raw.owner === n.id);
        const nameField = (key, label) => {
          const shown = Object.fromEntries(names.map((n) => [n.id, n.name]));
          if (raw[key] && !shown[raw[key]]) shown[raw[key]] = (d.json.labels && d.json.labels[raw[key]]) || raw[key];
          const s = el("select", { "aria-label": label }, el("option", { value: "" }, "없음"), ...Object.entries(shown).map(([id, name]) => el("option", { value: id }, name)));
          s.value = raw[key] || "";
          fields.push([key, s, raw[key] || "", shown]);
          return el("label", { class: "ac2-help" }, label, s);
        };
        const textField = (key, label) => {
          const i = el("input", { "aria-label": label });
          i.value = raw[key] || "";
          fields.push([key, i, raw[key] || ""]);
          return el("label", { class: "ac2-help" }, label, i);
        };
        const rows = raw.kind === "fact" ? [nameField("about", "누구·무엇에 대한"), textField("mode", "종류"), textField("slot", "항목"), textField("terms", "조건"), textField("status", "상태"), textField("scope", "적용 범위")]
          : raw.kind === "view" ? [nameField("owner", "누구의 생각"), nameField("about", "누구·무엇에 대한"), textField("aspect", "생각의 갈래")]
          : raw.kind === "event" ? [textField("time", "때"), textField("setting", "상황"), textField("outcome", "결과")] : [];
        more.replaceChildren(...(rows.length ? [el("div", { class: "ac2-label" }, "다른 칸"), ...rows, el("div", { class: "ac2-help" }, "비우면 그 칸을 지워요. 비밀 표시, 근거·연결, 효과는 여기서 고치지 않아요.")] : []));
      })();
      const saveIt = async () => {
        if (writing) return note.show(mine ? "앞의 저장이 끝난 뒤 다시 눌러 주세요." : BUSY_TEXT, false);
        note.clear();
        const req = { chat_id: chatId, id: m.id };
        if (text.value !== bodyWas) req.body = text.value;
        const impSent = imp.value; // what this request sends is the next comparison, not later typing
        if (imp.value !== impWas && imp.value.trim() !== "") { // empty keeps it
          req.imp = Number(imp.value); // the range is the backend's check
          if (Number.isNaN(req.imp)) return note.show(ERR_TEXT.bad_importance, false);
        }
        const changed = fields.filter(([, input, was]) => input.value !== was).map((f) => [f, f[1].value]);
        if (changed.length) req.patch = Object.fromEntries(changed.map(([f, value]) => [f[0], value]));
        if (req.body === undefined && req.imp === undefined && !req.patch) return draw(); // nothing changed
        sent = mine = true;
        let r;
        try { r = await oneWrite(() => bridge("/memory/edit", { method: "POST", body: JSON.stringify(req) }), null, note.node); } finally { mine = false; }
        if (!r.ok) return note.show(errText(r), false);
        if (req.body !== undefined) { m.body = req.body.trim(); bodyWas = req.body; }
        if (req.imp !== undefined) { m.imp = req.imp; impWas = impSent; }
        // a name field shows its name, as the list does; the list's grouping follows on its next read
        for (const [f, sent] of changed) {
          const [key, , , shown] = f;
          m[key] = shown ? (sent ? shown[sent] || sent : "") : sent;
          f[2] = sent; // the next save in this editor compares with what was saved
        }
        if (item.isConnected) draw(); else await refreshShown(); // the list was drawn again meanwhile
      };
      slot.replaceChildren(el("div", { class: "ac2-panel ac2-editing" },
        el("label", { class: "ac2-help" }, "기억 내용"), text,
        el("div", { class: "ac2-acts" }, el("label", { class: "ac2-help" }, "중요도 (0~10, 비우면 그대로)"), imp),
        more,
        el("div", { class: "ac2-help" }, "저장하면 다음 응답부터 고친 내용이 들어가요. 효과 값과 비밀 정보, 원래 근거는 그대로예요. 이 채팅에서 갈라진 분기 채팅도 이 기억을 함께 읽어요."),
        note.node,
        el("div", { class: "ac2-acts" }, el("button", { type: "button", class: "ac2-btn ac2-primary", on: { click: saveIt } }, "저장"),
          el("button", { type: "button", class: "ac2-btn", on: { click: () => (mine ? note.show("저장이 끝난 뒤 닫을 수 있어요.", false) : slot.replaceChildren()) } }, "취소"))));
    };
    const remove = () => askHere(slot, "이 기억을 지울까요? 다음 응답부터 넣지 않아요. 효과와 비밀 정보도 함께 지워지고, 이 채팅에서 갈라진 분기 채팅에서도 사라져요. 이 턴의 마지막 기억이면 그 턴은 다시 만들어요.", "삭제", async () => {
      const r = await bridge("/memory/delete", { method: "POST", body: JSON.stringify({ chat_id: chatId, id: m.id }) });
      if (r.ok) {
        const remade = (r.json && r.json.remade) || [];
        if (!item.isConnected) await refreshShown(); // the list was drawn again meanwhile
        else if (remade.length) item.replaceChildren(el("div", { class: "ac2-msg ac2-ok" }, remade.join(", ") + "턴의 기억이 모두 지워져 그 턴을 다시 만들고 있어요."));
        else item.remove();
      }
      return r;
    });
    draw();
    return item;
  }

  // The memories tab: the chat in view, in four parts (UI design 10-27's
  // sub tabs): its memories, its stored turns, its names, and the cold start.
  let memorySub = "list";
  // This turn (UI design 10-27 "이번 턴"; user 10-28: in the settings, not on
  // the chat screen): what the backend put in the chat's last request, in
  // the order it went, and how the previous turn's memory is going. All of
  // it is Go's record (/injection), kept while the backend runs.
  function meter(label, used, max, tone) {
    const pct = max > 0 ? Math.min(100, Math.round((used / max) * 100)) : 0;
    return el("div", { class: "ac2-stat" }, label, el("b", { style: "font-size:16px" }, used + " / " + max + "자"),
      el("div", { class: "ac2-meter", role: "img", "aria-label": label + " " + pct + "%" }, el("i", { style: "width:" + pct + "%", ...(tone ? { "data-tone": tone } : {}) })));
  }
  function givenRow(m) {
    const item = el("div", { class: "ac2-item" });
    const rows = memoryDetails(m);
    let open = false;
    const draw = () => item.replaceChildren(...nodes(memoryHead(m),
      rows.length ? el("div", { class: "ac2-acts" }, sbtn(open ? "자세히 닫기" : "자세히", () => { open = !open; draw(); })) : null,
      open ? el("div", { class: "ac2-panel" }, ...rows.map(([label, v]) => el("div", {}, el("span", { class: "ac2-help" }, label + " · "), v))) : null));
    draw();
    return item;
  }
  const GEN_TAG_TONE = { acc: "acc", warn: "warn", bad: "bad" };
  async function turnTab(body) {
    const msg = msgBox();
    const chatId = (await chatInView()).id;
    if (!chatId) { body.append(msg.node); return msg.show(ERR_TEXT.chat_required, false); }
    const refresh = sbtn("새로고침", async () => { body.replaceChildren(); await turnTab(body); });
    const r = await bridge("/injection?chat_id=" + encodeURIComponent(chatId), { method: "GET" });
    if (!r.ok) { body.append(msg.node, el("div", { class: "ac2-row" }, refresh)); return msg.show(errText(r), false); }
    const v = r.json, g = v.generation;
    let genBox = null;
    if (g) {
      const [text, tone] = HUD_GEN[g.state] || [HUD_STATE[g.state] || g.state, "gray"];
      const bits = [g.state === "done" && g.count != null ? g.count + "개 만듦" : "", g.elapsed_ms != null ? (g.elapsed_ms / 1000).toFixed(1) + "초" : "", g.detail ? WARNING_TEXT[g.detail] || g.detail : ""].filter(Boolean);
      genBox = el("div", { class: "ac2-box" }, el("div", { class: "ac2-top", style: "align-items:center" },
        el("div", { class: "ac2-main" }, el("div", { class: "ac2-label" }, "직전 " + g.turn + "턴 기억 만들기"), bits.length ? el("div", { class: "ac2-meta" }, bits.join(" · ")) : null),
        tag(text.replace(/^직전 턴 /, ""), GEN_TAG_TONE[tone])),
        g.next_at ? el("div", { class: "ac2-help" }, "다음 시도 " + clock(g.next_at) + " · 대화는 그대로 이어져요.") : null);
    }
    if (v.none) {
      body.append(...nodes(el("div", { class: "ac2-box" }, el("div", { class: "ac2-label" }, "넣은 기억"),
        el("div", { class: "ac2-help" }, "백엔드가 켜진 뒤 이 채팅에서 아직 기억을 넣지 않았어요. 다음 응답을 받으면 여기에 보여요.")), genBox, el("div", { class: "ac2-row" }, refresh)));
      return;
    }
    const mems = v.memories || [];
    body.append(...nodes(
      el("div", { class: "ac2-box" },
        el("div", { class: "ac2-top", style: "align-items:center" }, el("div", { class: "ac2-main ac2-label" }, "마지막 요청에 넣은 기억"), el("span", { class: "ac2-help" }, [clock(v.at), v.total_ms + "ms"].filter(Boolean).join(" · "))),
        el("div", { class: "ac2-stats" }, el("div", { class: "ac2-stat" }, "넣은 기억", el("b", {}, v.items + "개")),
          meter("기억 예산", v.chars, v.chars_max), meter("비밀 예산", v.secret_chars, v.secret_max, "vio")),
        line("직전 턴 질의", RECENT_QUERY_TEXT[v.recent_query_used] || "미관측"),
        v.warning ? el("div", { class: "ac2-warn" }, "고른 방식 · " + (WARNING_TEXT[v.warning] || v.warning)) : null,
        v.read_error ? el("div", { class: "ac2-help" }, "기억 내용을 읽지 못했어요. 넣은 원문은 아래에서 볼 수 있어요.") : null),
      genBox,
      v.narrative ? narrBox(v.narrative) : null,
      el("div", { class: "ac2-help" }, mems.length ? "넣은 순서대로예요." : "이번 요청에 맞는 기억이 없어 넣지 않았어요."),
      mems.length ? el("div", { class: "ac2-list" }, ...mems.map(givenRow)) : null,
      v.block ? el("details", { class: "ac2-ver" }, el("summary", {}, "넣은 원문 그대로 보기"), el("div", { class: "ac2-pre ac2-panel" }, v.block)) : null,
      v.delivery_id ? pickedBox(chatId, v.delivery_id) : null,
      el("div", { class: "ac2-row" }, refresh), msg.node));
  }
  // the narrative guide sent and the input as written and as sent (E03)
  function narrBox(n) {
    const [, title, detail, tone] = narrLine(n);
    return el("div", { class: "ac2-box" }, el("div", { class: "ac2-top", style: "align-items:center" }, el("div", { class: "ac2-main ac2-label" }, title), tone ? tag("확인 필요", "warn") : null),
      el("div", { class: "ac2-help" }, detail),
      n.guide_error || n.input_error ? el("div", { class: "ac2-help" }, [n.guide_error, n.input_error].filter(Boolean).join(" · ")) : null,
      n.guide_text ? el("details", { class: "ac2-ver" }, el("summary", {}, "넣은 서사 가이드 보기"), el("div", { class: "ac2-pre ac2-panel" }, n.guide_text)) : null,
      n.input_after ? el("details", { class: "ac2-ver" }, el("summary", {}, "입력 원문과 다듬은 글"),
        el("div", { class: "ac2-panel" }, el("div", { class: "ac2-label" }, "쓴 글"), el("div", { class: "ac2-pre" }, n.input_before || ""), el("div", { class: "ac2-label" }, "보낸 글"), el("div", { class: "ac2-pre" }, n.input_after))) : null);
  }
  // How Go chose (C21): every candidate it weighed for that request, a page
  // at a time from the stored record; read only when opened.
  // over_ceiling is decided when the budget is first filled; scene contrast
  // removes afterwards and never refills, so it is not "budget left over".
  const PICK_TEXT = { taken: ["넣음", "acc"], over_ceiling: ["순위에서 밀림", ""], parent_not_taken: ["근거 사건이 안 들어가 빠짐", ""], contrast_removed: ["장면 대비 단계에서 빠짐", ""], not_chosen: ["전처리에서 고르지 않음", ""] };
  function pickedBox(chatId, id) {
    const list = el("div", { class: "ac2-list" });
    const note = el("div", { class: "ac2-help" });
    const more = el("div", { class: "ac2-row" });
    let cursor = 0, shown = 0, started = false, pushed = false, removed = false;
    const load = async () => {
      more.replaceChildren();
      const r = await bridge("/injection/trace?chat_id=" + encodeURIComponent(chatId) + "&delivery_id=" + id + "&row_cursor=" + cursor + "&row_limit=50", { method: "GET" });
      // a failed read keeps what was shown and can be asked again from the same place
      if (!r.ok) { note.textContent = r.code === "store_failed" ? "고른 과정 기록을 읽지 못했어요." : errText(r); more.append(sbtn("다시 시도", load)); return; }
      const d = r.json.delivery;
      for (const t of d.trace || []) {
        // C18 a pinned row is not ranked: it goes first into its budget
        const pin = t.hold === "pin";
        const [text, tone] = pin && t.result === "over_ceiling" ? ["예산에 들어가지 못함", ""] : PICK_TEXT[t.result] || [t.result, ""];
        const bits = [t.id, KIND_TEXT[t.kind] || t.kind, t.turn ? t.turn + "턴" : "", pin ? "고정 · 순위 없이 예산에 먼저" : "등수 합 " + t.rank, t.size ? t.size + "자" : "",
          t.unrelated != null ? "판정 관계없음 " + Math.round(t.unrelated * 100) + "%" : "", t.ai_unrelated != null ? "AI " + (t.ai_unrelated ? "관계없음" : "관계있음") : ""].filter(Boolean);
        list.append(el("div", { class: "ac2-item" }, el("div", { class: "ac2-top", style: "align-items:center" },
          el("div", { class: "ac2-main ac2-meta" }, bits.join(" · ")), pin ? tag("고정", "acc") : null, t.secret ? tag("비밀", "vio") : null, tag(text, tone))));
        shown++;
        if (t.result === "over_ceiling") pushed = true;
        if (t.result === "contrast_removed") removed = true;
      }
      note.textContent = "후보 " + d.row_total + "개 중 " + shown + "개 · 위에서부터 Go가 본 순서예요. 등수 합은 작을수록 앞이에요." +
        (pushed ? " '순위에서 밀림'은 처음 예산을 채울 때 자리가 없던 기억이에요." : "") +
        (removed ? " 장면 대비로 뺀 자리는 다른 기억으로 다시 채우지 않아요." : "");
      if (d.next_cursor != null) { cursor = d.next_cursor; more.append(sbtn("더 보기", load)); }
    };
    const box = el("details", { class: "ac2-ver" }, el("summary", {}, "고른 과정 보기"), el("div", { class: "ac2-panel" }, note, list, more));
    box.addEventListener("toggle", () => { if (box.open && !started) { started = true; load(); } });
    return box;
  }

  async function memoriesTab(body) {
    const msg = msgBox();
    const open = await chatInView();
    const chatId = open.id;
    if (!chatId) { body.append(msg.node); return msg.show(ERR_TEXT.chat_required, false); }
    const part = el("div", { class: "ac2-field", style: "gap:14px" });
    const subs = [["list", "기억"], ["turns", "대화 원문"], ["names", "이름"], ["body", "신체"], ["cold", "지난 대화로 만들기"]];
    const nav = el("nav", { class: "ac2-sub", "aria-label": "기억 소분류" });
    const go = async (k) => {
      memorySub = k;
      nav.replaceChildren(...subs.map(([key, label]) => el("button", { type: "button", class: "ac2-subtab", "aria-pressed": String(key === k), on: { click: () => go(key) } }, label)));
      part.replaceChildren();
      if (k === "list") await memoryList(part, chatId);
      else if (k === "turns") part.append(memoryTurnsBox(chatId));
      else if (k === "names") part.append(memoryNamesBox(chatId));
      else if (k === "body") part.append(memoryBodyBox(chatId));
      else part.append(coldStart(chatId, open.name), hypaImport(chatId, open.name));
    };
    body.append(nav, part, msg.node);
    await go(memorySub);
  }

  // The list by kind (user 10-28: items apart, like AC1): all of them by
  // turn, or one kind; Go groups a kind (facts by mode, subjective memories
  // by whose they are) and the list heads each group.
  // C14 이름별: Go lists a name's current facts by mode, then their
  // subjective memories, and what is about nobody last; this only heads them.
  const KIND_TABS = [["", "전체"], ["event", "사건"], ["fact", "사실"], ["view", "주관 기억"], ["person", "이름별"]];
  let memoryKind = "";
  // 이름별 one name at a time (user 11-09): Go lists the names (people,
  // things, places, groups) and the chosen name's parts; each part is a
  // folded box, its changed values the first one. A tab is chosen by the
  // name's stored reference (two names may read the same; "" is nobody)
  let memoryPerson = null;
  const NOBODY = "특정 이름 없음(세계 규칙 등)";
  const memoryGroup = (kind, m) => kind === "fact" ? m.mode || "그 밖의 사실"
    : kind === "view" ? (m.owner ? m.owner + "의 기억" : "누구의 기억인지 모름")
    : m.turn + "턴";
  async function memoryList(part, chatId) {
    const msg = msgBox();
    const kindBar = el("div", { class: "ac2-kinds", role: "group", "aria-label": "기억 종류" });
    const drawKinds = (kinds) => {
      const n = (k) => k ? kinds[k] || 0 : (kinds.event || 0) + (kinds.fact || 0) + (kinds.view || 0);
      kindBar.replaceChildren(...KIND_TABS.map(([k, label]) => el("button", { type: "button", class: "ac2-kind", "aria-pressed": String(k === memoryKind),
        on: { click: () => { memoryKind = k; return load(true); } } }, label, el("b", {}, String(n(k))))));
    };
    let lastGroup = null;
    const personBar = el("div", { class: "ac2-kinds", role: "group", "aria-label": "이름" });
    const drawPeople = (people) => {
      personBar.hidden = memoryKind !== "person" || !people.length;
      personBar.replaceChildren(...people.map((p) => el("button", { type: "button", class: "ac2-kind", "aria-pressed": String(p.ref === memoryPerson),
        ...(p.changes ? { title: "바뀐 값 " + p.changes + "개" } : {}),
        on: { click: () => { memoryPerson = p.ref; return load(true); } } },
        p.name || NOBODY, NAME_TYPE[p.type] ? el("span", { class: "ac2-help" }, " " + NAME_TYPE[p.type]) : null, el("b", {}, String(p.count)))));
    };
    // a name's changed values, oldest first: before → value of each event
    // that changed it, and each older value a later record replaced
    const changeRow = (c) => el("div", { class: "ac2-item ac2-change" },
      el("div", { class: "ac2-help" }, c.turn + "턴" + (c.date ? " · " + c.date : "") + (c.slot ? " · " + c.slot : "")),
      c.replaced ? el("div", {}, c.value, el("span", { class: "ac2-help" }, " (나중 기록으로 바뀐 옛 값)"))
        : el("div", {}, (c.before ? c.before + " → " : "") + (c.value || c.status || "") + (c.delta ? " (" + c.delta + ")" : "")),
      c.event ? el("div", { class: "ac2-help" }, c.event) : null);
    const find = el("input", { type: "search", placeholder: "본문·이름으로 찾기", "aria-label": "기억 찾기" });
    const list = el("div", { class: "ac2-list" });
    const more = el("button", { type: "button", class: "ac2-btn" }, "더 불러오기");
    const count = el("span", { class: "ac2-help" });
    // choosing several: a box on each row, then one confirmation for all
    const selected = new Set();
    let choosing = false;
    const bar = el("div");
    const slot = el("div");
    const drawBar = () => {
      const remove = choosing ? sbtn("고른 기억 " + selected.size + "개 지우기", removeSome, "ac2-danger") : null;
      if (remove) remove.disabled = selected.size === 0;
      bar.replaceChildren(el("div", { class: "ac2-acts" },
        sbtn(choosing ? "고르기 끝" : "여러 개 고르기", () => { choosing = !choosing; load(true); }), remove));
    };
    const removeSome = () => {
      const ids = [...selected];
      askHere(slot, "고른 기억 " + ids.length + "개를 지울까요? 되돌릴 수 없어요. 이 채팅에서 갈라진 분기 채팅에서도 사라져요. 한 턴의 기억을 모두 지우면 그 턴은 다시 만들어요(평론가를 한 번 불러 비용이 들어요).", "삭제", async () => {
        const r = await bridge("/memory/delete", { method: "POST", body: JSON.stringify({ chat_id: chatId, ids }) });
        if (r.ok) {
          await refreshShown();
          const remade = (r.json && r.json.remade) || [];
          if (remade.length) msg.show(remade.join(", ") + "턴의 기억이 모두 지워져 그 턴을 다시 만들고 있어요. 이번 턴 탭에서 진행을 볼 수 있어요.", true);
        }
        return r;
      });
    };
    // choosing a whole group (a turn in 전체, a mode or an owner in a kind)
    // with one box on its head; rows of it a later page brings come checked
    const groups = new Map();
    const choose = (pick, id, on) => {
      if (pick.disabled) return;
      pick.checked = on;
      if (on) selected.add(id); else selected.delete(id);
    };
    const groupHead = (g, n) => {
      const entry = { box: null, rows: [], body: null };
      groups.set(g, entry);
      if (n !== undefined) { // a person's part: folded, opened by its head
        const label = g + " " + n + "개";
        let head = label;
        if (choosing) {
          entry.box = el("input", { type: "checkbox", class: "ac2-check", style: "margin:0", "aria-label": g + " 기억 모두 고르기" });
          entry.box.addEventListener("change", () => { for (const [pick, id] of entry.rows) choose(pick, id, entry.box.checked); drawBar(); });
          entry.box.addEventListener("click", (e) => e.stopPropagation());
          head = el("span", { style: "display:inline-flex;gap:8px;align-items:center" }, entry.box, label);
        }
        entry.body = el("div");
        return el("details", { class: "ac2-fold" }, el("summary", { class: "ac2-group", style: "cursor:pointer;min-height:36px;display:flex;align-items:center" }, head), entry.body);
      }
      if (!choosing) return el("div", { class: "ac2-group" }, g);
      entry.box = el("input", { type: "checkbox", class: "ac2-check", style: "margin:0", "aria-label": g + " 기억 모두 고르기" });
      entry.box.addEventListener("change", () => { for (const [pick, id] of entry.rows) choose(pick, id, entry.box.checked); drawBar(); });
      return el("label", { class: "ac2-group", style: "display:flex;gap:8px;align-items:center;cursor:pointer" }, entry.box, g + " 모두 고르기");
    };
    let offset = 0;
    let reads = 0; // only the latest read draws: an older answer cannot add rows after it (review 07)
    const load = async (reset) => {
      const mine = ++reads;
      msg.clear();
      if (reset) { offset = 0; list.replaceChildren(); selected.clear(); if (!(activeBox && slot.children.includes(activeBox))) slot.replaceChildren(); lastGroup = null; groups.clear(); }
      drawBar();
      const q = encodeURIComponent(find.value.trim());
      const kind = memoryKind;
      const who = kind === "person" && memoryPerson !== null ? "&person=" + encodeURIComponent(memoryPerson) : "";
      const res = await bridge("/memory/list?chat_id=" + encodeURIComponent(chatId) + "&q=" + q + (kind ? "&kind=" + kind : "") + who + "&offset=" + offset + "&limit=" + MEM_PAGE, { method: "GET" });
      if (mine !== reads) return;
      if (!res.ok) return msg.show(errText(res), false);
      const v = res.json;
      drawKinds(v.kinds || {});
      const people = v.people || [];
      // the first person until one is chosen, or when the chosen one is gone
      if (kind === "person" && people.length && !people.some((p) => p.ref === memoryPerson)) { memoryPerson = people[0].ref; return load(true); }
      drawPeople(people);
      const partCount = new Map((v.parts || []).map((p) => [p.name, p.count]));
      const changes = offset === 0 && kind === "person" ? v.changes || [] : [];
      if (changes.length) {
        list.append(el("details", { class: "ac2-fold" }, el("summary", { class: "ac2-group", style: "cursor:pointer;min-height:36px;display:flex;align-items:center" }, "바뀐 값 " + changes.length + "개"),
          el("div", {}, ...changes.map(changeRow))));
      }
      for (const m of v.memories) {
        const g = kind === "person" ? (m.kind === "view" ? "주관 기억" : m.mode || "그 밖의 사실") : memoryGroup(kind, m);
        if (g !== lastGroup) { if (!groups.has(g)) list.append(groupHead(g, kind === "person" ? partCount.get(g) || 0 : undefined)); lastGroup = g; }
        let pick = null;
        if (choosing) {
          const entry = groups.get(g);
          pick = el("input", { type: "checkbox", class: "ac2-check", "aria-label": m.turn + "턴 기억 고르기" });
          pick.disabled = !!m.inherited;
          pick.addEventListener("change", () => {
            choose(pick, m.id, pick.checked);
            if (entry.box) entry.box.checked = entry.rows.every(([p]) => p.disabled || p.checked);
            drawBar();
          });
          entry.rows.push([pick, m.id]);
          if (entry.box && entry.box.checked) choose(pick, m.id, true);
        }
        (groups.get(g).body || list).append(memoryCard(chatId, m, pick));
      }
      offset += v.memories.length;
      if (choosing) drawBar(); // rows a checked group head brought in count too
      count.textContent = "기억 " + v.total + "개" + (find.value.trim() ? " 찾음" : "") + (kind === "person" && v.replaced ? " · 새 값으로 바뀐 옛 기억 " + v.replaced + "개는 각 이름의 \"바뀐 값\"에서 볼 수 있어요" : "");
      more.hidden = offset >= v.total;
      if (!v.total && !changes.length) list.append(el("div", { class: "ac2-item" }, el("div", { class: "ac2-help" }, find.value.trim() ? "찾는 기억이 없어요." : kind === "person" ? "이름에 대한 사실·주관 기억이 아직 없어요." : kind ? KIND_TEXT[kind] + " 기억이 아직 없어요." : "이 채팅에는 아직 기억이 없어요.")));
    };
    let timer = null;
    find.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(() => load(true), 300); });
    more.addEventListener("click", () => load(false));
    part.append(el("div", { class: "ac2-top" }, el("div", { class: "ac2-main" }, el("h1", { class: "ac2-h1" }, "기억"), count), find),
      kindBar, personBar, bar, slot, list, more, msg.node);
    liveView(list, () => load(true)); // its own list: part is shared by the sub tabs
    await load(true);
  }

  // Stored turns, read only (user decision 10-28: AC1's turn delete becomes
  // clearing or remaking the turn's memories).
  const TURN_STATE = { made: ["만듦", "acc"], waiting: ["대기", "warn"], failed: ["실패", "bad"], none: ["없음", ""] };
  function memoryTurnsBox(chatId) {
    const box = el("div", { class: "ac2-field", style: "gap:14px" });
    const list = el("div", { class: "ac2-list" });
    const more = el("button", { type: "button", class: "ac2-btn" }, "더 불러오기");
    const note = msgBox();
    let offset = 0, reads = 0; // only the latest read draws (review 07)
    const load = async (reset) => {
      const mine = ++reads;
      more.disabled = true;
      note.clear();
      try {
        if (reset) { offset = 0; list.replaceChildren(); }
        const r = await bridge("/chats/turns?chat_id=" + encodeURIComponent(chatId) + "&offset=" + offset + "&limit=" + MEM_PAGE, { method: "GET" });
        if (mine !== reads) return;
        if (!r.ok) return note.show(errText(r), false);
        for (const t of r.json.turns) {
          const slot = el("div");
          const [stateText, tone] = TURN_STATE[t.state] || [t.state, ""];
          const user = el("div", { class: "ac2-text ac2-clamp" }, t.user);
          const reply = el("div", { class: "ac2-text ac2-clamp" }, t.reply);
          // a turn whose memories are deleted is made again (user 10-29), so
          // the turn has one action: make it again
          const ask = () => askHere(slot, t.turn + "턴의 기억을 다시 만들까요? 평론가를 한 번 불러 비용이 들어요. 지금 기억은 새 결과로 바뀌어요.",
            "다시 만들기", async () => {
              const res = await bridge("/memory/turn/remake", { method: "POST", body: JSON.stringify({ chat_id: chatId, turn: t.turn }) });
              if (res.ok) await refreshShown();
              return res;
            }, true);
          let wide = false;
          const widen = sbtn("펼치기", () => {
            wide = !wide;
            for (const x of [user, reply]) x.className = "ac2-text" + (wide ? "" : " ac2-clamp");
            widen.textContent = wide ? "접기" : "펼치기";
          });
          const acts = el("div", { class: "ac2-acts" }, widen);
          if (!t.inherited) acts.append(sbtn("이 턴 다시 만들기", ask));
          list.append(el("div", { class: "ac2-item" },
            el("div", { class: "ac2-top" }, el("span", { class: "ac2-num ac2-label" }, t.turn + "턴"),
              el("div", { class: "ac2-main ac2-meta" }, "기억 " + t.memories + "개" + (t.inherited ? " · 부모 채팅에서 읽음" : "")),
              el("div", { class: "ac2-tags" }, tag(stateText, tone))),
            el("div", { class: "ac2-help" }, "사용자"), user, el("div", { class: "ac2-help" }, "응답"), reply, acts, slot));
        }
        offset += r.json.turns.length;
        more.hidden = offset >= r.json.total;
        if (!r.json.total) list.append(el("div", { class: "ac2-item" }, el("div", { class: "ac2-help" }, "저장된 대화 원문이 없어요.")));
      } finally { if (mine === reads) more.disabled = false; }
    };
    more.addEventListener("click", () => load(false));
    // C30: every turn whose memory could not be made, as the backend finds
    // them (retries used up included), made again after a preview
    const failSlot = el("div");
    const remakeFailed = async () => {
      if (busyHere(failSlot)) return;
      note.clear();
      const p = await bridge("/memory/turn/remake-failed", { method: "POST", body: JSON.stringify({ chat_id: chatId, dry_run: true }) });
      if (!p.ok) return note.show(errText(p), false);
      const turns = p.json.turns || [];
      if (!turns.length) return note.show("기억을 만들지 못한 턴이 없어요.", true);
      askHere(failSlot, "기억을 만들지 못한 " + turns.length + "개 턴(" + turns.slice(0, 10).join(", ") + (turns.length > 10 ? " …" : "") + "턴)을 다시 만들까요? 턴마다 평론가를 한 번씩 불러 비용이 들어요.", "다시 만들기", async () => {
        const r = await bridge("/memory/turn/remake-failed", { method: "POST", body: JSON.stringify({ chat_id: chatId }) });
        if (!r.ok) return r;
        await refreshShown();
        note.show((r.json.turns || []).length + "개 턴을 다시 만들기 시작했어요.", true);
        return { ok: true, keep: false };
      }, true);
    };
    box.append(el("div", { class: "ac2-top" }, el("div", { class: "ac2-main" }, el("h1", { class: "ac2-h1" }, "대화 원문"),
      el("span", { class: "ac2-help" }, "백엔드에 저장된 원문이에요. 읽기만 하고, 턴마다 기억을 지우거나 다시 만들 수 있어요.")), sbtn("실패한 턴 다시 만들기", remakeFailed), sbtn("새로고침", () => load(true))),
      note.node, failSlot, list, more);
    load(true);
    liveView(box, () => load(true));
    return box;
  }

  // Body tracking (1.0 E09/E10, AC1's 주기·임신 panel): the two switches
  // for every chat, each tracked person's state as delivered, the facts read
  // from turns or recorded here, and the person's model settings. The
  // backend decides and calculates; this screen shows and sends.
  const BODY_KIND = { period_start: "월경 시작", pregnancy_confirmed: "임신 확인", pregnancy_ended: "임신 끝남", recovery_started: "회복 시작", recovery_ended: "회복 끝남", conception_exposure: "임신 가능한 관계" };
  const BODY_SCOPE = { flashback: "회상", planned: "계획", hypothetical: "가정" };
  const BODY_SPEC = [["cycle_days", "주기(일)", "1"], ["variation_days", "주기 ± (일)", "1"], ["period_days", "월경 기간(일)", "1"], ["luteal_min_days", "황체기 최소(일)", "1"],
    ["luteal_max_days", "황체기 최대(일)", "1"], ["viability", "주기 착상 가능성(0~1)", "0.05"], ["peak", "최고 수정 가능성(0~1)", "0.05"], ["gestation_days", "임신 기간(일)", "1"]];
  // settings typed on the body screen and not saved yet, kept across redraws
  // (review 12 R09): chat|person|field -> value
  const bodyDrafts = new Map();
  function memoryBodyBox(chatId) {
    const box = el("div", { class: "ac2-field", style: "gap:14px" });
    const note = msgBox();
    const list = el("div", { class: "ac2-list" });
    const switches = el("div", { class: "ac2-field" });
    let reads = 0;
    const write = async (slot, run, done) => {
      if (busyHere(slot)) return;
      note.clear();
      const r = await oneWrite(run, box, box);
      if (!r.ok) return note.show(errText(r), false);
      await refreshShown(); // the screen shown now, also one drawn again meanwhile (review 12 R13)
      note.show(done, true);
    };
    const load = async () => {
      const mine = ++reads;
      const r = await bridge("/body?chat_id=" + encodeURIComponent(chatId), { method: "GET" });
      if (mine !== reads) return;
      if (!r.ok) return note.show(errText(r), false);
      const v = r.json;
      const save = (k) => async (on) => {
        // one write at a time, as every other setting (review 12 R03)
        if (writing) { note.show(BUSY_TEXT, false); return false; }
        const next = { cycle_tracking_enabled: v.cycle_tracking_enabled, automatic_pregnancy_enabled: v.automatic_pregnancy_enabled, [k]: on };
        const res = await oneWrite(() => bridge("/body/settings", { method: "POST", body: JSON.stringify(next) }), box, box);
        if (!res.ok) { note.show(errText(res), false); return false; }
        v[k] = on;
        await refreshShown();
        note.show("저장했어요. 모든 채팅에 적용돼요.", true);
      };
      switches.replaceChildren(toggle("ac2-body-cycle", "주기 추적 사용", v.cycle_tracking_enabled, save("cycle_tracking_enabled")),
        toggle("ac2-body-preg", "자동 임신 사용", v.automatic_pregnancy_enabled, save("automatic_pregnancy_enabled")),
        el("div", { class: "ac2-help" }, "켜면 대화에 나온 월경·임신 같은 일을 기억과 함께 기록하고, 계산한 몸 상태를 기억 블록 끝에 따로 넣어요(신체 몫 " + v.budget + "자, 두 가지를 다 켜도 한 번). 계산 값은 인물들이 아는 사실로 쓰이지 않아요."));
      list.replaceChildren();
      if (!v.people.length) list.append(el("div", { class: "ac2-item" }, el("div", { class: "ac2-help" },
        v.cycle_tracking_enabled || v.automatic_pregnancy_enabled ? "아직 신체 일이 기록된 인물이 없어요. 대화에 나오면 그 인물부터 추적해요." : "꺼져 있어요.")));
      for (const p of v.people) {
        const slot = el("div");
        const facts = el("div", { class: "ac2-field" }, ...p.facts.map((f) => {
          const row = el("div", { class: "ac2-top", style: "align-items:center" },
            el("div", { class: "ac2-main ac2-meta" }, [f.day || "날짜 모름", BODY_KIND[f.kind] || f.kind, BODY_SCOPE[f.scope], f.user ? "직접 기록" : "", (f.partners || []).length ? "상대 " + f.partners.join(", ") : "",
              f.result ? (f.result.implanted ? "모형: 착상" : "모형: 착상 없음") : ""].filter(Boolean).join(" · ")));
          if (f.user) row.append(sbtn("지우기", () => askHere(slot, (f.day || "") + " " + (BODY_KIND[f.kind] || f.kind) + " 기록을 지울까요?", "지우기",
            async () => {
              const res = await bridge("/body/record/delete", { method: "POST", body: JSON.stringify({ chat_id: chatId, id: f.id }) });
              if (res.ok) { await refreshShown(); note.show("기록을 지웠어요.", true); }
              return res;
            }), "ac2-danger"));
          return row;
        }));
        const draftKey = (k) => chatId + "|" + p.who + "|" + k;
        const inputs = BODY_SPEC.map(([k, label, step]) => {
          const input = el("input", { id: "ac2-body-" + p.who + "-" + k, type: "number", step, value: bodyDrafts.has(draftKey(k)) ? bodyDrafts.get(draftKey(k)) : p.spec[k], "aria-label": label });
          input.addEventListener("input", () => bodyDrafts.set(draftKey(k), input.value));
          return input;
        });
        const conceive = el("input", { type: "checkbox", "aria-label": "임신 가능" }); conceive.checked = !!p.spec.can_conceive;
        const cycleOn = el("input", { type: "checkbox", "aria-label": "주기 사용" }); cycleOn.checked = !!p.spec.cycle_enabled;
        const saveSpec = () => {
          const spec = { can_conceive: conceive.checked, cycle_enabled: cycleOn.checked };
          const sent = BODY_SPEC.map((_, i) => inputs[i].value);
          BODY_SPEC.forEach(([k], i) => { spec[k] = Number(sent[i]); });
          write(slot, async () => {
            const r = await bridge("/body/person", { method: "POST", body: JSON.stringify({ chat_id: chatId, who: p.who, spec }) });
            // a value typed after these were sent stays as typed
            if (r.ok) BODY_SPEC.forEach(([k], i) => { if (bodyDrafts.get(draftKey(k)) === sent[i]) bodyDrafts.delete(draftKey(k)); });
            return r;
          }, "설정을 저장했어요.");
        };
        const kind = el("select", { "aria-label": "기록할 일" }, ...["period_start", "pregnancy_confirmed", "pregnancy_ended", "recovery_started", "recovery_ended"].map((k) => el("option", { value: k }, BODY_KIND[k])));
        const day = el("input", { type: "text", placeholder: "2026/01/02 또는 Day 12", "aria-label": "작중 날" });
        const record = () => write(slot, () => bridge("/body/record", { method: "POST", body: JSON.stringify({ chat_id: chatId, who: p.who, kind: kind.value, day: day.value.trim() }) }), "기록했어요.");
        list.append(el("div", { class: "ac2-item" },
          el("div", { class: "ac2-title2" }, p.name),
          ...(p.state.lines.length ? p.state.lines.map((l) => el("div", { class: "ac2-meta" }, l.replace(/^- body [^\[]*/, ""))) : [el("div", { class: "ac2-help" }, "계산할 수 있는 상태가 아직 없어요(작중 날짜를 모르거나 꺼져 있음).")]),
          el("details", { class: "ac2-ver" }, el("summary", {}, "기록된 일 " + p.facts.length + "개"), facts),
          el("details", { class: "ac2-ver" }, el("summary", {}, "사실 직접 기록"), el("div", { class: "ac2-acts" }, kind, day, sbtn("기록", record))),
          el("details", { class: "ac2-ver" }, el("summary", {}, "설정"), el("div", { class: "ac2-grid" }, ...BODY_SPEC.map(([, label], i) => el("label", { class: "ac2-field" }, el("span", { class: "ac2-help" }, label), inputs[i]))),
            el("div", { class: "ac2-acts" }, el("label", {}, conceive, " 임신 가능"), el("label", {}, cycleOn, " 주기 사용"), sbtn("설정 저장", saveSpec))),
          slot));
      }
    };
    box.append(el("div", { class: "ac2-top" }, el("div", { class: "ac2-main" }, el("h1", { class: "ac2-h1" }, "신체"),
      el("span", { class: "ac2-help" }, "주기·임신 추적. 계산 값은 작중 모형이고, 인물이 아는 사실이 아니에요.")), sbtn("새로고침", load)), switches, note.node, list);
    load();
    liveView(box, load);
    return box;
  }

  // The merge preview as lines: counts Go read (a "?" where a read failed) and its warnings.
  const MERGE_WARN = { same_name_other_targets: "같은 이름을 쓰는 다른 대상이 있어요. 정말 같은 것인지 확인해 주세요.", same_name_distinct_ids: "같은 이름이 서로 다른 기록으로 저장돼 있어요.",
    different_types: "두 이름의 종류가 달라요(예: 인물과 물건).", protected_name_change: "비밀 표시(숨김·모름)에 쓰인 이름이 바뀌어요." };
  function mergePreviewLines(v) {
    const c = v.counts || {}, n = (k) => c[k] ? (c[k].status === "available" && c[k].count != null ? c[k].count : "?") : 0;
    const out = [el("div", {}, "합치면 바뀌는 기억 " + n("memories") + "개(사건 " + n("memories_event") + " · 사실 " + n("memories_fact") + " · 주관 " + n("memories_view") + "), 바뀐 값 " + n("effects") + ", 연결 " + n("links") + ", 누가 아는가 " + n("awareness") + ", 다른 이름 " + n("user_aliases") + "개")];
    if (n("other_sessions")) out.push(el("div", {}, "이 이름을 물려받은 다른 채팅 " + n("other_sessions") + "개의 기억도 함께 바뀌어요."));
    out.push(el("div", {}, "다시 만들 임베딩 문서 " + n("flat_documents") + "개 · 문맥 문서 " + n("contextual_documents") + "개"));
    for (const w of v.warnings || []) out.push(el("div", { class: "ac2-warn" }, MERGE_WARN[w] || w));
    if (Object.values(c).some((x) => x && x.status !== "available")) out.push(el("div", {}, "일부 수는 읽지 못해 ?로 보여요."));
    return out;
  }
  // Names: renamed, or merged into another (the merged one stays an alias).
  const NAME_TYPE = { person: "인물", place: "장소", thing: "물건", group: "무리" };
  function memoryNamesBox(chatId) {
    const box = el("div", { class: "ac2-field", style: "gap:14px" });
    const list = el("div", { class: "ac2-list" });
    const note = msgBox();
    let reads = 0;
    const load = async () => {
      const mine = ++reads;
      note.clear();
      const r = await bridge("/names?chat_id=" + encodeURIComponent(chatId), { method: "GET" });
      if (mine !== reads) return;
      if (!r.ok) return note.show(r.code === "store_failed" ? "이름 목록을 읽지 못했어요. 새로고침해 주세요." : errText(r), false);
      list.replaceChildren();
      const names = r.json.names;
      for (const n of names) {
        const slot = el("div");
        const rename = () => {
          const name = el("input", { value: n.name, "aria-label": n.name + " 새 이름" });
          if (!askHere(slot, "", "이름 저장", async () => {
            const res = await bridge("/names/edit", { method: "POST", body: JSON.stringify({ chat_id: chatId, id: n.id, name: name.value }) });
            if (res.ok) await refreshShown();
            return res;
          }, true)) return;
          slot.querySelector(".ac2-say").replaceChildren(name);
        };
        const merge = () => {
          const target = el("select", { "aria-label": n.name + "을 합칠 이름" }, el("option", { value: "" }, "남길 이름 고르기"));
          for (const other of names) if (other.id !== n.id && !other.inherited) target.append(el("option", { value: other.id }, other.name));
          // what the merge would change, read from Go before it is confirmed (C17)
          const preview = el("div", { class: "ac2-help" });
          let asked = 0;
          target.addEventListener("change", async () => {
            const mine = ++asked;
            preview.replaceChildren();
            if (!target.value) return;
            preview.textContent = "합치면 무엇이 바뀌는지 확인하는 중…";
            const p = await bridge("/names/merge/preview", { method: "POST", body: JSON.stringify({ chat_id: chatId, keep: target.value, merge: n.id }) });
            if (mine !== asked) return;
            preview.textContent = p.ok ? "" : "바뀔 내용을 미리 읽지 못했어요. 합치기는 그대로 할 수 있어요.";
            if (p.ok) preview.append(...mergePreviewLines(p.json));
          });
          if (!askHere(slot, "", "합치기", async () => {
            if (!target.value) return { ok: false, code: "pick_name" };
            const res = await bridge("/names/merge", { method: "POST", body: JSON.stringify({ chat_id: chatId, keep: target.value, merge: n.id }), timeoutMs: 0 });
            if (res.ok) await refreshShown();
            return res;
          })) return;
          slot.querySelector(".ac2-say").replaceChildren(el("span", {}, n.name + "을(를) 같은 인물·물건으로 합쳐요. 참조는 남길 이름으로 옮기고, 이 이름은 다른 이름(별명)으로 남아요. "), target, preview);
        };
        list.append(el("div", { class: "ac2-item" },
          el("div", { class: "ac2-top" }, el("div", { class: "ac2-main" }, el("div", { class: "ac2-title2" }, n.name),
            el("div", { class: "ac2-meta" }, "기억 " + n.memories + "개 · 다른 이름 " + (n.aliases.join(", ") || "없음") + (n.inherited ? " · 부모 채팅에서 읽음" : ""))),
            el("div", { class: "ac2-tags" }, tag(NAME_TYPE[n.type] || n.type || "이름"))),
          n.inherited ? null : el("div", { class: "ac2-acts" }, sbtn("이름 고치기", rename), sbtn("같은 것과 합치기", merge)), slot));
      }
      if (!names.length) list.append(el("div", { class: "ac2-item" }, el("div", { class: "ac2-help" }, "저장된 이름이 없어요.")));
      // merges that kept a record can be undone (C17); older ones cannot
      const label = {};
      for (const n of names) { label[n.id] = n.name; (n.alias_ids || []).forEach((id, i) => { label[id] = (n.aliases || [])[i] || id; }); }
      merged.replaceChildren(...(r.json.merges || []).map((g) => {
        const slot = el("div");
        const undo = () => askHere(slot, (label[g.merge] || g.merge) + "을(를) " + (label[g.keep] || g.keep) + "에서 다시 떼어 낼까요? 합칠 때 옮긴 참조를 원래대로 돌리고, 다음 기억 찾기부터 따로 쓰여요.", "되돌리기", async () => {
          const res = await bridge("/names/unmerge", { method: "POST", body: JSON.stringify({ chat_id: chatId, merge_id: g.merge_id }), timeoutMs: 0 });
          if (res.ok) { row.remove(); await refreshShown(); }
          return res.code === "no_record" ? { ok: false, code: "unmerge_no_record" } : res;
        });
        const row = el("div", { class: "ac2-item" }, el("div", { class: "ac2-top", style: "align-items:center" },
          el("div", { class: "ac2-main" }, (label[g.merge] || g.merge) + " → " + (label[g.keep] || g.keep) + " 합침"), sbtn("되돌리기", undo)), slot);
        return row;
      }));
      mergedHead.hidden = !(r.json.merges || []).length;
    };
    const merged = el("div", { class: "ac2-list" });
    const mergedHead = el("div", { class: "ac2-label" }, "합친 이름");
    box.append(el("div", { class: "ac2-top" }, el("div", { class: "ac2-main" }, el("h1", { class: "ac2-h1" }, "이름"),
      el("span", { class: "ac2-help" }, "인물·장소·물건의 이름이에요. 고친 이름과 합친 별명은 다음 기억 찾기부터 쓰여요.")), sbtn("새로고침", load)),
      note.node, list, mergedHead, merged);
    load();
    liveView(box, load);
    return box;
  }

  // Worldline: the backend's map of a chat's turns, growing from turn 1 at
  // the bottom; the backend folds what it does not show, so a long chat
  // stays a short map. Drag to move, wheel or two fingers to zoom, a node to
  // see its turn.
  const WL_ROW = 64;
  const VERSION_TEXT = { reroll: "리롤", edit: "수정" };
  function svgEl(tag, attrs, ...kids) {
    const e = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (k === "on") for (const [ev, fn] of Object.entries(v)) e.addEventListener(ev, fn);
      else e.setAttribute(k, v);
    }
    for (const k of kids) if (k !== null && k !== undefined) e.append(k);
    return e;
  }
  function clip(s, n) {
    s = String(s || "");
    return s.length > n ? s.slice(0, n) + "…" : s;
  }
  async function worldlineTab(body, chatId) {
    const msg = msgBox();
    const openChat = (await chatInView()).open;
    chatId = chatId || (await chatInView()).id;
    if (!chatId) { body.append(msg.node); return msg.show(ERR_TEXT.chat_required, false); }
    const enc = encodeURIComponent(chatId);
    let opened = ""; // the one fold opened on this map (opening another folds it again)
    const view = { x: 0, y: 0, k: 1 };
    let places = []; // [turn, y] of the turns drawn
    let selected = 0;
    let dragged = false;
    const head = el("div", { class: "ac2-box" });
    const layer = svgEl("g");
    const map = el("div", { class: "ac2-map" }, svgEl("svg", { class: "ac2-wl", role: "group", "aria-label": "세계선 지도" }, layer));
    const detail = el("div", { class: "ac2-box" }, el("div", { class: "ac2-help" }, "노드를 누르면 그 턴의 기록과 기억이 여기에 나와요."));
    const apply = () => layer.setAttribute("transform", "translate(" + view.x + "," + view.y + ") scale(" + view.k + ")");
    const at = (y, share) => { // puts map y at share of the height
      const r = map.getBoundingClientRect();
      view.x = r.width < 520 ? 60 * view.k + 16 : r.width / 2 - 40 * view.k; // a phone: the trunk left, its texts in view
      view.y = r.height * share - y * view.k;
      apply();
    };
    const zoomAt = (px, py, k) => {
      k = Math.min(3, Math.max(0.2, k));
      view.x = px - (px - view.x) * k / view.k;
      view.y = py - (py - view.y) * k / view.k;
      view.k = k;
      apply();
    };
    const zoomBy = (f) => { const r = map.getBoundingClientRect(); zoomAt(r.width / 2, r.height / 2, view.k * f); };
    const newest = () => { if (places.length) at(places[places.length - 1][1], 0.25); };
    const first = () => { if (places.length) at(places[0][1], 0.7); };
    const fit = () => {
      const b = layer.getBBox(), r = map.getBoundingClientRect();
      if (!b.width || !b.height) return;
      view.k = Math.min(2, Math.max(0.2, Math.min((r.width - 40) / b.width, (r.height - 40) / b.height)));
      view.x = (r.width - b.width * view.k) / 2 - b.x * view.k;
      view.y = (r.height - b.height * view.k) / 2 - b.y * view.k;
      apply();
    };
    // drag, pinch and wheel; a press that moved is not a click
    const pts = new Map();
    let pinch = null;
    map.addEventListener("pointerdown", (e) => {
      pts.set(e.pointerId, { x: e.clientX, y: e.clientY, sx: e.clientX, sy: e.clientY });
      if (pts.size === 1) dragged = false;
      if (pts.size === 2) {
        const [a, b] = [...pts.values()];
        pinch = { d: Math.hypot(a.x - b.x, a.y - b.y) || 1, k: view.k };
      }
    });
    map.addEventListener("pointermove", (e) => {
      const p = pts.get(e.pointerId);
      if (!p) return;
      const dx = e.clientX - p.x, dy = e.clientY - p.y;
      p.x = e.clientX; p.y = e.clientY;
      if (!dragged && Math.hypot(e.clientX - p.sx, e.clientY - p.sy) < 5) return;
      if (!dragged) { dragged = true; try { map.setPointerCapture(e.pointerId); } catch (_) {} }
      if (pts.size === 1) { view.x += dx; view.y += dy; apply(); return; }
      if (pts.size === 2 && pinch) {
        const [a, b] = [...pts.values()], r = map.getBoundingClientRect();
        zoomAt((a.x + b.x) / 2 - r.left, (a.y + b.y) / 2 - r.top, pinch.k * Math.hypot(a.x - b.x, a.y - b.y) / pinch.d);
      }
    });
    const up = (e) => { pts.delete(e.pointerId); if (pts.size < 2) pinch = null; };
    map.addEventListener("pointerup", up);
    map.addEventListener("pointercancel", up);
    map.addEventListener("wheel", (e) => {
      e.preventDefault();
      const r = map.getBoundingClientRect();
      zoomAt(e.clientX - r.left, e.clientY - r.top, view.k * Math.exp(-e.deltaY * 0.0015));
    }, { passive: false });
    const button = (label, aria, fn) => el("button", { class: "ac2-btn", "aria-label": aria, on: { click: fn } }, label);
    const ctl = el("div", { class: "ac2-ctl" }, button("+", "확대", () => zoomBy(1.25)), button("−", "축소", () => zoomBy(0.8)),
      button("지금", "최근 턴으로", newest), button("처음", "첫 턴으로", first), button("전체", "전체 보기", fit));
    ctl.addEventListener("pointerdown", (e) => e.stopPropagation());
    map.append(ctl);
    const press = (g, fn) => {
      g.addEventListener("click", () => { if (!dragged) fn(); });
      g.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); fn(); } });
    };
    const other = (chat) => async () => { body.replaceChildren(); await worldlineTab(body, chat); };
    const badge = (text) => el("span", { class: "ac2-badge" }, text);

    const select = async (turn) => {
      selected = turn;
      for (const g of layer.querySelectorAll(".wl-node")) g.classList.toggle("wl-sel", Number(g.dataset.turn) === turn);
      detail.replaceChildren(el("div", { class: "ac2-help" }, turn + "턴을 불러오는 중…"));
      const memPath = "/memory/list?chat_id=" + enc + "&turn=" + turn + "&limit=" + MEM_PAGE + "&offset=";
      const [t, m] = await Promise.all([
        bridge("/worldline/turn?chat_id=" + enc + "&turn=" + turn, { method: "GET" }),
        bridge(memPath + "0", { method: "GET" })]);
      if (selected !== turn) return;
      const note = msgBox();
      if (!t.ok) { detail.replaceChildren(note.node); return note.show(errText(t), false); }
      const v = t.json;
      // the turn's memories a page at a time, as the memory tab shows them
      const mems = el("div", { class: "ac2-mlist" });
      const more = el("button", { class: "ac2-btn" }, "더 보기");
      let got = 0;
      const page = (r) => {
        if (!r.ok) { more.hidden = true; return mems.append(el("div", { class: "ac2-msg ac2-err" }, errText(r))); }
        for (const mm of r.json.memories) mems.append(memoryCard(chatId, mm));
        got += r.json.memories.length;
        more.hidden = got >= r.json.total;
      };
      more.addEventListener("click", async () => {
        if (more.disabled) return; // one page at a time: a second click would ask for the same offset
        more.disabled = true;
        const r = await bridge(memPath + got, { method: "GET" });
        more.disabled = false;
        if (selected === turn) page(r);
      });
      page(m);
      const versions = v.versions.map((x, i) => el("details", { class: "ac2-ver" },
        el("summary", {}, (VERSION_TEXT[x.kind] || x.kind) + " " + (i + 1)), el("div", { class: "ac2-pre" }, x.text)));
      detail.replaceChildren(...[
        el("div", { class: "ac2-row ac2-mhead" }, el("span", { class: "ac2-label" }, turn + "턴"), v.inherited ? badge("부모와 같음") : null, v.off ? badge("꺼짐") : null),
        el("div", { class: "ac2-help" }, "입력"), el("div", { class: "ac2-pre" }, v.user),
        el("div", { class: "ac2-help" }, "답"), el("div", { class: "ac2-pre" }, v.reply || "(아직 없음)"),
        versions.length ? el("div", { class: "ac2-help" }, "다른 버전 " + versions.length + "개 · 기억은 지금 답으로 만들어요.") : null, ...versions,
        el("div", { class: "ac2-label" }, v.off ? "꺼진 턴이라 기억을 만들거나 읽지 않아요." : "이 턴의 기억 " + (m.ok ? m.json.total : 0) + "개"),
        mems, more].filter(Boolean));
    };

    const branch = (lines, marks, fromY, toY, b, label) => {
      lines.append(svgEl("path", { d: "M0," + fromY + " C-70," + fromY + " -80," + toY + " -120," + toY, class: "wl-bline" }));
      const g = svgEl("g", { class: "wl-branch", role: "button", tabindex: "0", "aria-label": label + " 보기", transform: "translate(-120," + toY + ")" },
        svgEl("rect", { x: "-170", y: "-15", width: "170", height: "30", rx: "8" }),
        svgEl("text", { x: "-85", y: "5", "text-anchor": "middle" }, label + " · " + b.turns + "턴"));
      press(g, other(b.chat));
      marks.append(g);
    };
    const draw = (v) => {
      const lines = svgEl("g"), marks = svgEl("g");
      layer.replaceChildren(lines, marks);
      places = [];
      const y = (i) => -i * WL_ROW;
      let boxY = Infinity; // the last branch box drawn (boxes go up the map)
      v.nodes.forEach((n, i) => {
        if (i > 0) {
          const prev = v.nodes[i - 1];
          lines.append(svgEl("line", { x1: "0", y1: String(y(i - 1)), x2: "0", y2: String(y(i)), class: prev.inherited && n.inherited ? "wl-trunk wl-tin" : "wl-trunk" }));
        }
        if (n.fold) {
          const [a, b] = n.fold;
          const g = svgEl("g", { class: "wl-fold", role: "button", tabindex: "0", "aria-label": a + "–" + b + "턴 접힘, 펼치기", transform: "translate(0," + y(i) + ")" },
            svgEl("rect", { x: "-96", y: "-15", width: "192", height: "30", rx: "15" }),
            svgEl("text", { x: "0", y: "5", "text-anchor": "middle" }, a + "–" + b + "턴 접힘 · 펼치기"));
          press(g, async () => { opened = a + "-" + b; await load(b); });
          marks.append(g);
          return;
        }
        places.push([n.turn, y(i)]);
        const title = n.turn + "턴" + (n.inherited ? " · 부모와 같음" : "") + (n.off ? " · 꺼짐" : "") + (n.memories ? " · 기억 " + n.memories : "");
        const g = svgEl("g", { class: "wl-node" + (n.inherited ? " wl-in" : "") + (n.off ? " wl-off" : "") + (n.branches ? " wl-fork" : "") + (n.turn === selected ? " wl-sel" : ""),
          role: "button", tabindex: "0", "data-turn": String(n.turn), "aria-label": title, transform: "translate(0," + y(i) + ")" },
          svgEl("circle", { r: "9", class: "wl-dot" }),
          svgEl("text", { x: "20", y: "-4" }, title),
          svgEl("text", { x: "20", y: "13", class: "wl-p" }, clip(n.user, 28)));
        if (n.versions) {
          lines.append(svgEl("line", { x1: "0", y1: String(y(i)), x2: "-32", y2: String(y(i)), class: "wl-dash" }));
          g.append(svgEl("circle", { cx: "-40", r: "9", class: "wl-ver" }), svgEl("text", { x: "-40", y: "4", "text-anchor": "middle", class: "wl-vt" }, String(n.versions)));
        }
        press(g, () => select(n.turn));
        marks.append(g);
        for (const b of n.branches || []) {
          boxY = Math.min(y(i) - 30, boxY - 36); // above the turn, and above the box drawn before
          branch(lines, marks, y(i), boxY, b, "갈라진 세계선");
        }
      });
      // below turn 1: the chat this one branched from, and branches from the start
      let below = WL_ROW;
      if (v.parent) {
        const p = v.parent;
        lines.append(svgEl("line", { x1: "0", y1: "0", x2: "0", y2: String(below), class: "wl-dash" }));
        const g = svgEl("g", { class: "wl-branch", role: "button", tabindex: "0", "aria-label": "부모 세계선 보기", transform: "translate(0," + below + ")" },
          svgEl("rect", { x: "-110", y: "-15", width: "220", height: "30", rx: "8" }),
          svgEl("text", { x: "0", y: "5", "text-anchor": "middle" }, "부모 " + p.chat.slice(0, 8) + " · " + (p.turn ? p.turn + "턴에서" : "처음에서") + " 갈라짐"));
        press(g, other(p.chat));
        marks.append(g);
        below += WL_ROW;
      }
      (v.start_branches || []).forEach((b, j) => branch(lines, marks, 0, below - WL_ROW + 30 + j * 36, b, "처음에서 갈라짐"));
      if (!v.nodes.length) marks.append(svgEl("text", { x: "0", y: "0", "text-anchor": "middle", class: "wl-p" }, "아직 턴이 없어요."));
    };

    const load = async (around) => {
      msg.clear();
      const res = await bridge("/worldline?chat_id=" + enc + (opened ? "&open=" + opened : ""), { method: "GET" });
      if (!res.ok) return msg.show(errText(res), false);
      const v = res.json;
      const elsewhere = chatId !== openChat;
      head.replaceChildren(...[
        el("div", { class: "ac2-row ac2-mhead" },
          el("span", { class: "ac2-label" }, elsewhere ? "다른 세계선 · " + chatId.slice(0, 8) : "이 채팅의 세계선"),
          el("span", { class: "ac2-help" }, v.turns + "턴"),
          elsewhere && openChat ? el("button", { class: "ac2-btn", on: { click: other(openChat) } }, "열린 채팅으로") : null),
        v.parent ? el("div", { class: "ac2-help" }, "부모 세계선 " + v.parent.chat.slice(0, 8) + "의 " + (v.parent.turn ? v.parent.turn + "턴" : "처음") + "에서 갈라졌어요. 회색 구간은 부모와 같은 턴이에요.") : null,
        el("div", { class: "ac2-help" }, "끌어서 옮기고, 휠이나 두 손가락으로 확대·축소해요. 접힌 구간을 누르면 50턴씩 펼쳐요. 주황 테두리는 다른 세계선이 갈라진 턴이에요.")].filter(Boolean));
      draw(v);
      const place = places.find((p) => p[0] === around);
      if (place) at(place[1], 0.5); else newest();
    };
    body.append(head, map, msg.node, detail);
    await load(0);
  }

  // Chats tab (U1): the backend's GET /chats as cards, most recent first;
  // the open chat without records is shown as "기록 없음". choose(chat)
  // shows a chat's memories. Each card holds chatCardActions(chat, redraw).
  async function chatsTab(body, choose) {
    const msg = msgBox();
    const list = el("div", { class: "ac2-list" });
    const count = el("span", { class: "ac2-help" });
    const name = (chat) => [chat.char_name, chat.chat_name].filter(Boolean).join(" / ") || "채팅 " + chat.chat_id.slice(0, 8);
    let reads = 0; // only the latest read draws (review 07)
    // C22/C25: the chats to read before the open chat, in the order chosen
    // here (kept while the tab is drawn again); the backend attaches them
    const pick = [];
    const pastBox = el("div");
    let shown = null; // the last drawn open chat, names and attached past
    const attachPast = async (open, detach, slot, label) => {
      if (busyHere(slot)) return;
      const request = detach ? { chat_id: open, detach: true } : { chat_id: open, chats: pick.slice() };
      // the backend waits for running memory work before it writes: its answer is waited for
      const preview = detach ? null : await bridge("/chats/attach", { method: "POST", body: JSON.stringify({ ...request, dry_run: true }), timeoutMs: 0 });
      if (preview && !preview.ok) return slot.replaceChildren(el("div", { class: "ac2-msg ac2-err" }, errText(preview)));
      const say = detach ? "지금 채팅에 연결한 앞부분 과거를 풀까요? 그 채팅들의 기억은 그대로예요."
        : "이 순서로 지금 채팅 앞에 읽게 할까요? " + preview.json.chats.map((c) => label(c.chat_id) + "(턴 " + c.turns + " · 기억 " + c.memories + ")").join(" → ")
          + " → 지금 채팅. 모두 턴 " + preview.json.turns + "개 · 기억 " + preview.json.memories + "개. 복사하지 않고 원본은 그대로예요. 고른 채팅에 나중에 생기는 턴은 읽지 않아요. 이미 연결한 과거가 있으면 이것으로 바뀌어요.";
      askHere(slot, say, detach ? "연결 풀기" : "이어붙이기", async () => {
        const r = await bridge("/chats/attach", { method: "POST", body: JSON.stringify(request), timeoutMs: 0 });
        if (!r.ok) return r;
        // only what this request sent leaves the list; chats chosen meanwhile stay (review 11 R03)
        if (!detach) for (const id of request.chats) { const i = pick.indexOf(id); if (i >= 0) pick.splice(i, 1); }
        await refreshShown();
        msg.show(detach ? "앞부분 과거 연결을 풀었어요." : "앞부분 과거로 이어붙였어요 · 턴 " + r.json.turns + "개, 기억 " + r.json.memories + "개", true);
        return { ok: true, keep: false };
      }, true);
    };
    const drawPast = () => {
      pastBox.replaceChildren();
      if (!shown || !shown.open) return;
      const { open, names, past } = shown;
      const label = (id) => names.get(id) || id.slice(0, 8);
      const now = past && past.ok ? past.json.chats || [] : [];
      const slot = el("div");
      const acts = [];
      if (pick.length) acts.push(sbtn("지금 채팅 앞에 이어붙이기", () => attachPast(open, false, slot, label)), sbtn("목록 비우기", () => { pick.length = 0; drawPast(); }));
      if (now.length) acts.push(sbtn("과거 연결 풀기", () => attachPast(open, true, slot, label)));
      pastBox.append(el("section", { class: "ac2-box" }, el("h2", { class: "ac2-h2" }, "앞부분 과거 이어붙이기"),
        el("div", { class: "ac2-help" }, now.length ? "지금 연결: " + now.map((c) => label(c.chat_id) + "(턴 " + c.turns + " · 기억 " + c.memories + ")").join(" → ") + " → 지금 채팅"
          : "다른 채팅 카드의 '이어붙일 목록에 넣기'로 지금 채팅 앞에 읽을 과거를 순서대로 골라요."),
        pick.length ? el("div", { class: "ac2-list" }, ...pick.map((id, i) => el("div", { class: "ac2-item" }, el("div", { class: "ac2-top", style: "align-items:center" },
          el("div", { class: "ac2-main ac2-meta" }, (i + 1) + ". " + label(id)), sbtn("빼기", () => { pick.splice(i, 1); drawPast(); }))))) : null,
        acts.length ? el("div", { class: "ac2-acts" }, ...acts) : null, slot));
    };
    const addPick = (id) => { if (!pick.includes(id)) pick.push(id); drawPast(); };
    // C26: this chat beside the open one, as the backend counts and previews
    // them (read only; the first few originals and memories of each)
    const compare = async (id, slot, label) => {
      if (!shown || !shown.open) return;
      slot.replaceChildren(el("div", { class: "ac2-help" }, "비교하는 중…"));
      const r = await bridge("/chats/compare?left_chat_id=" + encodeURIComponent(id) + "&right_chat_id=" + encodeURIComponent(shown.open) + "&left_limit=5&right_limit=5", { method: "GET" });
      if (!r.ok) return slot.replaceChildren(el("div", { class: "ac2-msg ac2-err" }, r.code === "chat_not_found" ? "한쪽 채팅의 기록이 없어 비교할 수 없어요." : errText(r)));
      const cut = (t) => { const v = String(t || ""); return v.length > 120 ? v.slice(0, 120) + "…" : v; };
      const side = (v, head) => el("div", { class: "ac2-field" },
        el("div", { class: "ac2-title2" }, head + " · " + label(v.chat_id)),
        el("div", { class: "ac2-meta" }, "턴 " + v.turns + " · 기억 " + v.memories.total + " (사건 " + v.memories.event + " · 사실 " + v.memories.fact + " · 생각 " + v.memories.view + ") · 이름 " + v.names),
        el("div", { class: "ac2-meta" }, "마지막 활동 " + (v.last_at ? new Date(v.last_at).toLocaleString() : "없음") + (v.branch_of ? " · " + label(v.branch_of.chat_id) + "에서 갈라짐" : "")),
        el("div", { class: "ac2-help" }, "앞 턴 원문"),
        ...(v.originals.length ? v.originals.map((o) => el("div", { class: "ac2-meta" }, o.turn_id + "턴 · " + cut(o.input) + " → " + cut(o.response))) : [el("div", { class: "ac2-meta" }, "없음")]),
        el("div", { class: "ac2-help" }, "앞 기억"),
        ...(v.memory_preview.length ? v.memory_preview.map((m) => el("div", { class: "ac2-meta" }, m.id + " · " + (KIND_TEXT[m.kind] || m.kind) + (m.secret ? " · 비밀" : "") + (m.inherited ? " · 물려받음" : "") + " · " + cut(m.body))) : [el("div", { class: "ac2-meta" }, "없음")]));
      slot.replaceChildren(el("div", { class: "ac2-panel" }, el("div", { class: "ac2-grid" }, side(r.json.left, "이 채팅"), side(r.json.right, "지금 채팅")),
        el("div", { class: "ac2-row" }, sbtn("닫기", () => slot.replaceChildren()))));
    };
    const redraw = async () => {
      const mine = ++reads;
      msg.clear();
      const res = await bridge("/chats", { method: "GET" });
      if (mine !== reads) return;
      if (!res.ok) return msg.show(errText(res), false);
      const view = await chatInView(); // one host read: the chat in view and the open chat
      if (mine !== reads) return;
      // the backend's order and counts; only the open chat without records is added here
      const chats = res.json.chats.slice();
      const names = new Map(chats.map((chat) => [chat.chat_id, name(chat)]));
      if (view.open && !names.has(view.open)) {
        chats.unshift({ chat_id: view.open, char_name: "", chat_name: view.isOpen ? view.name : "", turns: 0,
          memories: { total: 0, event: 0, fact: 0, view: 0 }, names: 0, last_at: null, branch_of: null });
      }
      // the open chat's attached past (C22), read from the backend
      const past = view.open ? await bridge("/chats/attach?chat_id=" + encodeURIComponent(view.open), { method: "GET" }) : null;
      if (mine !== reads) return;
      if (past && !past.ok) msg.show("지금 채팅의 앞부분 과거 연결을 읽지 못했어요. " + errText(past), false);
      shown = { open: view.open, names, past };
      drawPast();
      count.textContent = "채팅 " + chats.length + "개";
      list.replaceChildren();
      for (const chat of chats) {
        const title = name(chat);
        const m = chat.memories;
        const b = chat.branch_of;
        const recorded = names.has(chat.chat_id);
        const item = el("div", { class: "ac2-item", "aria-current": String(chat.chat_id === view.id) },
          el("div", { class: "ac2-top" },
            el("div", { class: "ac2-main" },
              el("div", { class: "ac2-title2" }, title),
              el("div", { class: "ac2-meta" }, recorded
                ? "턴 " + chat.turns + " · 기억 " + m.total + " (사건 " + m.event + " · 사실 " + m.fact + " · 생각 " + m.view + ") · 이름 " + chat.names
                : "아직 기록이 없어요"),
              el("div", { class: "ac2-meta" }, "마지막 활동 " + (chat.last_at ? new Date(chat.last_at).toLocaleString() : "없음")
                + (b ? " · " + (names.get(b.chat_id) || b.chat_id.slice(0, 8)) + "에서" + (b.turn ? " " + b.turn + "턴 뒤" : "") + " 갈라짐" : ""))),
            el("div", { class: "ac2-tags" },
              recorded ? null : tag("기록 없음"),
              chat.chat_id === view.open ? tag("현재 채팅", "acc") : null,
              chat.chat_id === view.id ? tag("보는 중", "blu") : null)));
        const slot = el("div");
        item.append(el("div", { class: "ac2-acts" }, sbtn("기억 보기", () => choose({ id: chat.chat_id, name: title })), ...chatCardActions(chat, redraw, view.open, slot, (t) => msg.show(t, true)),
          view.open && view.open !== chat.chat_id ? sbtn("이어붙일 목록에 넣기", () => addPick(chat.chat_id)) : null,
          view.open && view.open !== chat.chat_id && recorded ? sbtn("지금 채팅과 비교", () => compare(chat.chat_id, slot, (id) => names.get(id) || id.slice(0, 8))) : null), slot);
        list.append(item);
      }
      if (!chats.length) list.append(el("div", { class: "ac2-item" }, el("div", { class: "ac2-help" }, "기록된 채팅이 없어요.")));
    };
    body.append(el("div", { class: "ac2-top" }, el("div", { class: "ac2-main" }, el("h1", { class: "ac2-h1" }, "채팅"), count),
      sbtn("새로고침", redraw)), msg.node, pastBox, list);
    liveView(list, redraw);
    await redraw();
  }
  // A chat's actions (U2's backend): copy or move it into the open chat, or
  // delete it. The backend's preview counts fill the confirmation under the
  // row; the result stays there after the list is drawn again.
  const OP_TEXT = { delete: "삭제", copy: "지금 채팅으로 복사", move: "지금 채팅으로 이동" };
  function chatCardActions(chat, redraw, openId, slot, report) {
    const id = String(chat.chat_id || chat.id || "");
    slot = slot || el("div");
    const ask = async (op) => {
      if (busyHere(slot)) return; // nothing is read or sent again
      const to = op === "delete" ? "" : openId !== undefined ? openId : await currentChatId();
      const request = op === "delete" ? { chat_id: id } : { from: id, to };
      const path = "/chats/" + op;
      const preview = await bridge(path, { method: "POST", body: JSON.stringify({ ...request, dry_run: true }) });
      if (!preview.ok) return slot.replaceChildren(el("div", { class: "ac2-msg ac2-err" }, errText(preview)));
      const p = preview.json;
      const copied = op === "delete" ? "" : "옮길 턴 " + p.copied_turns + "개 · 기억 " + p.copied_memories + "개"
        + (p.unmatched_turns ? " · 짝이 안 맞아 못 옮기는 턴 " + p.unmatched_turns + "개" : "") + (p.existing_turns ? " · 이미 기억이 있어 그대로 둘 턴 " + p.existing_turns + "개" : "");
      const deleted = op === "copy" ? "" : "지울 턴 " + p.deleted_turns + "개 · 기억 " + p.deleted_memories + "개"
        + ((p.branches || []).length ? " · 갈라진 채팅 " + p.branches.length + "개는 빌려 쓰던 기억을 먼저 복사해 둬요" : "")
        + ((p.past_readers || []).length ? " · 이 채팅을 앞부분 과거로 연결한 채팅 " + p.past_readers.length + "개는 연결이 풀려요" : "");
      const say = { delete: "이 채팅의 Archive Center 기록을 지울까요? ", copy: "이 채팅의 기억을 지금 채팅으로 복사할까요? 원본은 그대로예요. ",
        move: "이 채팅의 기억을 지금 채팅으로 옮기고 원본 기록을 지울까요? " }[op] + [copied, deleted].filter(Boolean).join(" · ") + (op === "copy" ? "" : ". RisuAI 대화 원문은 그대로예요.");
      askHere(slot, say, OP_TEXT[op], async () => {
        const r = await bridge(path, { method: "POST", body: JSON.stringify(request) });
        if (!r.ok) return r;
        if (op !== "copy" && (await chatInView()).id === id) showChat(null);
        const v = r.json;
        await refreshShown();
        const text = OP_TEXT[op] + " 완료 · " + [op === "delete" ? "" : "옮긴 턴 " + v.copied_turns + "개, 기억 " + v.copied_memories + "개",
          op === "copy" ? "" : "지운 턴 " + v.deleted_turns + "개, 기억 " + v.deleted_memories + "개"].filter(Boolean).join(" · ");
        // the list was drawn again: the result goes above it (report), else
        // under this row
        if (report) report(text); else slot.replaceChildren(el("div", { class: "ac2-msg ac2-ok", role: "status" }, text));
        return { ok: true, keep: !report };
      }, op === "copy");
    };
    const acts = [];
    if (openId && openId !== id) acts.push(sbtn(OP_TEXT.copy, () => ask("copy")), sbtn(OP_TEXT.move, () => ask("move")));
    acts.push(sbtn(OP_TEXT.delete, () => ask("delete"), "ac2-danger"));
    return acts;
  }
  // Maintenance at the end of the status tab (U4): reindex, full database
  // reset; null = none yet.
  function maintenanceBox() {
    const box = el("div", { class: "ac2-field", style: "gap:14px" });
    // the turn HUD on the chat screen: on/off and compact, kept in the backend
    const hudBox = el("section", { class: "ac2-box" }, el("h2", { class: "ac2-h2" }, "채팅 화면 표시"));
    const hudNote = msgBox();
    (async () => {
      const r = await bridge("/settings/hud", { method: "GET" });
      if (!r.ok) { hudBox.append(hudNote.node); return hudNote.show(errText(r), false); }
      const cur = { enabled: !!r.json.enabled, compact: !!r.json.compact };
      const save = (k) => async (v) => {
        hudNote.clear();
        const next = { ...cur, [k]: v };
        const saved = await bridge("/settings/hud", { method: "POST", body: JSON.stringify(next) });
        if (!saved.ok) { hudNote.show(errText(saved), false); return false; }
        cur[k] = v;
        hudSettingsChanged();
        hudNote.show("저장했어요.", true);
      };
      hudBox.append(el("div", { class: "ac2-help" }, "채팅 화면 구석에 기억 만들기 진행을 작게 보여 줘요. 넣은 기억은 이번 턴 탭에서 봐요."),
        toggle("ac2-hud-on", "채팅 화면에 진행 표시", cur.enabled, save("enabled")), toggle("ac2-hud-compact", "처음에는 한 줄로 접어 두기", cur.compact, save("compact")), hudNote.node);
    })();

    // vectors: fill the missing ones or make them all again, for the chat in
    // view or every chat; the counts come first, from the backend
    const vec = el("section", { class: "ac2-box" }, el("h2", { class: "ac2-h2" }, "벡터 정비"));
    const note = msgBox();
    const slot = el("div");
    const progress = el("div", { class: "ac2-stats" });
    const scope = el("select", { "aria-label": "정비 대상" }, el("option", { value: "chat" }, "지금 보는 채팅"), el("option", { value: "all" }, "전체 채팅"));
    const error = (r) => ({ embedding_off: "임베딩이 꺼져 있어요. 모델과 API 키 설정을 확인해 주세요.",
      reset_phrase_mismatch: "‘전체 DB 초기화’를 정확히 입력해 주세요." }[r.code] || errText(r));
    let reads = 0; // only the latest read draws (review 07)
    const showProgress = async () => {
      const mine = ++reads;
      const r = await bridge("/status", { method: "GET" });
      if (mine !== reads) return;
      if (!r.ok) return note.show(error(r), false);
      const e = r.json.embedding;
      progress.replaceChildren(el("div", { class: "ac2-stat" }, "벡터를 기다리는 기억", el("b", {}, String(e.waiting_vectors))),
        el("div", { class: "ac2-stat" }, "임베딩 실패", el("b", {}, String(e.failures ?? 0))));
      if (e.index_failing) progress.append(el("div", { class: "ac2-warn", style: "grid-column:1/-1" }, "임베딩 실패 · " + (e.index_detail || "다시 시도를 기다려요.")));
    };
    const ask = async (mode) => {
      if (writing) return note.show(BUSY_TEXT, false); // nothing is read or sent again
      note.clear();
      slot.replaceChildren();
      const req = { mode };
      let target = "전체 채팅";
      if (scope.value !== "all") {
        const chat = await chatInView();
        if (!chat.id) return note.show(ERR_TEXT.chat_required, false);
        req.chat_id = chat.id;
        target = chat.name || chat.id.slice(0, 8);
      }
      const r = await bridge("/maintenance/reindex?mode=" + mode + (req.chat_id ? "&chat_id=" + encodeURIComponent(req.chat_id) : ""), { method: "GET" });
      if (!r.ok) return note.show(error(r), false);
      const p = r.json;
      if (!p.count) return note.show("대상 기억이 없어요.", true);
      askHere(slot, "대상: " + target + " · " + p.chats + "개 채팅, " + p.count + "개 기억 · " + p.model + ". " + (mode === "all" ? "지금 벡터를 지우고 새로 만들어요. " : "") + "임베딩 호출 비용이 들어요.",
        mode === "all" ? "다시 만들기" : "채우기", async () => {
          const done = await bridge("/maintenance/reindex", { method: "POST", body: JSON.stringify(req) });
          if (!done.ok) return { ...done, code: done.code };
          note.show(done.json.count + "개 기억의 벡터 작업을 맡겼어요. 아래 숫자로 진행을 확인하세요.", true);
          refreshChip();
          await refreshShown();
          return { ok: true };
        }, true);
    };
    vec.append(el("div", { class: "ac2-help" }, "임베딩 모델을 바꿨거나 실패한 벡터가 쌓였을 때 써요. 벡터가 없는 동안 기억은 단어와 연결로 골라요."),
      el("div", { class: "ac2-acts" }, scope, sbtn("빠진 벡터 채우기", () => ask("missing")), sbtn("벡터 전부 다시 만들기", () => ask("all")), sbtn("새로고침", showProgress)),
      slot, note.node, progress);
    showProgress();

    // the full reset, behind the typed phrase (user decision 10-28)
    const reset = el("section", { class: "ac2-box ac2-dangerzone" }, el("h2", { class: "ac2-h2", style: "color:var(--bad)" }, "전체 DB 초기화"));
    const phrase = el("input", { "aria-label": "초기화 확인 문구", placeholder: "전체 DB 초기화", autocomplete: "off" });
    const rnote = msgBox();
    const doReset = async () => {
      rnote.clear();
      const r = await bridge("/maintenance/reset", { method: "POST", body: JSON.stringify({ phrase: phrase.value }) });
      if (!r.ok) return rnote.show(error(r), false);
      phrase.value = "";
      rnote.show("모든 채팅의 기록을 지웠어요. 설정과 접속 토큰, 보낼 상자는 그대로예요.", true);
      refreshChip();
      await showProgress();
    };
    reset.append(el("div", { class: "ac2-help" }, "모든 채팅의 관찰·기억·이름·벡터를 지워요. 되돌릴 수 없어요. 설정과 접속 토큰은 남아요. 지우려면 아래에 ‘전체 DB 초기화’를 그대로 입력하세요."),
      el("div", { class: "ac2-acts" }, phrase, el("button", { type: "button", class: "ac2-btn ac2-danger", on: { click: doReset } }, "전체 DB 초기화")), rnote.node);

    box.append(hudBox, vec, reset);
    liveView(box, showProgress);
    return box;
  }

  // Turn HUD (U6's backend; UI design 10-27 "채팅 안 상태 표시"): one chip on
  // the chat screen, "AC2 · 직전 턴 기억 …", that opens to how the last
  // turns' memory is going, with a retry for a failed turn. What was given
  // stays in the settings' this-turn tab (user 10-28). Every count, stage and retry choice is Go's (/hud).
  // The chip lives in the host's page (SafeElement, asynchronous); a refused
  // page permission leaves it off and the rest of the plugin as it is. The
  // hooks tell it the chat they read (hudSee); it asks again only while a
  // turn is under way.
  const HUD_STATE = { waiting: "대기", running: "진행 중", done: "완료", failed: "실패", retry_waiting: "다시 시도 대기", off: "사용 안 함", skipped: "건너뜀" };
  const HUD_GEN = { done: ["직전 턴 기억 저장됨", "acc"], running: ["직전 턴 기억 만드는 중…", "warn"], waiting: ["직전 턴 기억 기다리는 중", "warn"],
    retry_waiting: ["직전 턴 기억 다시 시도 대기", "warn"], failed: ["직전 턴 기억 만들지 못함", "bad"], off: ["직전 턴은 기억 안 함", "gray"] };
  const HUD_NOTICE = { response_deleted: "삭제를 반영했어요", member_deleted: "삭제를 반영했어요", turn_deleted: "삭제를 반영했어요", response_replaced: "리롤로 바뀐 답을 반영했어요",
    response_new_generation: "리롤로 바뀐 답을 반영했어요", member_edited: "고친 입력을 반영했어요", response_revised: "고친 답을 반영했어요", turn_off: "사용 안 함(OOC)으로 바뀐 턴", reply_off: "응답의 사용 여부가 바뀌었어요" };
  // tones (user 11-08 HUD rebuild): blue under way, green saved, red failed, gray off
  const HUD_TONE = { run: ["rgba(37,99,235,.14)", "#2563EB"], acc: ["rgba(20,137,126,.16)", "#14897E"], bad: ["rgba(197,48,48,.16)", "#C53030"], gray: ["rgba(128,128,128,.18)", "#7A776E"] };
  const HUD_PREP = { jev: "판정 모델", ai: "AI", ai_jev: "AI + 판정 모델", go: "Go 기본" };
  const HUD_PREP_FALLBACK = { decision_off: "판정 모델 없음", ai_off: "AI 연결 없음", decision_failed: "판정 모델 실패", ai_failed: "AI 실패", ai_and_decision_failed: "AI·판정 모델 실패", store_failed: "설정 읽기 실패", wide_list_failed: "넓은 목록 실패", final_delivery_failed: "최종 전달 실패", nothing_to_choose: "고를 기억 없음" };
  const HUD_LINK = { timeout: "응답 시간 초과", connect_failed: "연결할 수 없음", http_error: "백엔드 오류", decode_failed: "응답을 읽지 못함", body_read_failed: "응답을 읽지 못함" };
  const secs = (ms) => (ms / 1000).toFixed(1) + "초";
  // the chip of a view: the earlier turn's generation (made at the next
  // input), or the request that could not reach the backend
  function hudChip(v, link, readFail) {
    if (link) return ["연결 실패", "bad"];
    if (readFail) return ["상태 못 읽음", "bad"];
    const turns = (v && v.turns) || [];
    // an earlier turn's failure outside the latest two (review 19 B-04)
    const f = ((v && v.failures) || [])[0];
    const older = f && (f.state === "waiting" ? ["다시 시도 요청함", "run"] : f.state === "running" ? [f.turn + "턴 다시 시도 중", "run"] : [f.turn + "턴 실패" + (f.state === "retry_waiting" ? " · 곧 다시" : ""), "bad"]);
    const t = turns[1] || turns[0];
    if (!t) return older || ["확인 중", "gray"];
    const g = (t.stages || []).find((s) => s.key === "generation") || {};
    if (older && (g.state === "done" || g.state === "off" || !g.state)) return older;
    // a retry is told from its first call too (review 19 R2-B04)
    if (g.state === "running") return [t.retried ? "다시 시도 중 " + t.attempt + "/" + t.attempts : "만드는 중", "run"];
    if (g.state === "waiting") return [t.retried ? "다시 시도 요청함" : "기다리는 중", "run"];
    if (g.state === "retry_waiting") return [t.turn + "턴 실패 · 곧 다시", "bad"];
    if (g.state === "failed") return [t.turn + "턴 실패", "bad"];
    if (g.state === "done") return [(g.count || 0) + "개 저장", "acc"];
    if (g.state === "off") return ["기억 안 함", "gray"];
    return ["확인 중", "gray"];
  }
  // narrative support (E03): what the guide and the input improvement did
  const NARR_TEXT = { ai_off: "출판사 연결 없음", ai_failed: "AI 실패", store_failed: "설정 읽기 실패", nothing_to_guide: "넣은 비밀 아닌 기억 없음", nothing_kept: "근거 있는 항목 없음",
    no_budget: "예산 0", no_time_left: "시간 부족", no_input_text: "입력 글 없음" };
  const GUIDE_TEXT = { weak: "약하게", mid: "중간", strong: "강하게" };
  function narrLine(n) {
    const quiet = { made: 1, nothing_kept: 1, nothing_to_guide: 1, changed: 1, unchanged: 1 };
    const parts = [];
    if (n.level) parts.push("가이드 " + (GUIDE_TEXT[n.level] || n.level) + " · " + (n.guide === "made" ? n.kept + "개" + (n.dropped ? " (근거 없어 뺌 " + n.dropped + ")" : "") : NARR_TEXT[n.guide] || n.guide || ""));
    else if (n.guide) parts.push(NARR_TEXT[n.guide] || n.guide);
    if (n.input) parts.push(n.input_result === "changed" && n.input === "blend" ? "입력에 기억 녹임" : "입력 " + (n.input_result === "changed" ? "다듬음" : n.input_result === "unchanged" ? "그대로" : "원문 그대로 · " + (NARR_TEXT[n.input_result] || n.input_result || "")));
    const warn = (n.guide && !quiet[n.guide]) || (n.input && n.input_result && !quiet[n.input_result]);
    return [warn ? "!" : "✓", "서사 지원 " + secs(n.ms || 0), parts.join(" · "), warn ? "warn" : ""];
  }
  // what one turn shows: [mark, title, detail, tone], in order
  function hudLines(t) {
    const st = {};
    for (const s of t.stages || []) st[s.key] = s;
    const out = [];
    const inj = st.injected || {};
    const p = t.injection && t.injection.preprocess;
    if (p) {
      // the mode chosen; when it fell back, why; otherwise what it did
      const calls = [p.decision_calls ? "판정 " + p.decision_calls + "번" : "", p.ai_calls ? "AI " + p.ai_calls + "번" : ""].filter(Boolean).join(" · ");
      const detail = p.ran === "go" ? (HUD_PREP[p.mode] || p.mode) + " 못 함 → Go 기본 · " + (HUD_PREP_FALLBACK[p.fallback] || p.fallback || "")
        : [HUD_PREP[p.ran] || p.ran, p.ran !== p.mode ? "(" + (HUD_PREP_FALLBACK[p.fallback] || p.fallback || "") + ")" : "", calls, "뺌 " + p.removed + " · 더함 " + p.added].filter(Boolean).join(" · ");
      out.push([p.ran === "go" ? "!" : "✓", "기억 고르기 " + secs(p.ms || 0), detail, p.ran === "go" ? "warn" : ""]);
    }
    const n = t.injection && t.injection.narrative;
    if (n) out.push(narrLine(n));
    if (inj.state === "done") out.push(["✓", "기억 넣음" + (inj.elapsed_ms != null ? " " + secs(inj.elapsed_ms) : ""), (inj.count || 0) + "개" + (inj.detail ? " · " + (WARNING_TEXT[inj.detail] || inj.detail) : ""), ""]);
    const rs = st.response_saved || {};
    if (rs.state === "done") out.push(["✓", "응답 저장", "", ""]);
    else if (inj.state === "done") out.push(["…", "응답 기다리는 중" + (rs.elapsed_ms != null ? " " + secs(rs.elapsed_ms) : ""), "", ""]);
    const cf = st.confirmed || {};
    out.push(cf.state === "done" ? ["✓", "다음 입력으로 확정", "", ""] : ["…", "다음 입력 기다리는 중", "기억은 다음 입력 때 만들어요", ""]);
    const g = st.generation || {};
    if (g.state === "done") out.push(["✓", "기억 " + (g.count || 0) + "개 만듦" + (g.elapsed_ms != null ? " " + secs(g.elapsed_ms) : ""), t.attempt > 1 && t.attempts ? t.attempts + "번 중 " + t.attempt + "번째에 성공" : "", ""]);
    else if (g.state === "running") out.push(["…", (t.retried ? "다시 시도 중" : "기억 만드는 중") + (g.elapsed_ms != null ? " " + secs(g.elapsed_ms) : ""), t.attempts ? t.attempts + "번 중 " + t.attempt + "번째" : "", ""]);
    else if (g.state === "waiting" && cf.state === "done") out.push(["…", t.retried ? "다시 시도 요청함" : "기억 만들기 기다리는 중", "", ""]);
    else if (g.state === "off") out.push(["–", "이 턴은 기억 안 함", "사용 안 함(OOC)", ""]);
    else if (g.state === "failed" || g.state === "retry_waiting") out.push(["fail", "기억 만들기 실패", g.detail ? (WARNING_TEXT[g.detail] || g.detail) : "", "bad"]);
    const vc = st.vectors || {};
    if (vc.state === "done") out.push(["✓", "벡터 저장" + (vc.elapsed_ms != null ? " " + secs(vc.elapsed_ms) : ""), "", ""]);
    else if (vc.state === "skipped") out.push(["–", "벡터 저장 건너뜀", "만든 기억이 없어요", ""]);
    else if (vc.state === "running") out.push(["…", "벡터 저장 중", "", ""]);
    else if (vc.state === "failed" || vc.state === "retry_waiting") out.push(["!", "벡터 저장 실패", vc.detail || "", "warn"]);
    return out;
  }
  function hudStart() {
    const ui = { chat: "", view: null, doc: null, denied: false, root: null, listeners: [], timer: null, asking: false, again: false, open: null, dismissed: false, epoch: 0, serial: Promise.resolve(), stopped: false, lines: [], lineTurns: [], links: {}, readFail: null, more: null };
    const safe = (fn) => (...args) => Promise.resolve().then(() => fn(...args)).catch((e) => { recordPluginError(e); console.warn("[AC2] HUD", e); });
    const cancel = () => { if (ui.timer) clearTimeout(ui.timer); ui.timer = null; };
    const remove = async () => {
      for (const [target, id] of ui.listeners) await target.removeEventListener("click", id);
      ui.listeners = [];
      if (ui.root) await ui.root.remove();
      ui.root = null;
    };
    const node = async (tag, text, style) => {
      const n = await ui.doc.createElement(tag);
      if (text != null) await n.setTextContent(String(text));
      if (style) await n.setStyleAttribute(style);
      return n;
    };
    const add = async (parent, tag, text, style) => { const n = await node(tag, text, style); await parent.appendChild(n); return n; };
    const click = async (n, fn) => { ui.listeners.push([n, await n.addEventListener("click", safe(fn))]); };
    const BTN = "font:inherit;font-size:12px;color:inherit;background:transparent;border:1px solid rgba(128,128,128,.45);border-radius:6px;min-height:30px;padding:0 10px;cursor:pointer";
    const MARK = { "✓": "#14897E", "…": "#2563EB", "!": "#B7791F", "–": "#7A776E" };
    const draw = async () => {
      const v = ui.view;
      const enabled = !!(v && v.settings && v.settings.enabled);
      // a request that did not reach the backend is told even before any
      // view was read (review 19 B-05); a failed HUD read only once one was
      const link = (!v || enabled) && ui.links[ui.chat] || null;
      const readFail = enabled && ui.readFail && ui.readFail.chat === ui.chat ? ui.readFail : null;
      const turns = (enabled && v.turns) || [];
      const failures = (enabled && v.failures) || [];
      const hidden = ui.stopped || unloaded || ui.dismissed || ui.denied || !(turns.length || failures.length || link || readFail);
      // the host keeps every element it ever handed out: the chip is built
      // again only when what it shows changes; elapsed times alone rewrite
      // the text of the lines already shown
      let shown = null;
      if (!hidden) {
        const open = ui.open == null ? !(v && v.settings && v.settings.compact) : ui.open;
        shown = JSON.stringify([open, hudChip(v, link, readFail), open ? v : null, open ? link : null, open ? readFail : null, ui.more], (k, x) => (k === "elapsed_ms" ? undefined : x));
        if (ui.root && ui.body && shown === ui.shown) {
          // the turns of the view just read (the lines shown follow them)
          const now0 = turns[0], now1 = turns[1] || now0;
          const texts = open ? [now0, now1 === now0 ? null : now1].filter(Boolean).flatMap((t) => hudLines(t).map((l) => l[1])) : [];
          for (let i = 0; i < texts.length && i < ui.lines.length; i++) {
            if (ui.lines[i][1] !== texts[i]) { await ui.lines[i][0].setTextContent(texts[i]); ui.lines[i][1] = texts[i]; }
          }
          await ui.body.appendChild(ui.root);
          return;
        }
      }
      await remove();
      ui.shown = null;
      if (hidden) return;
      if (!ui.doc) {
        try { ui.doc = await api.getRootDocument(); } catch (_) { ui.denied = true; return; }
        if (!ui.doc) { ui.denied = true; return; }
      }
      if (ui.stopped || unloaded) return;
      const body = await ui.doc.querySelector("body");
      if (!body) return;
      const open = ui.open == null ? !(v && v.settings && v.settings.compact) : ui.open;
      const latest = turns[0];
      const prev = turns[1] || latest;
      const [chipText, tone] = hudChip(v, link, readFail);
      const [bg, dot] = HUD_TONE[tone] || HUD_TONE.gray;
      ui.lines = [];
      ui.lineTurns = [latest, prev === latest ? null : prev].filter(Boolean);
      // narrow (user 11-08 "좌우로 길게 나오는 느낌"): about 300 px, two-line
      // entries, stacked from the bottom with the panel above the chip
      const wrap = await node("section", null, "position:fixed;right:12px;bottom:calc(35vh + env(safe-area-inset-bottom,0px));z-index:1000;width:min(300px,calc(100vw - 24px));display:flex;flex-direction:column-reverse;align-items:flex-end;gap:6px;font:12px/1.5 system-ui,'IBM Plex Sans KR',sans-serif;color:CanvasText;box-sizing:border-box");
      ui.root = wrap;
      const chip = await add(wrap, "button", null, "display:inline-flex;align-items:center;gap:6px;min-height:28px;padding:0 10px;border:0;border-radius:999px;font:inherit;cursor:pointer;color:" + dot + ";background:" + bg + ";box-shadow:0 1px 4px rgba(0,0,0,.12)");
      await add(chip, "span", null, "width:7px;height:7px;border-radius:50%;flex:none;background:" + dot);
      await add(chip, "span", "AC2 · " + chipText);
      await click(chip, async () => { ui.open = !open; await render(); });
      if (open) {
        const panel = await add(wrap, "div", null, "width:100%;box-sizing:border-box;max-height:calc(65vh - 24px);overflow:auto;background:Canvas;border:1px solid rgba(128,128,128,.35);border-radius:12px;padding:12px 14px;display:flex;flex-direction:column;gap:6px;box-shadow:0 3px 14px rgba(0,0,0,.14)");
        const head = await add(panel, "div", null, "display:flex;align-items:center;gap:8px");
        await add(head, "div", "AC2 기억 처리", "flex:1;font-weight:600;font-size:13px");
        const x = await add(head, "button", "×", BTN + ";min-width:28px;min-height:28px;padding:0;font-size:15px");
        await x.setAttribute("aria-label", "닫기");
        await click(x, async () => { ui.dismissed = true; await render(); });
        const RED = "background:rgba(197,48,48,.12);color:#C53030;border-radius:8px;padding:8px 10px;display:flex;flex-direction:column;gap:3px";
        // a failure stands out with what happened and a retry where it shows
        const failBox = async (parent, t, title, detailText, full, state) => {
          const box = await add(parent, "div", null, RED);
          const head = await add(box, "div", title, "font-weight:600");
          if (detailText) await add(box, "div", detailText);
          const tries = t.attempts && t.attempt > 0 ? t.attempts + "번 중 " + t.attempt + "번째" : "";
          const again = t.next_at ? "다음 시도 " + clock(t.next_at) : state === "retry_waiting" ? "곧 다시 시도" : "";
          const left = again ? [tries ? tries + " 실패" : "", again].filter(Boolean).join(" · ") : [tries, "자동 재시도 끝"].filter(Boolean).join(" · ");
          await add(box, "div", left);
          await add(box, "div", "대화는 그대로 이어지고, 이 턴의 기억만 비어 있어요.", "opacity:.85");
          const row = await add(box, "div", null, "display:flex;gap:6px;margin-top:2px");
          const chat = ui.chat;
          if (t.retry) {
            const b = await add(row, "button", "다시 시도", BTN);
            await click(b, async () => {
              const r = await bridge("/hud/retry", { method: "POST", body: JSON.stringify({ chat_id: chat, turn: t.turn }) });
              if (!r.ok) { await add(box, "div", errText(r)); return; }
              if (ui.chat === chat) { ui.view = r.json; await render(); poll(); }
            });
          }
          if (detailText) {
            const m = await add(row, "button", ui.more === t.turn ? "접기" : "자세히", BTN);
            await click(m, async () => { ui.more = ui.more === t.turn ? null : t.turn; await render(); });
            if (ui.more === t.turn) await add(box, "div", "기록된 오류: " + (full || "없음"), "font-family:ui-monospace,monospace;font-size:11px;word-break:break-all");
          }
          return head;
        };
        if (link) {
          const box = await add(panel, "div", null, RED);
          await add(box, "div", "백엔드 연결 실패", "font-weight:600");
          await add(box, "div", (HUD_LINK[link.kind] || link.kind || "") + (link.status ? " · " + link.status : "") + " · " + clock(link.at));
          await add(box, "div", link.aux ? "보조 요청(번역 등)이라 기억과는 상관없어요." : "이 요청은 기억 없이 보냈어요.", "opacity:.85");
        }
        if (readFail) {
          // the view below is the last one read (review 19 B-07)
          const box = await add(panel, "div", null, RED);
          await add(box, "div", "HUD 상태를 읽지 못함", "font-weight:600");
          await add(box, "div", (HUD_LINK[readFail.kind] || readFail.kind || "") + (readFail.status ? " · " + readFail.status : "") + " · " + clock(readFail.at));
          await add(box, "div", "아래는 마지막으로 읽은 상태예요.", "opacity:.85");
        }
        for (const t of ui.lineTurns) {
          await add(panel, "div", t.turn + "턴 · " + (t === latest ? "지금" : "직전") + (t.off ? " · 사용 안 함(OOC)" : ""), "color:#7A776E;margin-top:4px");
          for (const [mark, title, detailText, lineTone] of hudLines(t)) {
            if (mark === "fail") {
              const g = (t.stages || []).find((x) => x.key === "generation") || {};
              ui.lines.push([await failBox(panel, t, title, detailText, g.detail, g.state), title]);
              continue;
            }
            const line = await add(panel, "div", null, "display:flex;gap:6px;align-items:flex-start");
            await add(line, "span", mark, "flex:none;width:12px;text-align:center;color:" + (MARK[mark] || "#7A776E"));
            const textCol = await add(line, "div", null, "display:flex;flex-direction:column;min-width:0");
            ui.lines.push([await add(textCol, "div", title, lineTone === "warn" ? "color:#B7791F" : ""), title]);
            if (detailText) await add(textCol, "div", detailText, "color:#7A776E;word-break:keep-all");
          }
        }
        // earlier turns' failures outside the latest two (review 19 B-04)
        for (const f of failures.slice(0, 3)) {
          if (f.state === "waiting" || f.state === "running") { await add(panel, "div", f.turn + "턴 · " + (f.state === "running" ? "다시 시도 중" + (f.attempts && f.attempt > 0 ? " · " + f.attempts + "번 중 " + f.attempt + "번째" : "") : "다시 시도 요청함"), "color:#2563EB;margin-top:4px"); continue; }
          await add(panel, "div", f.turn + "턴 · 이전", "color:#7A776E;margin-top:4px");
          await failBox(panel, f, "기억 만들기 실패", f.detail ? (WARNING_TEXT[f.detail] || f.detail) : "", f.detail, f.state);
        }
        if (failures.length > 3) await add(panel, "div", "그 밖에 " + (failures.length - 3) + "턴도 기억을 못 만들었어요", "color:#C53030");
        for (const n of (enabled && v.notices) || []) await add(panel, "div", n.turn + "턴 · " + (HUD_NOTICE[n.kind] || n.kind), "border-radius:8px;padding:6px 10px;background:rgba(183,121,31,.14)");
        // the memories themselves stay off the chat screen (user 10-28)
        await add(panel, "div", "넣은 기억 내용은 설정의 이번 턴 탭에서 봐요", "color:#7A776E;margin-top:2px");
      }
      if (ui.stopped || unloaded) { await remove(); return; }
      await body.appendChild(wrap);
      ui.body = body; ui.shown = shown;
    };
    const render = () => { ui.serial = ui.serial.then(draw).catch((e) => { recordPluginError(e); console.warn("[AC2] HUD draw", e); }); return ui.serial; };
    const poll = safe(async () => {
      cancel();
      if (ui.stopped || unloaded || !ui.chat) return;
      if (ui.asking) { ui.again = true; return; }
      ui.asking = true;
      const epoch = ui.epoch, chat = ui.chat;
      try {
        const r = await bridge("/hud?chat_id=" + encodeURIComponent(chat), { method: "GET" });
        if (epoch !== ui.epoch || ui.stopped || unloaded) return;
        if (r.ok) { ui.view = r.json; ui.readFail = null; await render(); }
        else {
          // a failed read is told; the last view is not shown as current
          // (review 19 B-07), and it is read again while a turn was under way
          // the same failure again keeps its first time: the panel is not
          // built again for every read (review 19 R2-C01)
          const was = ui.readFail;
          if (!was || was.chat !== chat || was.kind !== r.kind || was.status !== r.status) ui.readFail = { chat, kind: r.kind, status: r.status, at: new Date().toISOString() };
          await render();
        }
        const last = ui.view;
        if (!ui.stopped && !unloaded && last && last.settings && last.settings.enabled && (last.active || flushing)) ui.timer = setTimeout(poll, 2000);
      } finally {
        ui.asking = false;
        if (ui.again) { ui.again = false; poll(); }
      }
    });
    // a hook observed this chat (no host read of its own); a new chat opens
    // the chip again as the setting says
    hudSee = safe(async (id) => {
      if (id === null || id === undefined || id === "") return;
      cancel(); ui.epoch++; ui.dismissed = false;
      const chat = String(id);
      if (ui.chat !== chat) { ui.open = null; ui.view = null; await render(); }
      ui.chat = chat;
      await poll();
    });
    hudSettingsChanged = safe(async () => { ui.open = null; ui.denied = false; ui.dismissed = false; ui.shown = null; await poll(); });
    // a memory request that did not reach the backend is shown until one does
    // aux: the request was not the main model's (translation and the like);
    // the backend gives those no memory, but it cannot say so when it was
    // not reached, so the host's own request type tells it (review 19 R2-B05)
    hudLink = safe(async (id, res, aux) => {
      if (id === null || id === undefined || id === "") return;
      // an auxiliary request the backend passed over says nothing of the
      // memory request (review 19 B-06); each chat keeps its own
      if (res && res.ok && res.json && res.json.skipped) return;
      const chat = String(id);
      if (res && res.ok) delete ui.links[chat];
      else if (!ui.links[chat] || !aux) { ui.links[chat] = { chat, aux, kind: res && res.kind, status: res && res.status, at: new Date().toISOString() }; ui.dismissed = false; }
      await render();
    });
    cleanups.push(async () => {
      ui.stopped = true; ui.epoch++; cancel();
      hudSee = () => {}; hudSettingsChanged = () => {}; hudLink = () => {};
      await ui.serial; await remove();
    });
  }

  async function openSettings() {
    if (!document.getElementById("ac2-style")) {
      const st = document.createElement("style");
      st.id = "ac2-style";
      st.textContent = PANEL_CSS;
      document.head.append(st);
    }
    const tabs = [["chats", "채팅"], ["turn", "이번 턴"], ["status", "상태"], ["memories", "기억"], ["worldline", "세계선"], ["conn", "연결"], ["budget", "기억 예산"], ["models", "모델"], ["prep", "전처리"], ["update", "업데이트"]];
    const body = el("div", { class: "ac2-body" });
    const tabBar = el("div", { class: "ac2-tabs", role: "tablist" });
    writeBar = el("div", { class: "ac2-msg", role: "status", hidden: true }); // the write under way (oneWrite)
    if (writing) showWrite(WRITING_TEXT, true); // opened again while a write runs
    // the chat the memories and worldline tabs show, and a way back to the open chat
    const viewBar = el("div", { class: "ac2-viewbar" });
    const drawView = async () => {
      const v = await chatInView();
      viewBar.hidden = !["turn", "memories", "worldline"].includes(settingsTab);
      viewBar.replaceChildren(...nodes(
        el("span", { class: "ac2-label" }, "보는 채팅: " + (v.name || (v.id ? "채팅 " + v.id.slice(0, 8) : "없음"))),
        el("span", { class: "ac2-help" }, v.isOpen ? "현재 채팅" : "다른 채팅"),
        v.isOpen || !v.open ? null : el("button", { class: "ac2-btn", on: { click: () => { showChat(null); draw(); } } }, "현재 채팅 보기")));
    };
    const draw = async () => {
      tabBar.replaceChildren(...tabs.map(([k, label]) => el("button", { class: "ac2-tab", "aria-pressed": String(settingsTab === k), on: { click: () => { settingsTab = k; draw(); } } }, label)));
      // each draw fills its own pane, so a slow earlier tab's late answer
      // lands in its detached pane instead of the tab now shown
      const pane = el("div", { style: "display:contents" });
      body.replaceChildren(pane);
      await drawView();
      // a chat chosen in the chats tab opens its memories
      const choose = (chat) => { showChat(chat); settingsTab = "memories"; draw(); };
      await ({ chats: (b) => chatsTab(b, choose), turn: turnTab, status: statusTab, memories: memoriesTab, worldline: (b) => worldlineTab(b), conn: connTab, budget: budgetTab, models: modelsTab, prep: prepTab, update: updateTab }[settingsTab])(pane);
    };
    document.body.replaceChildren(el("div", { class: "ac2-wrap" }, el("div", { class: "ac2-card" },
      el("div", { class: "ac2-head" }, el("div", { class: "ac2-logo", "aria-hidden": "true" }, "AC"), el("div", { class: "ac2-title" }, "Archive Center 2 · 설정"),
        el("button", { class: "ac2-btn", on: { click: () => api.hideContainer() } }, "닫기")),
      tabBar, writeBar, viewBar, body)));
    // a failure told in a screen this opening replaced is told again here
    if (!writing && writeFailure && !failureHome.isConnected) showWrite(writeFailure, false);
    await api.showContainer("fullscreen");
    await draw();
  }

  await register("settings", () => api.registerSetting("Archive Center 2", openSettings, "", "none", "ac2-settings"), () => {});
  await register("chip", () => { refreshChip(); }, () => { if (chipTimer) clearTimeout(chipTimer); chipTimer = null; });
  // Turn HUD (U6): started here, stopped on unload by its own owner.
  hudStart();

  try { runtime = await api.getRuntimeInfo(); } catch (e) { recordPluginError(e); runtime = { error: String(e) }; }
  const health = await bridge("/version", { method: "GET" });
  const loadCoords = await coords();
  const loadPending = [];
  const loadChat = await chatSnapshot(loadCoords.character_index, loadCoords.chat_index, loadPending, await captureText());
  await record("load", { runtime, backend_version: health, coords: loadCoords, stored_chat: loadChat }, loadPending);
  hudSee(loadChat && loadChat.chat_id); // the chip for the chat open at load
})();
