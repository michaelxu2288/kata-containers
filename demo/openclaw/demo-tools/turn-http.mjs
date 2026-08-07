// resident turn server: plain http on :18888 -> loopback gateway ws -> reply
// baked into the source pre-snapshot; every clone wakes with it listening.
// AUTO-FIRE: if /tmp/auto-fire exists (armed before snapshot #2), a restored
// clone detects its ip change, names itself after a US city, and posts its
// fruit take to the chatroom - with no human prompting it.
import http from "node:http";
import { networkInterfaces } from "node:os";
import { existsSync } from "node:fs";

const TOKEN = process.env.OPENCLAW_GATEWAY_TOKEN || "dummy-token-for-sandbox";
const FRUIT = process.env.FRUIT_STAND_URL || "http://fruit-stand.default.svc.cluster.local";

function turn(message, sessionId) {
  return new Promise((resolve, reject) => {
    const t0 = performance.now();
    const ws = new WebSocket("ws://127.0.0.1:18789", { headers: { Origin: "http://127.0.0.1:18789" } });
    const send = (id, method, params) => ws.send(JSON.stringify({ type: "req", id, method, params }));
    let reqId = null;
    const timer = setTimeout(() => { try { ws.close(); } catch {} reject(new Error("timeout")); }, 120000);
    ws.onerror = () => { clearTimeout(timer); reject(new Error("ws-error")); };
    ws.onmessage = (ev) => {
      let f; try { f = JSON.parse(ev.data); } catch { return; }
      if (f.type === "event" && f.event === "connect.challenge") {
        send("c1", "connect", {
          minProtocol: 3, maxProtocol: 3,
          client: { id: "openclaw-control-ui", version: "2026.3.23", platform: "linux", mode: "webchat", instanceId: "ts-" + Math.random().toString(36).slice(2) },
          caps: [], auth: { token: TOKEN }, role: "operator",
          scopes: ["operator.admin","operator.read","operator.write","operator.approvals","operator.pairing"]
        });
        return;
      }
      if (f.type === "res" && f.id === "c1") {
        if (!f.ok) { clearTimeout(timer); reject(new Error("connect-failed " + JSON.stringify(f.error))); return; }
        reqId = "a-" + Date.now() + "-" + Math.random().toString(36).slice(2);
        send(reqId, "agent", { message, sessionId, deliver: false, timeout: 120, idempotencyKey: reqId });
        return;
      }
      if (f.type === "res" && f.id === reqId) {
        if (f.payload && f.payload.status === "accepted") return; // ack; final follows
        clearTimeout(timer);
        try { ws.close(); } catch {}
        if (!f.ok) { reject(new Error(JSON.stringify(f.error))); return; }
        const texts = ((f.payload?.result?.payloads) || []).map((p) => p.text).filter(Boolean);
        resolve({ reply: texts.join("\n") || f.payload?.summary || null, srvMs: +(performance.now() - t0).toFixed(1) });
      }
    };
  });
}

http.createServer(async (req, res) => {
  const u = new URL(req.url, "http://x");
  if (u.pathname === "/healthz") { res.end("ok\n"); return; }
  if (u.pathname !== "/turn") { res.statusCode = 404; res.end("not found\n"); return; }
  let body = "";
  for await (const c of req) body += c;
  // demo turns must be single-round and snappy: no tools, short answers.
  // raw=1 skips the no-tools leash for agentic turns (e.g. "go post this yourself")
  const raw = u.searchParams.get("raw") === "1";
  const message = (u.searchParams.get("m") || body || "Reply with just OK") +
    (raw ? "" : " (Answer in one or two short sentences. Do not use tools.)");
  const sessionId = u.searchParams.get("s") || "turnsrv";
  try {
    const out = await turn(message, sessionId);
    if (u.searchParams.get("post") === "1" && out.reply) {
      const user = u.searchParams.get("name") || "agent";
      try {
        const r = await fetch(FRUIT + "/api/messages", {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ user, message: out.reply }),
          signal: AbortSignal.timeout(10000),
        });
        out.posted = r.ok;
      } catch { out.posted = false; }
    }
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(out) + "\n");
  } catch (e) {
    res.statusCode = 500;
    res.end(JSON.stringify({ error: String(e.message || e) }) + "\n");
  }
}).listen(18888, "0.0.0.0", () => console.log("turn-http listening :18888"));

// ---- auto-fire on restore ----
// the original's ip never changes; a restored clone gets a NEW ip during the
// restore's network re-identity. that ip flip is the wake-up signal.
function eth0() {
  const addrs = networkInterfaces()["eth0"] || [];
  for (const a of addrs) if (a.family === "IPv4") return a.address;
  return null;
}
const bootIP = eth0();
let fired = false;
setInterval(async () => {
  if (fired || !existsSync("/tmp/auto-fire")) return;
  const cur = eth0();
  if (!cur || !bootIP || cur === bootIP) return;
  fired = true;
  try {
    const octet = cur.split(".")[3];
    const name = `Agent-${octet} ${cur}`;
    const out = await turn(`Your name is Agent-${octet}. In 2 sentences, say which fruit is the best and why. Give a confident opinion. No disclaimers. Do not use any tools.`, "demo-main");
    await fetch(FRUIT + "/api/messages", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ user: name, message: out.reply }),
      signal: AbortSignal.timeout(10000),
    });
    console.log("auto-fired as", name);
  } catch (e) { console.log("auto-fire failed:", String(e)); }
}, 500);
