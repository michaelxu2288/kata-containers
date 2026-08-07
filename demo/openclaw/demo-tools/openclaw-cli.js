#!/usr/bin/env node
// tiny in-pod cli: openclaw-cli --ask "question" [--post <name>] [--do]
// talks to this pod's own resident turn server over loopback;
// --post also publishes the reply to the fruit-stand chatroom as <name>
// --do lets the agent use its tools (default turns are leashed to no-tools)
const argv = process.argv.slice(2);
const di = argv.indexOf("--do");
const agentic = di >= 0; if (agentic) argv.splice(di, 1);
const pi = argv.indexOf("--post");
let postName = pi >= 0 ? argv.splice(pi, 2)[1] : null;
if (pi >= 0 && !postName) {
  // fallback if the name expanded empty: derive from this pod's ip
  const addrs = require("node:os").networkInterfaces()["eth0"] || [];
  const ip = (addrs.find((a) => a.family === "IPv4") || {}).address || "?";
  postName = `Agent-${ip.split(".")[3]} ${ip}`;
}
const i = argv.indexOf("--ask");
if (i < 0 || !argv[i + 1]) {
  console.error('usage: openclaw-cli --ask "question" [--post <name>]');
  process.exit(1);
}
const q = argv.slice(i + 1).join(" ");
const url = "http://127.0.0.1:18888/turn?s=demo-main" +
  (postName ? `&post=1&name=${encodeURIComponent(postName)}` : "") +
  (agentic ? "&raw=1" : "");
const t0 = Date.now();
fetch(url, { method: "POST", body: q })
  .then((r) => r.json())
  .then((j) => {
    console.log(j.reply || JSON.stringify(j));
    console.log(`(${((Date.now() - t0) / 1000).toFixed(3)}s)`);
  })
  .catch((e) => { console.error("error:", e.message); process.exit(1); });
