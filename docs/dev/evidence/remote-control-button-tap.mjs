// Taps a session's Remote Control button on the real dashboard in headless
// Chrome and prints what the card says before and after, with Remote
// Control session IDs masked and session UUIDs cut to 8 characters. The
// screenshot shows only that card, masked the same way. With --look it
// taps nothing: it prints the card and takes the screenshot.
//
//   SESSIONHUB_READ_TOKEN=... HEADLESS_SHELL=/path/to/chrome-headless-shell \
//     node docs/dev/evidence/remote-control-button-tap.mjs [--look] '<card title>' <screenshot.png>
//
// The token is read from the environment and never printed.
import { spawn } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const args = process.argv.slice(2);
const look = args[0] === "--look";
const [title, shot] = look ? args.slice(1) : args;
const token = process.env.SESSIONHUB_READ_TOKEN;
const shell = process.env.HEADLESS_SHELL;
const base = process.env.SESSIONHUB_URL || "https://sessionhub.example.com";
if (!title || !shot || !token || !shell) {
  console.error("usage: SESSIONHUB_READ_TOKEN=... HEADLESS_SHELL=... node remote-control-button-tap.mjs [--look] '<card title>' <screenshot.png>");
  process.exit(2);
}
const mask = (s) => s
  .replace(/session_[A-Za-z0-9_-]+/g, "session_<masked>")
  .replace(/\b([0-9a-f]{8})-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/g, "$1…");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const port = 9333;
const profile = mkdtempSync(join(tmpdir(), "sessionhub-tap-"));
const chrome = spawn(shell, [`--remote-debugging-port=${port}`, `--user-data-dir=${profile}`, "--window-size=360,1600", "about:blank"], { stdio: "ignore" });
try {
  let pages = [];
  for (let i = 0; i < 100 && !pages.length; i++) {
    try {
      pages = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).filter((t) => t.type === "page");
    } catch {
      await sleep(100);
    }
  }
  const ws = new WebSocket(pages[0].webSocketDebuggerUrl);
  await new Promise((r) => ws.addEventListener("open", r, { once: true }));
  let seq = 0;
  const waiting = new Map();
  ws.addEventListener("message", (m) => {
    const d = JSON.parse(m.data);
    const f = waiting.get(d.id);
    if (f) { waiting.delete(d.id); f(d); }
  });
  const send = (method, params = {}) => new Promise((r) => { const id = ++seq; waiting.set(id, r); ws.send(JSON.stringify({ id, method, params })); });
  const evaluate = async (expr) => (await send("Runtime.evaluate", { expression: expr, returnByValue: true })).result.result.value;
  const card = `[...document.querySelectorAll("article.card")].find((c) => c.querySelector(".title").textContent === ${JSON.stringify(title)})`;
  const cardText = `(() => { document.querySelectorAll("details.ended").forEach((d) => { d.open = true; }); const c = ${card}; return c ? c.textContent : ""; })()`;

  await send("Page.navigate", { url: `${base}/?token=${encodeURIComponent(token)}` });
  let before = "";
  for (let i = 0; i < 50 && !before; i++) { await sleep(200); before = await evaluate(cardText); }
  if (!before) throw new Error(`no card titled ${title}`);
  console.log(look ? "card:" : "before:", mask(before));
  if (!look) console.log(await evaluate(`(() => { const c = ${card};
    const b = [...c.querySelectorAll("button")].find((x) => /Remote Control/.test(x.textContent));
    if (!b) return "no Remote Control button";
    if (b.disabled) return "button disabled";
    b.click();
    return "tapped: " + b.textContent; })()`));
  if (!look) {
    let after = "";
    for (let i = 0; i < 130; i++) {
      await sleep(1000);
      after = await evaluate(cardText);
      if (!after.includes("Sending…")) break;
    }
    console.log("after:", mask(after));
  }
  // Mask every text node for the screenshot: links, and UUIDs in resume commands.
  await evaluate(`(() => {
    const w = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = w.nextNode(); n; n = w.nextNode()) {
      n.nodeValue = n.nodeValue
        .replace(/session_[A-Za-z0-9_-]+/g, "session_<masked>")
        .replace(/\\b([0-9a-f]{8})-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\\b/g, "$1…");
    }
  })()`);
  // Show only this card, at the top of the page, so the clip is exact.
  const box = await evaluate(`(() => { const c = (${card}).cloneNode(true);
    document.body.replaceChildren(c); window.scrollTo(0, 0);
    const r = c.getBoundingClientRect();
    return { x: 0, y: 0, width: Math.ceil(r.right + 8), height: Math.ceil(r.bottom + 8) }; })()`);
  const png = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false, clip: { ...box, scale: 1 } });
  writeFileSync(shot, Buffer.from(png.result.data, "base64"));
} finally {
  chrome.kill();
}
