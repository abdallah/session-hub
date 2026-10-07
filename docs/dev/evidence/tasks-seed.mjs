// Seeds a scratch sessionhub with two days of tasks for the Tasks tab
// screenshots. Yesterday's writes go through the HTTP API, then sqlite3
// moves their task events and reports back 24 hours; today's writes follow.
// Run it against a scratch database only: it rewrites timestamps in place.
//
//   SESSIONHUB_URL=http://localhost:8799 SESSIONHUB_DB=/tmp/x/sessionhub.db \
//   TOWER_TOKEN=... BLUEBOX_TOKEN=... node docs/dev/evidence/tasks-seed.mjs
//
// Tokens are read from the environment and never printed.
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";

const base = process.env.SESSIONHUB_URL;
const db = process.env.SESSIONHUB_DB;
const tokens = { tower: process.env.TOWER_TOKEN, bluebox: process.env.BLUEBOX_TOKEN };
if (!base || !db || !tokens.tower || !tokens.bluebox) {
  console.error("usage: SESSIONHUB_URL=... SESSIONHUB_DB=... TOWER_TOKEN=... BLUEBOX_TOKEN=... node tasks-seed.mjs");
  process.exit(2);
}

async function call(machine, method, path, body) {
  const resp = await fetch(base + path, {
    method,
    headers: { "Authorization": "Bearer " + tokens[machine], "Content-Type": "application/json", "X-Hub-Action": "tasks" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await resp.text();
  if (!resp.ok) throw new Error(`${method} ${path}: ${resp.status} ${text}`);
  return text ? JSON.parse(text) : null;
}

async function session(machine, title, prompt) {
  const id = randomUUID();
  await call(machine, "POST", "/v1/sessions", { id, agent: "claude", source: "hooks", cwd: "~/Code/ops" });
  await call(machine, "POST", `/v1/sessions/${id}/title`, { title });
  if (prompt) await call(machine, "POST", `/v1/sessions/${id}/events`, { kind: "prompt", source: "hooks", payload: { prompt } });
  return { id, machine };
}

const report = (s, done) => call(s.machine, "POST", `/v1/sessions/${s.id}/report`, { done, in_flight: [], waiting_on: [] });
const create = (machine, body) => call(machine, "POST", "/v1/tasks", body);
const move = (machine, id, to, extra = {}) => call(machine, "POST", `/v1/tasks/${id}/state`, { to, ...extra });
const link = (s, id) => call(s.machine, "POST", `/v1/tasks/${id}/sessions`, { session_id: s.id });

// Yesterday.
const sToken = await session("tower", "Rotate the CI deploy token");
const sPg = await session("bluebox", "Upgrade Postgres on staging");
const sCert = await session("tower", "Renew the wildcard certificate");
const sAlerts = await session("bluebox", "Move alert rules to Terraform");

const tToken = await create("tower", { title: "Rotate the CI deploy token", ref: "OPS-1234", ref_url: "https://tickets.example/issue/OPS-1234", source: "ticket", state: "in_progress" });
await link(sToken, tToken.id);
await report(sToken, ["Rotated the token in GitLab", "Updated the runner secret", "Ran the deploy pipeline green"]);
await move("tower", tToken.id, "done");

const tReply = await create("tower", { title: "Reply to a teammate about the backup window", source: "email" });
const tPg = await create("bluebox", { title: "Upgrade Postgres on staging", ref: "OPS-412", ref_url: "https://tickets.example/issue/OPS-412", source: "ticket", state: "in_progress" });
await link(sPg, tPg.id);
await report(sPg, ["Took a snapshot of staging-db"]);
await create("tower", { title: "Write the tasks docs", source: "other" });
const tFlaky = await create("bluebox", { title: "Fix the flaky login test", ref: "OPS-1301", source: "ticket", state: "in_progress" });
await move("bluebox", tFlaky.id, "done");

const shift = (table) => `UPDATE ${table} SET ts = strftime('%Y-%m-%dT%H:%M:%S', ts, '-1 day') || substr(ts, 20);`;
execFileSync("sqlite3", [db, ".timeout 5000",
  shift("task_events"), shift("reports"),
  "UPDATE tasks SET created_at = strftime('%Y-%m-%dT%H:%M:%S', created_at, '-1 day') || substr(created_at, 20), updated_at = strftime('%Y-%m-%dT%H:%M:%S', updated_at, '-1 day') || substr(updated_at, 20);"]);

// Today.
await move("tower", tReply.id, "in_progress");
await move("bluebox", tFlaky.id, "in_progress"); // reopened: yesterday's Done still shows it
const tCert = await create("tower", { title: "Renew the wildcard certificate", source: "chat", state: "in_progress" });
await link(sCert, tCert.id);
await report(sCert, ["Issued the new certificate", "Reloaded nginx on both proxies"]);
await move("tower", tCert.id, "done");
await report(sPg, ["Ran pg_upgrade on staging", "Checked replication lag"]);
await move("bluebox", tPg.id, "done_proposed", { session_id: sPg.id, note: "staging is on 16" });
await create("bluebox", { title: "Move alert rules to Terraform", ref: "OPS-1302", source: "ticket", session_id: sAlerts.id });
await create("tower", { title: "Write the tasks docs", source: "other", session_id: sToken.id }); // agent proposal of an existing task title
await session("tower", "Investigate slow dashboard load", "the dashboard takes 4s to load, find out why");
await session("bluebox", "Answer the Hetzner invoice question", "check the October Hetzner invoice");
console.log("seeded");
