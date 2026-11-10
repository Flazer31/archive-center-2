//@name ac2_test_provider
//@display-name AC2 test provider (test only)
//@api 3.0
//@version 0.0.2
//@arg mode string json | stream | fail-once | slow-stream (default json)

// Test-only fake model registered through the official addProvider API.
// It makes no network calls, so hook timing can be recorded on web,
// desktop and PocketRisu as well as on a node server.
(async () => {
  const api = Risuai;
  let n = 0;
  const failed = new Set();

  async function mode() {
    try { return String((await api.getArgument("mode")) || "json"); } catch (_) { return "json"; }
  }

  function chunks(text, size) {
    const out = [];
    const chars = Array.from(text);
    for (let i = 0; i < chars.length; i += size) out.push(chars.slice(i, i + size).join(""));
    return out;
  }

  await api.addProvider("AC2 test provider", async (args, abortSignal) => {
    n += 1;
    const m = await mode();
    const last = (args.prompt_chat || []).slice(-1)[0];
    const text = `테스트 응답 ${n}번입니다. (test reply ${n}, mode ${m}, messages ${(args.prompt_chat || []).length})`;
    console.log("[AC2 test provider] call", n, m, last && last.role);

    if (m === "fail-once") {
      const key = JSON.stringify(args.prompt_chat || []);
      if (!failed.has(key)) {
        failed.add(key);
        return { success: false, content: "AC2 test provider: planned failure" };
      }
    }
    if (m === "stream" || m === "slow-stream") {
      const delay = m === "slow-stream" ? 600 : 120;
      const parts = chunks(text, 4);
      const stream = new ReadableStream({
        async start(controller) {
          for (const p of parts) {
            if (abortSignal && abortSignal.aborted) { controller.close(); return; }
            await new Promise((r) => setTimeout(r, delay));
            controller.enqueue(p);
          }
          controller.close();
        },
      });
      return { success: true, content: stream };
    }
    return { success: true, content: text };
  });
})();
