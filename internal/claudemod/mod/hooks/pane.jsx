// The sessionhub inbox pane's pure half: parsing `sessionhub inbox --json`,
// cleaning what the server sent, the argv of each action, and the tree the
// pane draws. register.js holds everything that calls `$` (the engine
// follows `$` only within one file).
//
// Every string from the server is untrusted: parseInbox cleans it of control
// and bidirectional characters, and the tree draws it only as Text.

/** @typedef {import('../types').PaneItem} PaneItem */
/** @typedef {import('../types').PanePermission} PanePermission */
/** @typedef {import('../types').PaneCounts} PaneCounts */
/** @typedef {import('../types').InboxPaneState} InboxPaneState */

// The most characters kept of any one server string, and the waiting_on
// lines drawn per item.
const MAX_TEXT = 500
const MAX_WAITING_LINES = 3

/** @type {readonly (readonly ['blocked' | 'waiting' | 'finished', string])[]} */
const GROUPS = [
  ['blocked', 'Blocked'],
  ['waiting', 'Waiting'],
  ['finished', 'Finished'],
]

// The same rules as the server's: a session ID, and `pr_` plus 22 base64url
// characters for a permission request. Both also keep an argv element from
// starting with `-`.
const SESSION_ID_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/
const REQUEST_ID_RE = /^pr_[A-Za-z0-9_-]{22}$/
// `sessionhub inbox dismiss` takes an ID or prefix of at least 4 characters.
const MIN_DISMISS_ID = 4
// An RFC 3339 time as Go writes it, for `inbox dismiss --since`.
const SINCE_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$/

// The tool input fields that say what a tool will do, in the order
// api.PermissionInputText tries them.
const INPUT_FIELDS = ['command', 'file_path', 'notebook_path', 'path', 'url', 'pattern', 'query', 'prompt']

/** @type {InboxPaneState} */
export const EMPTY_PANE = { items: [], counts: { blocked: 0, waiting: 0, finished: 0 }, error: '', at: 0 }

/**
 * The actions a Button runs: the argv after the binary, and the word the
 * toast uses.
 * @typedef {'allow' | 'deny' | 'dismiss'} ActionKind
 */
/** @type {Record<ActionKind, { label: string, args: (target: string, since: string) => string[] }>} */
export const ACTIONS = {
  allow: { label: 'Allow', args: (id) => ['approve', '--yes', id] },
  deny: { label: 'Deny', args: (id) => ['deny', '--yes', id] },
  dismiss: {
    label: 'Dismiss',
    args: (id, since) => ['inbox', 'dismiss', ...(since ? [`--since=${since}`] : []), id],
  },
}

// Control characters (C0, DEL, C1), zero-width characters, the line and
// paragraph separators, and the bidirectional controls.
/** @param {number} cp */
function isStripped(cp) {
  return (
    cp < 0x20 ||
    (cp >= 0x7f && cp <= 0x9f) ||
    (cp >= 0x200b && cp <= 0x200f) ||
    (cp >= 0x2028 && cp <= 0x202e) ||
    (cp >= 0x2060 && cp <= 0x2069) ||
    cp === 0xfeff
  )
}

// Drops control, zero-width, and bidirectional characters, folds whitespace,
// and keeps at most MAX_TEXT characters. Anything but a string is "".
/** @param {unknown} s */
export function clean(s) {
  if (typeof s !== 'string') return ''
  const out = Array.from(s)
    .map((ch) => (isStripped(ch.codePointAt(0) ?? 0) ? ' ' : ch))
    .join('')
    .replace(/\s+/g, ' ')
    .trim()
  return Array.from(out).slice(0, MAX_TEXT).join('')
}

// The first non-empty line of a CLI's output, cleaned, without the
// `sessionhub: ` the CLI puts before an error.
/** @param {unknown} out */
export function firstLine(out) {
  const line = String(out ?? '')
    .split(/\r?\n/)
    .find((l) => l.trim() !== '')
  return clean((line ?? '').replace(/^sessionhub:\s*/, ''))
}

// Cuts s to cols characters, with an ellipsis when it was longer.
/**
 * @param {string} s
 * @param {number} cols
 */
export function fit(s, cols) {
  const chars = Array.from(s)
  if (chars.length <= cols) return s
  return chars.slice(0, Math.max(0, cols - 1)).join('') + '…'
}

/** @param {number} ms */
export function age(ms) {
  const s = Math.max(0, Math.floor(ms / 1000))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

// What a person reads to decide: the first non-empty string among
// INPUT_FIELDS, a cut input (stored as a JSON string) as itself, else
// compact JSON. The result is untrusted: clean it.
/** @param {unknown} input */
export function inputText(input) {
  if (typeof input === 'string') return input
  if (input && typeof input === 'object' && !Array.isArray(input)) {
    const m = /** @type {Record<string, unknown>} */ (input)
    for (const k of INPUT_FIELDS) {
      const v = m[k]
      if (typeof v === 'string' && v !== '') return v
    }
  }
  return input === undefined || input === null ? '' : JSON.stringify(input)
}

// Parses `sessionhub inbox --json` into what the pane draws: every string
// cleaned, and anything it does not recognise left out. Throws on JSON that
// does not parse.
/**
 * @param {string} stdout
 * @returns {{ items: PaneItem[], counts: PaneCounts }}
 */
export function parseInbox(stdout) {
  const raw = JSON.parse(stdout)
  /** @type {PaneItem[]} */
  const items = []
  for (const it of Array.isArray(raw?.items) ? raw.items : []) {
    const group = it?.group
    if (group !== 'blocked' && group !== 'waiting' && group !== 'finished') continue
    const s = it.session ?? {}
    const id = typeof s.id === 'string' ? s.id : ''
    if (!SESSION_ID_RE.test(id)) continue
    const since = parseTime(it.since)
    items.push({
      group,
      sessionId: id,
      title: clean(s.title) || id.slice(0, 8),
      machine: clean(s.machine),
      since: Number.isFinite(since) ? since : 0,
      sinceText: Number.isFinite(since) && typeof it.since === 'string' && SINCE_RE.test(it.since) ? it.since : '',
      blockedOn: group === 'blocked' ? clean(s.blocked_on) : '',
      waitingOn: group === 'waiting' && Array.isArray(it.waiting_on) ? it.waiting_on.map(clean).filter(Boolean) : [],
      permission: group === 'blocked' ? parsePermission(it.permission) : null,
    })
  }
  const c = raw?.counts ?? {}
  return {
    items,
    counts: {
      blocked: count(c.blocked, items, 'blocked'),
      waiting: count(c.waiting, items, 'waiting'),
      finished: count(c.finished, items, 'finished'),
    },
  }
}

// Parses an RFC 3339 time as Go writes it, in milliseconds, or NaN. Go
// writes up to 9 fractional digits; ECMAScript promises Date.parse only 3,
// so the fraction is cut to milliseconds first.
/** @param {unknown} t */
export function parseTime(t) {
  if (typeof t !== 'string') return NaN
  return Date.parse(t.replace(/(\.\d{3})\d+/, '$1'))
}

/**
 * @param {unknown} n
 * @param {PaneItem[]} items
 * @param {string} group
 */
function count(n, items, group) {
  return typeof n === 'number' && Number.isInteger(n) && n >= 0 ? n : items.filter((i) => i.group === group).length
}

/**
 * @param {any} p
 * @returns {PanePermission | null}
 */
function parsePermission(p) {
  if (!p || typeof p !== 'object' || typeof p.id !== 'string' || !REQUEST_ID_RE.test(p.id)) return null
  return {
    id: p.id,
    tool: clean(p.tool_name) || 'a tool',
    summary: clean(inputText(p.tool_input)),
    truncated: p.truncated === true,
  }
}

// Whether the pane offers Allow once: an open request whose whole input was
// stored, for any tool but AskUserQuestion, which a remote allow answers
// nothing for.
/** @param {PanePermission | null} p */
export function canAllow(p) {
  return p !== null && p.tool !== 'AskUserQuestion' && !p.truncated
}

/**
 * What the pane draws.
 * @typedef {object} PaneView
 * @property {InboxPaneState} state
 * @property {number} now milliseconds
 * @property {number} cols the body's columns
 * @property {(kind: ActionKind, target: string, since?: string) => unknown} onAction
 * @property {() => unknown} onRefresh
 * @property {() => unknown} onClose
 */

/**
 * The pane's tree, built from the surface's elements.
 * @param {{ Box: any, Text: any, Button: any }} el
 * @param {PaneView} v
 */
export function paneTree({ Box, Text, Button }, v) {
  const { state: s, now } = v
  const cols = Math.max(20, Math.floor(v.cols || 0))
  /**
   * @param {string} text
   * @param {Record<string, unknown>} [props]
   */
  const line = (text, props = {}) => (
    <Text wrap="truncate-end" {...props}>
      {fit(text, cols)}
    </Text>
  )

  let head
  if (s.at === 0) head = s.error === '' ? 'Reading the inbox…' : 'Could not read the inbox.'
  else if (s.items.length === 0) head = `Nothing needs you · updated ${age(now - s.at)} ago`
  else head = `${s.items.length} need you · updated ${age(now - s.at)} ago`

  /** @param {PaneItem} it */
  const item = (it) => {
    const p = it.permission
    const facts = [it.title, it.machine, it.since > 0 ? age(now - it.since) : ''].filter(Boolean).join(' · ')
    const details = []
    if (it.blockedOn !== '') details.push(line(`  ${it.blockedOn}`))
    if (p !== null) {
      details.push(line(`  asks to use ${p.tool}${p.summary === '' ? '' : `: ${p.summary}`}`))
      if (p.tool === 'AskUserQuestion') details.push(line('  answer in the session', { dimColor: true }))
      else if (p.truncated) details.push(line('  the input was cut; allow it in the terminal', { dimColor: true }))
    }
    for (const w of it.waitingOn.slice(0, MAX_WAITING_LINES)) details.push(line(`  • ${w}`))
    if (it.waitingOn.length > MAX_WAITING_LINES) {
      details.push(line(`  and ${it.waitingOn.length - MAX_WAITING_LINES} more`, { dimColor: true }))
    }
    return (
      <Box key={`item:${it.sessionId}`} flexDirection="column">
        {line(facts)}
        {details}
        <Box flexDirection="row" gap={1}>
          {p !== null && canAllow(p) && (
            <Button
              key={`allow:${it.sessionId}`}
              variant="primary"
              label="Allow once"
              onPress={() => v.onAction('allow', p.id)}
            />
          )}
          {p !== null && <Button key={`deny:${it.sessionId}`} label="Deny" onPress={() => v.onAction('deny', p.id)} />}
          {it.sessionId.length >= MIN_DISMISS_ID && (
            <Button
              key={`dismiss:${it.sessionId}`}
              label="Dismiss"
              onPress={() => v.onAction('dismiss', it.sessionId, it.sinceText)}
            />
          )}
        </Box>
      </Box>
    )
  }

  const groups = GROUPS.map(([group, heading]) => {
    const items = s.items.filter((i) => i.group === group)
    if (items.length === 0) return null
    return (
      <Box key={`group:${group}`} flexDirection="column" marginTop={1}>
        {line(`${heading} (${items.length})`, { bold: true })}
        {items.map(item)}
      </Box>
    )
  })

  return (
    <Box flexDirection="column">
      {line(head, { dimColor: true })}
      {s.error !== '' && line(`sessionhub inbox failed: ${s.error}`, { color: 'error' })}
      {groups}
      <Box flexDirection="row" gap={1} marginTop={1}>
        <Button key="refresh" hotkey="r" label="Refresh" onPress={() => v.onRefresh()} />
        <Button key="close" role="dismiss" hotkey="q" label="Close" onPress={() => v.onClose()} />
      </Box>
    </Box>
  )
}
