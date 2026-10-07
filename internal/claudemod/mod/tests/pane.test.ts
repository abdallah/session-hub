import { describe, expect, mock, test } from 'claude-code/testing'
import type { Engine, Plugin } from 'claude-code/testing'
import type { On } from 'claude-code'
import { clean, fit, inputText, parseInbox, parseTime } from '../hooks/pane.jsx'

type Call = { args: string[]; timeoutMs?: number }
type Answer = { exitCode?: number; stdout?: string; stderr?: string } | 'deny'

const PANE = 'sessionhub-inbox'
const OK = { exitCode: 0, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false }
const NOW = Date.parse('2026-10-07T12:00:00Z')
const ESC = String.fromCharCode(27)
const RLO = String.fromCharCode(0x202e)

const PR_BASH = 'pr_' + 'A'.repeat(22)
const PR_ASK = 'pr_' + 'B'.repeat(22)
const PR_CUT = 'pr_' + 'C'.repeat(22)
const S_BASH = 'aaaa1111-0000-0000-0000-000000000001'
const S_ASK = 'bbbb2222-0000-0000-0000-000000000002'
const S_CUT = 'cccc3333-0000-0000-0000-000000000003'
const S_WAIT = 'dddd4444-0000-0000-0000-000000000004'
const S_DONE = 'eeee5555-0000-0000-0000-000000000005'

const ago = (min: number) => new Date(NOW - min * 60_000).toISOString()

// Whether s holds a control or bidirectional character.
function hasControl(s: string): boolean {
  return Array.from(s).some((ch) => {
    const cp = ch.codePointAt(0) ?? 0
    return cp < 0x20 || (cp >= 0x7f && cp <= 0x9f) || (cp >= 0x202a && cp <= 0x202e) || (cp >= 0x2066 && cp <= 0x2069)
  })
}

// `sessionhub inbox --json` as the server sends it: one item of each kind.
const INBOX = {
  items: [
    {
      group: 'blocked',
      // Go writes RFC 3339 with up to 9 fractional digits.
      since: ago(5.5).replace('.000Z', '.123456789Z'),
      session: { id: S_BASH, title: 'Fix CI', machine: 'box-a', blocked_on: 'Question: Which runner? (small, large)' },
      permission: { id: PR_BASH, tool_name: 'Bash', tool_input: { command: 'make test\nrm -rf build', description: 'x' } },
    },
    {
      group: 'blocked',
      since: ago(90),
      session: { id: S_ASK, title: 'Pick a library', machine: 'box-b' },
      permission: { id: PR_ASK, tool_name: 'AskUserQuestion', tool_input: { questions: [{ question: 'Which?' }] } },
    },
    {
      group: 'blocked',
      since: ago(1),
      session: { id: S_CUT, title: 'Long command', machine: 'box-a' },
      permission: { id: PR_CUT, tool_name: 'Bash', tool_input: '{"command":"echo aaaa', truncated: true },
    },
    {
      group: 'waiting',
      since: ago(60 * 26),
      session: { id: S_WAIT, title: `${ESC}[31mRed${ESC}[0m ${RLO}title`, machine: 'box-c' },
      waiting_on: ['review the plan', `pick${ESC}[2J a name`, 'third', 'fourth'],
    },
    { group: 'finished', since: ago(0.5), session: { id: S_DONE, machine: 'box-a' } },
  ],
  counts: { blocked: 3, waiting: 1, finished: 1 },
}

const PROPS = {
  title: 'sessionhub inbox',
  isFocused: false,
  bodyColumns: 60,
  placement: 'dock',
  scroll: { offset: 0, bodyRows: 40 },
  view: {},
} as const

// Stubs the engine's panes, toasts, and status line beneath the mod, and
// `$.process.run` as the sessionhub binary: records each argv after the
// binary and answers through `answer`.
function fakeEngine(on: On, answer: (call: Call) => Answer = () => ({})) {
  const calls: Call[] = []
  const panes: string[] = []
  const toasts: string[] = []
  const status: (string | undefined)[] = []
  const opened: unknown[] = []
  on('process.run', ($, e) => {
    const call: Call = { args: e.argv.slice(1), timeoutMs: e.init?.timeoutMs }
    calls.push(call)
    const a = answer(call)
    if (a === 'deny') return { deny: 'sessionhub is broken' }
    return { value: { ...OK, ...a } }
  })
  on('ui.open', ($, e) => {
    opened.push(e)
    if (!panes.includes(e.id)) panes.push(e.id)
    return { value: { isPlaced: true } }
  })
  on('ui.panes', () => ({
    value: panes.map((id) => ({ id, title: id, isShown: true, isFocused: false, isPlaced: true })),
  }))
  on('ui.close', ($, e) => {
    const i = panes.indexOf(e.id)
    if (i >= 0) panes.splice(i, 1)
    return { value: undefined }
  })
  on('ui.toast', ($, e) => {
    toasts.push(e.text)
    return { value: undefined }
  })
  on('ui.status', ($, e) => {
    status.push(e.text)
    return { value: undefined }
  })
  on('session.id', () => ({ value: 'sess-1' }))
  return { calls, panes, toasts, status, opened }
}

const isRead = (c: Call) => c.args[0] === 'inbox' && c.args[1] === '--json'
const reads = (calls: Call[]) => calls.filter(isRead).length

// Answers every `sessionhub inbox --json` with INBOX and every other call
// with success, unless `extra` answers first.
function inboxAnswer(extra: (call: Call) => Answer | undefined = () => undefined) {
  return (call: Call): Answer => {
    const a = extra(call)
    if (a !== undefined) return a
    if (isRead(call)) return { stdout: JSON.stringify(INBOX) + '\n' }
    return {}
  }
}

function mountPane($: Engine, bodyColumns = 60) {
  return $.ui.mount({
    plugin: 'sessionhub',
    surface: 'terminal',
    component: 'Pane',
    props: { ...PROPS, bodyColumns } as never,
    requestId: PANE,
  })
}

// Runs `/inbox`, as the person typing it does.
function inbox($: Engine) {
  return $.command.run({ command: 'inbox', args: '' } as never)
}

describe('inbox pane', () => {
  test('/inbox opens the pane and reads the inbox; running it again closes it', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    const eng = fakeEngine(on, inboxAnswer())

    expect(await inbox($)).toEqual({ text: 'Opened the sessionhub inbox.' })
    expect(eng.panes).toEqual([PANE])
    expect(eng.opened[0]).toMatchObject({ id: PANE, title: 'sessionhub inbox' })
    await clock.settle()
    expect(eng.calls).toEqual([{ args: ['inbox', '--json'], timeoutMs: 20_000 }])

    expect(await inbox($)).toEqual({ text: 'Closed the sessionhub inbox.' })
    expect(eng.panes).toEqual([])
  })

  test('draws the groups, the right buttons, and only cleaned text', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    fakeEngine(on, inboxAnswer())
    await inbox($)
    await clock.settle()
    const ui = await mountPane($)

    const texts = (await ui.findAll({ type: 'Text' })).map((t) => t.text)
    expect(texts).toContain('5 need you · updated 0s ago')
    expect(texts).toContain('Blocked (3)')
    expect(texts).toContain('Waiting (1)')
    expect(texts).toContain('Finished (1)')
    expect(texts).toContain('Fix CI · box-a · 5m')
    expect(texts).toContain('  Question: Which runner? (small, large)')
    expect(texts).toContain('  asks to use Bash: make test rm -rf build')
    expect(texts).toContain('Pick a library · box-b · 1h')
    expect(texts).toContain('  answer in the session')
    expect(texts).toContain('  the input was cut; allow it in the terminal')
    expect(texts).toContain('[31mRed [0m title · box-c · 1d')
    expect(texts).toContain('  • review the plan')
    expect(texts).toContain('  • pick [2J a name')
    expect(texts).toContain('  and 1 more')
    // A session with no title shows its short ID.
    expect(texts).toContain('eeee5555 · box-a · 30s')
    expect(texts.filter(hasControl)).toEqual([])

    const buttons = (await ui.findAll({ type: 'Button' })).map((b) => b.key)
    expect(buttons).toEqual([
      `allow:${S_BASH}`,
      `deny:${S_BASH}`,
      `dismiss:${S_BASH}`,
      // AskUserQuestion: no Allow, which would answer nothing.
      `deny:${S_ASK}`,
      `dismiss:${S_ASK}`,
      // A cut input: no Allow, which the server refuses.
      `deny:${S_CUT}`,
      `dismiss:${S_CUT}`,
      `dismiss:${S_WAIT}`,
      `dismiss:${S_DONE}`,
      'refresh',
      'close',
    ])
    expect((await ui.find({ key: `allow:${S_BASH}` }))?.props).toMatchObject({ label: 'Allow once' })
    expect((await ui.find({ key: 'close' }))?.props).toMatchObject({ hotkey: 'q', role: 'dismiss' })
  })

  test('fits every line to the pane width', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    fakeEngine(on, inboxAnswer())
    await inbox($)
    await clock.settle()
    const ui = await mountPane($, 24)

    const texts = (await ui.findAll({ type: 'Text' })).map((t) => t.text)
    expect(texts.filter((t) => Array.from(t).length > 24)).toEqual([])
    expect(texts).toContain('  Question: Which runne…')
  })

  test('Allow, Deny, and Dismiss run the CLI, toast, and read the inbox again', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    const eng = fakeEngine(
      on,
      inboxAnswer((call) => {
        if (call.args[0] === 'approve') return { stdout: `allowed once: ${PR_BASH}\n` }
        if (call.args[0] === 'deny') return { stdout: `denied: ${PR_ASK}\n` }
        if (call.args[1] === 'dismiss') return { stdout: 'dismissed dddd4444 (Red title)\n' }
        return undefined
      }),
    )
    await inbox($)
    await clock.settle()
    const ui = await mountPane($)
    expect(reads(eng.calls)).toBe(1)

    await ui.press({ key: `allow:${S_BASH}` })
    await ui.press({ key: `deny:${S_ASK}` })
    await ui.press({ key: `dismiss:${S_WAIT}` })

    const actions = eng.calls.filter((c) => !isRead(c))
    expect(actions.map((c) => c.args)).toEqual([
      ['approve', '--yes', PR_BASH],
      ['deny', '--yes', PR_ASK],
      ['inbox', 'dismiss', `--since=${ago(60 * 26)}`, S_WAIT],
    ])
    expect(actions.every((c) => c.timeoutMs === 20_000)).toBe(true)
    expect(eng.toasts).toEqual([
      `sessionhub: allowed once: ${PR_BASH}`,
      `sessionhub: denied: ${PR_ASK}`,
      'sessionhub: dismissed dddd4444 (Red title)',
    ])
    expect(reads(eng.calls)).toBe(4)
  })

  test('nothing but reads runs without a press', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    const eng = fakeEngine(on, inboxAnswer())
    await inbox($)
    await clock.settle()
    await mountPane($)
    await clock.advance(120_000)

    expect(eng.calls.filter((c) => !isRead(c))).toEqual([])
  })

  test('a failing action shows an error toast and the pane keeps working', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    const eng = fakeEngine(
      on,
      inboxAnswer((call) => {
        if (call.args[0] === 'approve') return { exitCode: 1, stderr: 'sessionhub: approve: already decided\n' }
        if (call.args[0] === 'deny') return 'deny'
        return undefined
      }),
    )
    await inbox($)
    await clock.settle()
    const ui = await mountPane($)

    await ui.press({ key: `allow:${S_BASH}` })
    await ui.press({ key: `deny:${S_BASH}` })

    expect(eng.toasts.length).toBe(2)
    expect(eng.toasts[0]).toBe('sessionhub: Allow failed: approve: already decided')
    expect(eng.toasts[1]).toMatch(/^sessionhub: Deny failed: .*sessionhub is broken/)
    expect((await ui.find({ key: `allow:${S_BASH}` }))?.type).toBe('Button')
    expect(reads(eng.calls)).toBe(3)
  })

  test('a failing read shows the error, keeps the last items, and backs off', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    let fail = false
    const eng = fakeEngine(
      on,
      inboxAnswer(() => (fail ? { exitCode: 1, stderr: 'sessionhub: inbox: server unreachable\n' } : undefined)),
    )
    await inbox($)
    await clock.settle()
    const ui = await mountPane($)

    fail = true
    await clock.advance(15_000)
    expect(reads(eng.calls)).toBe(2)
    const texts = (await ui.findAll({ type: 'Text' })).map((t) => t.text)
    expect(texts).toContain('sessionhub inbox failed: inbox: server unreachable')
    expect(texts).toContain('Fix CI · box-a · 5m')

    // After a failure the next read waits 30 s.
    fail = false
    await clock.advance(15_000)
    expect(reads(eng.calls)).toBe(2)
    await clock.advance(15_000)
    expect(reads(eng.calls)).toBe(3)
    const after = (await ui.findAll({ type: 'Text' })).map((t) => t.text)
    expect(after.filter((t) => t.startsWith('sessionhub inbox failed'))).toEqual([])
  })

  test('output that is not JSON shows an error instead of failing', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    fakeEngine(on, () => ({ stdout: 'not json' }))
    await inbox($)
    await clock.settle()
    const ui = await mountPane($)

    const texts = (await ui.findAll({ type: 'Text' })).map((t) => t.text)
    expect(texts[0]).toBe('Could not read the inbox.')
    expect(texts[1]).toMatch(/^sessionhub inbox failed: /)
  })

  test('refreshes every 15 seconds while open and stops when /inbox closes it', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    const eng = fakeEngine(on, inboxAnswer())
    await inbox($)
    await clock.settle()
    expect(reads(eng.calls)).toBe(1)
    await clock.advance(15_000)
    expect(reads(eng.calls)).toBe(2)
    await clock.advance(15_000)
    expect(reads(eng.calls)).toBe(3)

    await inbox($)
    await clock.advance(120_000)
    expect(reads(eng.calls)).toBe(3)
  })

  test('the Close button stops the refresh', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    const eng = fakeEngine(on, inboxAnswer())
    await inbox($)
    await clock.settle()
    const ui = await mountPane($)
    await ui.press({ key: 'close' })
    expect(eng.panes).toEqual([])
    await clock.advance(120_000)
    expect(reads(eng.calls)).toBe(1)
  })

  // A close the mod did not make, as the person's close mark makes one: an
  // outer plugin's `$.ui.close` passes through the mod's `ui.close` hook.
  const closer: Plugin = {
    name: 'closer',
    tier: 'prepend',
    register(on) {
      on('command.run', { command: 'close-inbox' }, async ($) => {
        await $.ui.close({ id: 'sessionhub-inbox' })
        return { text: 'closed' }
      })
    },
  }

  test('a close by anyone else stops the refresh', { plugins: [closer] }, async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    const eng = fakeEngine(on, inboxAnswer())
    await inbox($)
    await clock.settle()
    expect(reads(eng.calls)).toBe(1)

    await $.command.run({ command: 'close-inbox', args: '' } as never)
    expect(eng.panes).toEqual([])
    await clock.advance(120_000)
    expect(reads(eng.calls)).toBe(1)
  })

  test('session.end stops the refresh', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    const eng = fakeEngine(on, inboxAnswer())
    on('session.end', ($, e) => ({ sessionId: e.sessionId }))
    await inbox($)
    await clock.settle()
    await $.session.end({ reason: 'prompt_input_exit', sessionId: 'sess-1', resume: { id: 'sess-1' } } as never)
    await clock.advance(120_000)
    expect(reads(eng.calls)).toBe(1)
  })

  test('a session.start finds the pane still open and refreshes it again', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    const eng = fakeEngine(
      on,
      inboxAnswer((call) => (call.args[0] === 'mod' ? { stdout: call.args[1] === 'inbox-count' ? '{}' : '' } : undefined)),
    )
    on('session.start', () => ({ cwd: '/work' }))
    on('command.register', ($, e) => ({ value: { command: e.name } }))
    eng.panes.push(PANE)

    await $.session.start({ cwd: '/work', surface: 'terminal', isInteractive: true })
    await clock.settle()
    expect(reads(eng.calls)).toBe(1)
    await clock.advance(15_000)
    expect(reads(eng.calls)).toBe(2)
  })

  test('/inbox is registered only in an interactive terminal session', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    mock.store(on)
    fakeEngine(on, () => ({ stdout: '{}' }))
    const registered: string[] = []
    on('session.start', () => ({ cwd: '/work' }))
    on('command.register', ($, e) => {
      registered.push(e.name)
      return { value: { command: e.name } }
    })

    await $.session.start({ cwd: '/work', surface: 'desktop', isInteractive: true })
    await $.session.start({ cwd: '/work', surface: 'terminal', isInteractive: false })
    expect(registered).toEqual([])
    await $.session.start({ cwd: '/work', surface: 'terminal', isInteractive: true })
    await clock.settle()
    expect(registered).toEqual(['inbox'])
  })

  test('a read shares the counts with the status line and the other sessions', async ($, on) => {
    const clock = mock.clock(on, { now: NOW })
    const saved = new Map<string, unknown>()
    on('store.get', ($, e) => ({ value: saved.get(e.key) }))
    on('store.set', ($, e) => {
      saved.set(e.key, e.value)
      return { value: undefined }
    })
    const eng = fakeEngine(on, inboxAnswer())
    await inbox($)
    await clock.settle()

    expect(saved.get('inbox')).toEqual({ counts: { blocked: 3, waiting: 1, finished: 1 }, at: NOW })
    expect(eng.status).toEqual(['3 blocked · 1 waiting · 1 finished'])
  })
})

describe('pane helpers', () => {
  test('clean drops control and bidirectional characters and folds whitespace', () => {
    expect(clean(`a${ESC}[31m\tb\n\nc${RLO}d`)).toBe('a [31m b c d')
    expect(clean(42)).toBe('')
    expect(Array.from(clean('x'.repeat(2000))).length).toBe(500)
  })

  test('parseTime reads Go times with up to 9 fractional digits', () => {
    expect(parseTime('2026-10-07T11:55:00.123456789Z')).toBe(Date.parse('2026-10-07T11:55:00.123Z'))
    expect(parseTime('2026-10-07T13:55:00+02:00')).toBe(Date.parse('2026-10-07T11:55:00Z'))
    expect(parseTime('soon')).toBeNaN()
    expect(parseTime(undefined)).toBeNaN()
  })

  test('fit cuts with an ellipsis', () => {
    expect(fit('abcdef', 6)).toBe('abcdef')
    expect(fit('abcdefg', 6)).toBe('abcde…')
  })

  test('inputText picks what a person reads to decide', () => {
    expect(inputText({ description: 'd', command: 'ls' })).toBe('ls')
    expect(inputText({ file_path: '/tmp/x', content: 'y' })).toBe('/tmp/x')
    expect(inputText('{"command":"cut')).toBe('{"command":"cut')
    expect(inputText({ a: 1 })).toBe('{"a":1}')
    expect(inputText(undefined)).toBe('')
  })

  test('parseInbox leaves out items and requests with IDs it does not accept', () => {
    const { items, counts } = parseInbox(
      JSON.stringify({
        items: [
          { group: 'blocked', session: { id: '-y' }, since: ago(1) },
          { group: 'odd', session: { id: S_BASH } },
          {
            group: 'blocked',
            session: { id: S_BASH },
            since: 'not a time',
            permission: { id: 'abcd', tool_name: 'Bash', tool_input: {} },
          },
        ],
      }),
    )
    expect(items.map((i) => i.sessionId)).toEqual([S_BASH])
    expect(items[0]?.permission).toBeNull()
    expect(items[0]?.since).toBe(0)
    expect(items[0]?.sinceText).toBe('')
    expect(counts).toEqual({ blocked: 1, waiting: 0, finished: 0 })
  })
})
