// Screenshots the dashboard's Tasks tab on a scratch sessionhub seeded by
// tasks-seed.mjs: today, the day before, and the review strip, at desktop
// and phone width. Then it merges a proposal and makes a task from a
// session through the page, and screenshots the result.
//
//   SESSIONHUB_URL=http://localhost:8799 TOWER_TOKEN=... CHROME=/usr/bin/google-chrome \
//     node docs/dev/evidence/tasks-shots.mjs <out-dir>
//
// The machine token signs the browser in through a one-time login link. It
// is read from the environment and never printed.
import { spawn } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const out = process.argv[2];
const base = process.env.SESSIONHUB_URL;
const token = process.env.TOWER_TOKEN;
const chromeBin = process.env.CHROME || "/usr/bin/google-chrome";
if (!out || !base || !token) {
  console.error("usage: SESSIONHUB_URL=... TOWER_TOKEN=... node tasks-shots.mjs <out-dir>");
  process.exit(2);
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Sign in: a login link from the machine token, redeemed with the Origin
// the server expects. The cookie value goes straight into Chrome.
const login = await (await fetch(base + "/v1/logins", {
  method: "POST",
  headers: { "Authorization": "Bearer " + token, "Content-Type": "application/json" },
  body: JSON.stringify({ name: "evidence" }),
})).json();
const code = login.url.split("/login/")[1];
const redeem = await fetch(base + "/login/" + code, { method: "POST", headers: { "Origin": base }, redirect: "manual" });
const cookie = /__Host-hub_session=([^;]+)/.exec(redeem.headers.get("set-cookie") || "");
if (!cookie) throw new Error("login failed: " + redeem.status);

const port = 9334;
const profile = mkdtempSync(join(tmpdir(), "sessionhub-tasks-"));
const chrome = spawn(chromeBin, ["--headless=new", "--disable-gpu", "--no-first-run", `--remote-debugging-port=${port}`,
  `--user-data-dir=${profile}`, "about:blank"], { stdio: "ignore" });
try {
  let pages = [];
  for (let i = 0; i < 300 && !pages.length; i++) {
    try {
      pages = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).filter((t) => t.type === "page");
    } catch {
      // Chrome is not listening yet.
    }
    if (!pages.length) await sleep(100);
  }
  if (!pages.length) throw new Error("Chrome did not open a page");
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
  const evaluate = async (expr) => (await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true })).result.result.value;
  const until = async (expr, what) => {
    for (let i = 0; i < 50; i++) { if (await evaluate(expr)) return; await sleep(100); }
    throw new Error("timed out waiting for " + what);
  };
  const size = (width, mobile) => send("Emulation.setDeviceMetricsOverride", { width, height: 900, deviceScaleFactor: 1, mobile });
  const shot = async (name, selector) => {
    await sleep(300);
    let clip;
    if (selector) {
      const r = await evaluate(`(() => { const b = document.querySelector(${JSON.stringify(selector)}).getBoundingClientRect();
        return { x: b.x + scrollX, y: b.y + scrollY, width: b.width, height: b.height }; })()`);
      clip = { ...r, x: Math.max(0, r.x - 8), y: Math.max(0, r.y - 8), width: r.width + 16, height: r.height + 16, scale: 1 };
    }
    const res = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true, ...(clip ? { clip } : {}) });
    writeFileSync(join(out, name), Buffer.from(res.result.data, "base64"));
    console.log("wrote", name);
  };
  const click = (selector) => evaluate(`document.querySelector(${JSON.stringify(selector)}).click()`);
  const clickText = (text) => evaluate(`[...document.querySelectorAll("#tasks button")].find((b) => b.textContent === ${JSON.stringify(text)}).click()`);
  const boardDate = () => evaluate(`document.getElementById("task-date").value`);
  const columnsLoaded = `!!document.querySelector("#task-board .task-cols")`;

  await send("Network.enable");
  await send("Network.setCookie", { name: "__Host-hub_session", value: cookie[1], url: base + "/", secure: true, httpOnly: true, sameSite: "Strict" });
  await size(1280, false);
  await send("Page.navigate", { url: base + "/#tasks" });
  await until(columnsLoaded + ` && !!document.querySelector("#task-review .review")`, "today's columns");
  const today = await boardDate();
  console.log("today:", today, "tz:", await evaluate("Intl.DateTimeFormat().resolvedOptions().timeZone"));
  // Open one card's done items and its state menu, so the shot shows both.
  await evaluate(`document.querySelector("#task-board .col-done details:not(.snooze)").open = true`);
  await shot("tasks-today-desktop.png");
  await shot("tasks-review-desktop.png", "#task-review .review");

  await click("#task-prev");
  await until(`document.getElementById("task-date").value !== ${JSON.stringify(today)} && ` + columnsLoaded, "the day before");
  console.log("past:", await boardDate(), "menus:", await evaluate(`document.querySelectorAll("#task-board details.snooze").length`));
  await shot("tasks-past-desktop.png");

  await size(390, true);
  await shot("tasks-past-phone.png");
  await click("#task-today");
  await until(`document.getElementById("task-date").value === ${JSON.stringify(today)} && ` + columnsLoaded, "today again");
  await evaluate(`document.querySelector("#task-board .col-in_progress details.snooze").open = true`);
  await shot("tasks-today-phone.png");
  await shot("tasks-review-phone.png", "#task-review .review");

  // Writes through the page: merge the duplicate proposal into the open
  // task, then make a task from an untasked session.
  await size(1280, false);
  await evaluate(`(() => {
    const item = [...document.querySelectorAll(".review-item")].find((x) => x.querySelector(".t-title").textContent === "Write the tasks docs");
    const sel = item.querySelector("select");
    sel.value = [...sel.options].find((o) => o.textContent === "Write the tasks docs").value;
    sel.dispatchEvent(new Event("change"));
    [...item.querySelectorAll("button")].find((b) => b.textContent === "Merge").click();
  })()`);
  await until(`![...document.querySelectorAll(".review-item .t-title")].some((x) => x.textContent === "Write the tasks docs")`, "the merge");
  await clickText("Make task");
  await until(`[...document.querySelectorAll("#task-board .col-todo .t-title")].length === 2`, "the new task in Todo");
  console.log("tab:", await evaluate(`document.getElementById("tab-tasks").textContent`));
  await shot("tasks-after-writes-desktop.png");
} finally {
  chrome.kill();
  await sleep(300);
  rmSync(profile, { recursive: true, force: true });
}
