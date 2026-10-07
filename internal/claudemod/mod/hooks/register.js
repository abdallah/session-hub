// The sessionhub mod: reports what the session is blocked on and its usage to sessionhub,
// delivers sessionhub messages into the session, shows the sessionhub inbox count, and
// opens the inbox in a pane (`/inbox`).
//
// Every action runs the sessionhub binary (`sessionhub mod ...`, and for the pane
// `sessionhub inbox`, `sessionhub approve`, `sessionhub deny`); the mod holds no token
// and makes no network calls of its own.

import { atom, read, update } from 'claude-code'
import { SESSIONHUB_BIN } from './config.js'
import { ACTIONS, EMPTY_PANE, clean, firstLine, paneTree, parseInbox } from './pane.jsx'

/** @typedef {import('claude-code').EngineInterface} Engine */
/** @typedef {import('claude-code').Timer} Timer */
/** @typedef {import('claude-code').ProcessRunInit} ProcessRunInit */
/** @typedef {import('./pane.jsx').ActionKind} ActionKind */

// Until `sessionhub install-mod` fills in the path, use `sessionhub` from PATH. The check
// avoids a second copy of the placeholder, which the installer would rewrite.
const HUB = SESSIONHUB_BIN.startsWith('__') ? 'sessionhub' : SESSIONHUB_BIN

const POLL_TIMEOUT_MS = 35_000
const CALL_TIMEOUT_MS = 10_000
const POLL_AGAIN_MS = 1_000
const POLL_BACKOFF_MS = 30_000
// An empty poll faster than this means sessionhub did not hold the long poll (for
// example, the server is unreachable), so the loop backs off instead of
// starting a process every second.
const POLL_MIN_EMPTY_MS = 5_000
const INBOX_EVERY_MS = 60_000
// How long a delivery waits for `$.prompt.submit` to settle. A submit made
// during a turn settles only once the session is idle; past this the message
// is queued in the engine, so it counts as delivered.
const SUBMIT_WAIT_MS = 3_000
// The store key of the inbox counts every session on the machine shares, as
// `{ counts, at }`; a session runs `sessionhub mod inbox-count` only once they are
// INBOX_EVERY_MS old.
const INBOX_KEY = 'inbox'
// The inbox pane: its id and title, how often it reads the inbox while open
// (and after a failed read), and how long a `sessionhub` call it makes may take.
// The CLI gives each server call 15 s; this leaves it room to report its own
// error. `/inbox` toggles it.
const PANE = 'sessionhub-inbox'
const PANE_TITLE = 'sessionhub inbox'
const PANE_EVERY_MS = 15_000
const PANE_BACKOFF_MS = 30_000
const PANE_CALL_TIMEOUT_MS = 20_000
// What the pane draws, held by the engine for the session, so a reload keeps it.
const paneState = atom({ plugin: 'sessionhub', key: 'pane' }, EMPTY_PANE)

// One loop of each kind per mod instance. `start` cancels the timers it holds
// and bumps the generation, so an iteration still in flight from an earlier
// start sees the change and does not reschedule itself.
let generation = 0
// Set by session.start: the loops run only in an interactive terminal
// session, since `$.process` is CLI only and the mod also loads in Desktop
// and VS Code.
let eligible = false
/** @type {Timer | undefined} */
let pollTimer
/** @type {Timer | undefined} */
let inboxTimer
// The pane's refresh loop has a generation of its own, so closing the pane
// stops it without touching the other loops. `paneOpen` is set while the
// pane is open, so an action that ends after a close starts no loop.
let paneGeneration = 0
let paneOpen = false
/** @type {Timer | undefined} */
let paneTimer
// The pane actions still running, by kind and target, so a second press of
// the same button waits for the first.
/** @type {Set<string>} */
const paneRunning = new Set()

/**
 * @param {Engine} $
 * @param {string[]} args
 * @param {string} [stdin]
 * @param {number} [timeoutMs]
 */
function sessionhub($, args, stdin, timeoutMs = CALL_TIMEOUT_MS) {
  /** @type {ProcessRunInit} */
  const init = { timeoutMs }
  if (stdin !== undefined) init.stdin = stdin
  return $.process.run([HUB, 'mod', ...args], init)
}

// Runs `sessionhub mod ...` and resolves its stdout; rejects on a non-zero exit.
/**
 * @param {Engine} $
 * @param {string[]} args
 * @param {string} [stdin]
 * @param {number} [timeoutMs]
 */
async function hubOut($, args, stdin, timeoutMs) {
  const r = await sessionhub($, args, stdin, timeoutMs)
  if (r.exitCode !== 0) {
    throw new Error(`sessionhub mod ${args[0]} exited ${r.exitCode}: ${r.stderr.trim()}`)
  }
  return r.stdout
}

function stop() {
  generation++
  pollTimer?.cancel()
  inboxTimer?.cancel()
  pollTimer = undefined
  inboxTimer = undefined
  paneOpen = false
  stopPane()
}

/** @param {Engine} $ */
function start($) {
  stop()
  const gen = generation
  pollTimer = $.clock.after(0, () => pollOnce($, gen))
  inboxTimer = $.clock.after(0, () => inboxOnce($, gen))
  // The inbox pane outlives a reload and a /clear; its refresh loop does not.
  resumePane($).catch(() => {})
}

function stopPane() {
  paneGeneration++
  paneTimer?.cancel()
  paneTimer = undefined
}

// Closes the pane and stops its refresh loop. The mod's own `$.ui.close`
// does not pass through its own `ui.close` hook, so it stops the loop here.
/** @param {Engine} $ */
async function closePane($) {
  paneOpen = false
  stopPane()
  await $.ui.close({ id: PANE })
}

// Starts the pane's refresh loop again when the engine still shows the pane
// after a reload or a /clear.
/** @param {Engine} $ */
async function resumePane($) {
  const gen = paneGeneration
  const isUp = (await $.ui.panes()).some((p) => p.id === PANE)
  if (!isUp || gen !== paneGeneration) return
  paneOpen = true
  paneTimer = $.clock.after(0, () => paneLoop($, gen))
}

/**
 * @param {Engine} $
 * @param {number} gen
 */
async function paneLoop($, gen) {
  if (gen !== paneGeneration) return
  const ok = await paneRefresh($)
  if (gen !== paneGeneration || !paneOpen) return
  paneTimer = $.clock.after(ok ? PANE_EVERY_MS : PANE_BACKOFF_MS, () => paneLoop($, gen))
}

// Reads the inbox now, and counts the next 15 seconds from here.
/** @param {Engine} $ */
async function paneRefreshNow($) {
  if (!paneOpen) return
  stopPane()
  await paneLoop($, paneGeneration)
}

/**
 * @param {Engine} $
 * @param {string[]} args
 */
function paneRun($, args) {
  return $.process.run([HUB, ...args], { timeoutMs: PANE_CALL_TIMEOUT_MS })
}

// Reads the inbox for the pane, and shares its counts with the status line
// and the other sessions' cache. Resolves false when the read failed; the
// pane then keeps the last items and shows the error.
/** @param {Engine} $ */
async function paneRefresh($) {
  try {
    const r = await paneRun($, ['inbox', '--json'])
    if (r.exitCode !== 0) throw new Error(firstLine(r.stderr) || `sessionhub inbox exited ${r.exitCode}`)
    const { items, counts } = parseInbox(r.stdout)
    const now = await $.clock.now()
    await update($, paneState, () => ({ items, counts, error: '', at: now }))
    await $.store.set(INBOX_KEY, { counts, at: now }).catch(() => {})
    $.ui.status(statusText(counts))
    return true
  } catch (err) {
    const error = clean(err instanceof Error ? err.message : String(err)) || 'unknown error'
    await update($, paneState, (s) => ({ ...s, error })).catch(() => {})
    return false
  }
}

/**
 * Runs the action the person pressed, toasts what happened, and reads the
 * inbox again.
 * @param {Engine} $
 * @param {ActionKind} kind
 * @param {string} target a request ID for allow and deny, a session ID for dismiss
 */
async function paneAct($, kind, target) {
  const key = `${kind}:${target}`
  if (paneRunning.has(key)) return
  paneRunning.add(key)
  const { label, args } = ACTIONS[kind]
  try {
    const r = await paneRun($, args(target))
    if (r.exitCode === 0) $.ui.toast(`sessionhub: ${firstLine(r.stdout) || `${label} sent`}`)
    else $.ui.toast(`sessionhub: ${label} failed: ${firstLine(r.stderr) || `exit ${r.exitCode}`}`)
  } catch (err) {
    $.ui.toast(`sessionhub: ${label} failed: ${clean(err instanceof Error ? err.message : String(err))}`)
  } finally {
    paneRunning.delete(key)
  }
  await paneRefreshNow($)
}

/**
 * @param {Engine} $
 * @param {number} gen
 */
async function pollOnce($, gen) {
  if (gen !== generation) return
  let delay = POLL_AGAIN_MS
  try {
    const sessionId = await $.session.id()
    const began = await $.clock.now()
    const out = (await hubOut($, ['poll', '--session', sessionId], undefined, POLL_TIMEOUT_MS)).trim()
    if (out === '') {
      if ((await $.clock.now()) - began < POLL_MIN_EMPTY_MS) delay = POLL_BACKOFF_MS
    } else {
      await deliver($, JSON.parse(out))
    }
  } catch {
    delay = POLL_BACKOFF_MS
  }
  if (gen !== generation) return
  pollTimer = $.clock.after(delay, () => pollOnce($, gen))
}

/**
 * @param {Engine} $
 * @param {{ id: string | number, text: string }} claim
 */
async function deliver($, claim) {
  /** @type {Promise<{ drop?: string } | { error: string }>} */
  const submitted = $.prompt.submit({ text: claim.text }).then(
    (r) => (r && typeof r.drop === 'string' ? { drop: r.drop } : {}),
    (err) => ({ error: err instanceof Error ? err.message : String(err) }),
  )
  /** @type {Timer | undefined} */
  let timer
  /** @type {Promise<undefined>} */
  const timedOut = new Promise((resolve) => {
    timer = $.clock.after(SUBMIT_WAIT_MS, () => resolve(undefined))
  })
  const outcome = await Promise.race([submitted, timedOut])
  timer?.cancel()

  let state = 'delivered'
  let detail = ''
  if (outcome === undefined) {
    // Queued behind the running turn: report it delivered now, and log a
    // later refusal, since sessionhub has already been told.
    submitted
      .then((late) => {
        if ('drop' in late) $.ui.log(`sessionhub: message ${claim.id} was dropped: ${late.drop}`)
        if ('error' in late) $.ui.log(`sessionhub: message ${claim.id} failed: ${late.error}`)
      })
      .catch(() => {})
  } else if ('drop' in outcome) {
    state = 'refused'
    detail = String(outcome.drop)
  } else if ('error' in outcome) {
    state = 'busy'
    detail = outcome.error
  }
  const args = ['result', '--message', String(claim.id), '--state', state]
  if (detail !== '') args.push('--detail', detail)
  try {
    await hubOut($, args)
  } catch {
    // The server offers an unreported claim again; nothing more to do here.
  }
}

/**
 * @param {Engine} $
 * @param {number} gen
 */
async function inboxOnce($, gen) {
  if (gen !== generation) return
  let text
  try {
    text = statusText(await inboxCounts($))
  } catch {
    // Clear the line rather than keep counts that may be stale.
    text = undefined
  }
  if (gen !== generation) return
  $.ui.status(text)
  inboxTimer = $.clock.after(INBOX_EVERY_MS, () => inboxOnce($, gen))
}

// Resolves the inbox counts: the shared cache while it is fresh, else a new
// `sessionhub mod inbox-count`, written back to the cache.
/** @param {Engine} $ */
async function inboxCounts($) {
  const now = await $.clock.now()
  const cached = /** @type {{ counts?: unknown, at?: unknown } | undefined} */ (
    await $.store.get(INBOX_KEY).catch(() => undefined)
  )
  if (cached && typeof cached.at === 'number' && cached.counts && now - cached.at < INBOX_EVERY_MS && now >= cached.at) {
    return /** @type {Record<string, unknown>} */ (cached.counts)
  }
  const counts = JSON.parse(await hubOut($, ['inbox-count']))
  await $.store.set(INBOX_KEY, { counts, at: now }).catch(() => {})
  return counts
}

// Formats the inbox counts as the status line, or undefined when all are 0.
/**
 * @param {Record<string, unknown> | null | undefined} counts
 * @returns {string | undefined}
 */
export function statusText(counts) {
  const parts = []
  for (const kind of ['blocked', 'waiting', 'finished']) {
    const n = Number(counts?.[kind] ?? 0)
    if (n > 0) parts.push(`${n} ${kind}`)
  }
  return parts.length === 0 ? undefined : parts.join(' · ')
}

// A failed gating hook must never stop the event: pass it on (replay-safe, so
// a hook that already called next gets that result back).
/** @type {(...args: any[]) => any} */
const passThrough = ($, e, next) => next(e)

/** @param {import('claude-code').On} on */
export function register(on) {
  // `/inbox` opens the pane, or closes it when it is open.
  on('command.run', { command: 'inbox' }, async ($) => {
    if ((await $.ui.panes()).some((p) => p.id === PANE)) {
      await closePane($)
      return { text: 'Closed the sessionhub inbox.' }
    }
    await $.ui.open({ id: PANE, title: PANE_TITLE, focus: true })
    paneOpen = true
    stopPane()
    const gen = paneGeneration
    paneTimer = $.clock.after(0, () => paneLoop($, gen))
    return { text: 'Opened the sessionhub inbox.' }
  }).catch(() => ({ text: 'The sessionhub inbox pane failed to open or close.' }))

  // A close by anyone else (the person's close mark or key, an unload) stops
  // the pane's refresh loop.
  on('ui.close', { id: PANE }, async ($, e, next) => {
    paneOpen = false
    stopPane()
    return next(e)
  }).catch(passThrough)

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const state = await read($, paneState)
    return paneTree($.ui.resolve(e), {
      state,
      now: await $.clock.now(),
      cols: e.props.bodyColumns,
      onAction: (kind, target) => paneAct($, kind, target),
      onRefresh: () => paneRefreshNow($),
      onClose: () => closePane($),
    })
  })

  on('session.start', async ($, e, next) => {
    eligible = e.surface === 'terminal' && e.isInteractive
    if (eligible) {
      start($)
      try {
        await $.command.register({ name: 'inbox', description: 'Show or hide the sessionhub inbox pane' })
      } catch {
        // The session goes on without /inbox.
      }
    }
    return next(e)
  })

  // `/clear` and `/resume` end the session and go on under another id with no
  // session.start, so start the loops again when the next session starts.
  on('classic.SessionStart', { source: ['clear', 'resume'] }, async ($, e, next) => {
    if (eligible) start($)
    return next(e)
  }).catch(passThrough)

  on('session.end', async ($, e, next) => {
    stop()
    return next(e)
  })

  on('tool.call', { tool: 'AskUserQuestion' }, async ($, e, next) => {
    let sessionId = ''
    try {
      sessionId = await $.session.id()
    } catch {
      return next(e)
    }
    // Neither call is awaited, so sessionhub never delays the question or its
    // answer. The clear is chained after the post, so the two stay in order.
    // The clear carries the same questions: sessionhub clears only that question,
    // so a late clear never ends a newer one.
    const posted = sessionhub($, ['blocked-on', '--session', sessionId], JSON.stringify({ questions: e.questions }))
      .catch(() => {})
    try {
      return await next(e)
    } finally {
      posted
        .then(() =>
          sessionhub($, ['blocked-on', '--session', sessionId], JSON.stringify({ questions: e.questions, clear: true })),
        )
        .catch(() => {})
    }
  }).catch(passThrough)

  on('session.measure', async ($, e, next) => {
    if (e.changed.includes('context') || e.changed.includes('cost')) {
      // Fire and forget: the report never holds up the session.
      $.session.id()
        .then((sessionId) =>
          sessionhub($, ['usage', '--session', sessionId], JSON.stringify({ context: e.context, cost: e.cost })),
        )
        .catch(() => {})
    }
    return next(e)
  })
}
