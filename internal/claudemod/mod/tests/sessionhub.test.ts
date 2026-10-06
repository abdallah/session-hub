import { describe, expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { statusText } from '../hooks/register.js'

type Call = { args: string[]; stdin?: string; timeoutMs?: number }
type Answer = { exitCode?: number; stdout?: string } | 'deny'

const SESSION = 'sess-1'
const OK = { exitCode: 0, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false }

// Stubs `$.process.run` as the sessionhub binary: records each call (argv without the
// binary's path) and answers through `answer`, keyed by the subcommand.
function fakeHub(on: On, answer: (sub: string, call: Call) => Answer = () => ({})): Call[] {
  const calls: Call[] = []
  on('process.run', ($, e) => {
    const call: Call = { args: e.argv.slice(1), stdin: e.init?.stdin, timeoutMs: e.init?.timeoutMs }
    calls.push(call)
    const a = answer(String(call.args[1]), call)
    if (a === 'deny') return { deny: 'sessionhub is broken' }
    return { value: { ...OK, ...a } }
  })
  on('session.id', () => ({ value: SESSION }))
  return calls
}

function of(calls: Call[], sub: string): Call[] {
  return calls.filter((c) => c.args[1] === sub)
}

const QUESTIONS = [
  {
    question: 'Which library should we use?',
    header: 'Library',
    options: [
      { label: 'date-fns', description: 'small' },
      { label: 'luxon', description: 'zones' },
    ],
    multiSelect: false,
  },
]

// A blocked-on clear: the questions again, with `clear` set.
function isClear(stdin: string | undefined): boolean {
  return JSON.parse(stdin ?? '{}').clear === true
}

describe('blocked on', () => {
  test('posts the questions before the answer and clears after', async ($, on) => {
    const order: string[] = []
    const calls = fakeHub(on, (sub, call) => {
      order.push(isClear(call.stdin) ? 'clear' : 'post')
      return {}
    })
    on('tool.call', () => {
      order.push('answered')
      return { result: { answers: { 'Which library should we use?': 'luxon' } } }
    })

    const clock = mock.clock(on)
    mock.store(on)
    const r = await $.tool.call({ tool: 'AskUserQuestion', questions: QUESTIONS })
    await clock.settle()

    expect(r).toEqual({ result: { answers: { 'Which library should we use?': 'luxon' } } })
    const posts = of(calls, 'blocked-on')
    expect(posts.length).toBe(2)
    expect(posts[0]?.args).toEqual(['mod', 'blocked-on', '--session', SESSION])
    expect(JSON.parse(posts[0]?.stdin ?? '')).toEqual({ questions: QUESTIONS })
    expect(posts[0]?.timeoutMs).toBe(10_000)
    expect(posts[1]?.args).toEqual(['mod', 'blocked-on', '--session', SESSION])
    expect(JSON.parse(posts[1]?.stdin ?? '')).toEqual({ questions: QUESTIONS, clear: true })
    expect(order[order.length - 1]).toBe('clear')
  })

  test('a hanging sessionhub does not hold the question or the answer', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls: Call[] = []
    on('process.run', async ($, e) => {
      calls.push({ args: e.argv.slice(1), stdin: e.init?.stdin })
      await clock.sleep(10_000)
      return { value: OK }
    })
    on('session.id', () => ({ value: SESSION }))
    on('tool.call', () => ({ result: 'answered' }))

    const r = await $.tool.call({ tool: 'AskUserQuestion', questions: QUESTIONS })

    // The answer is back while the post still hangs, and no clear has started.
    expect(r).toEqual({ result: 'answered' })
    expect(calls.map((c) => isClear(c.stdin))).toEqual([false])

    // Once the post ends, the clear follows it.
    await clock.advance(10_000)
    expect(calls.map((c) => isClear(c.stdin))).toEqual([false, true])
  })

  test('a failing sessionhub does not block the tool', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls = fakeHub(on, () => 'deny')
    on('tool.call', () => ({ result: { answers: { q: 'a' } } }))

    const r = await $.tool.call({ tool: 'AskUserQuestion', questions: QUESTIONS })
    await clock.settle()

    expect(r).toEqual({ result: { answers: { q: 'a' } } })
    expect(of(calls, 'blocked-on').length).toBe(2)
  })

  test('a sessionhub exiting non-zero does not block the tool', async ($, on) => {
    fakeHub(on, () => ({ exitCode: 1 }))
    on('tool.call', () => ({ result: 'answered' }))

    expect(await $.tool.call({ tool: 'AskUserQuestion', questions: QUESTIONS })).toEqual({ result: 'answered' })
  })

  test('other tools are left alone', async ($, on) => {
    const calls = fakeHub(on)
    on('tool.call', () => ({ result: 'ok' }))

    await $.tool.call({ tool: 'Bash', command: 'ls' })

    expect(calls).toEqual([])
  })
})

describe('usage', () => {
  test('passes context and cost through', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls = fakeHub(on)
    on('session.measure', ($, e) => ({ changed: e.changed }))
    const context = { tokens: 50_000, window: 200_000, percent: 25 }

    await $.session.measure({ context, rateLimits: [], cost: { usd: 1.25 }, changed: ['context', 'cost'] })
    await clock.settle()

    const usage = of(calls, 'usage')
    expect(usage.length).toBe(1)
    expect(usage[0]?.args).toEqual(['mod', 'usage', '--session', SESSION])
    expect(JSON.parse(usage[0]?.stdin ?? '')).toEqual({ context, cost: { usd: 1.25 } })
    expect(usage[0]?.timeoutMs).toBe(10_000)
  })

  test('skips a measure that moved only the rate limits', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls = fakeHub(on)
    on('session.measure', ($, e) => ({ changed: e.changed }))

    await $.session.measure({ context: { window: 200_000 }, rateLimits: [], changed: ['rateLimits'] })
    await clock.settle()

    expect(of(calls, 'usage')).toEqual([])
  })

  test('a failing sessionhub does not fail the measure', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    fakeHub(on, () => 'deny')
    on('session.measure', ($, e) => ({ changed: e.changed }))

    const r = await $.session.measure({ context: { window: 1, percent: 1 }, rateLimits: [], changed: ['context'] })
    await clock.settle()

    expect(r).toEqual({ changed: ['context'] })
  })
})

const START = { cwd: '/work', surface: 'terminal', isInteractive: true } as const

describe('messages', () => {
  test('a polled message is submitted and reported delivered', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    let polls = 0
    const calls = fakeHub(on, (sub) => {
      if (sub === 'poll') {
        polls++
        return polls === 1 ? { stdout: '{"id":"m-1","text":"run the tests"}\n' } : { stdout: '' }
      }
      if (sub === 'inbox-count') return { stdout: '{"blocked":0,"waiting":0,"finished":0}' }
      return {}
    })
    const submitted: string[] = []
    on('prompt.submit', ($, e) => {
      submitted.push(e.text)
      return { text: e.text }
    })
    on('session.start', () => ({ cwd: '/work' }))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start(START)
    await clock.settle()

    expect(submitted).toEqual(['run the tests'])
    const poll = of(calls, 'poll')[0]
    expect(poll?.args).toEqual(['mod', 'poll', '--session', SESSION])
    expect(poll?.timeoutMs).toBe(35_000)
    const results = of(calls, 'result')
    expect(results.map((c) => c.args)).toEqual([['mod', 'result', '--message', 'm-1', '--state', 'delivered']])
    expect(results[0]?.timeoutMs).toBe(10_000)

    // The loop polls again a second later.
    expect(of(calls, 'poll').length).toBe(1)
    await clock.advance(1_000)
    expect(of(calls, 'poll').length).toBe(2)
  })

  test('a submit that throws is reported busy', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    let polls = 0
    const calls = fakeHub(on, (sub) => {
      if (sub === 'poll') return ++polls === 1 ? { stdout: '{"id":"m-3","text":"hello"}' } : {}
      if (sub === 'inbox-count') return { stdout: '{}' }
      return {}
    })
    on('prompt.submit', () => {
      throw new Error('session gone')
    })
    on('session.start', () => ({ cwd: '/work' }))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start(START)
    await clock.settle()

    const results = of(calls, 'result').map((c) => c.args)
    expect(results.length).toBe(1)
    expect(results[0]?.slice(0, 6)).toEqual(['mod', 'result', '--message', 'm-3', '--state', 'busy'])
  })

  test('a submit a hook drops is reported refused', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    let polls = 0
    const calls = fakeHub(on, (sub) => {
      if (sub === 'poll') return ++polls === 1 ? { stdout: '{"id":"m-2","text":"hello"}' } : {}
      if (sub === 'inbox-count') return { stdout: '{}' }
      return {}
    })
    on('prompt.submit', () => ({ drop: 'nope' }))
    on('session.start', () => ({ cwd: '/work' }))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start(START)
    await clock.settle()

    // A drop from a hook beneath is a refusal, with its reason.
    expect(of(calls, 'result').map((c) => c.args)).toEqual([
      ['mod', 'result', '--message', 'm-2', '--state', 'refused', '--detail', 'nope'],
    ])
  })

  test('a submit still pending after 3 s is reported delivered, and polling goes on', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    let polls = 0
    const calls = fakeHub(on, (sub) => {
      if (sub === 'poll') return ++polls === 1 ? { stdout: '{"id":"m-4","text":"later"}' } : {}
      if (sub === 'inbox-count') return { stdout: '{}' }
      return {}
    })
    // The session is in a long turn: the submit settles only after 60 s, and
    // then a hook drops it.
    on('prompt.submit', async () => {
      await clock.sleep(60_000)
      return { drop: 'too late' }
    })
    const logs: string[] = []
    on('ui.log', ($, e) => {
      logs.push(e.text)
      return { value: undefined }
    })
    on('session.start', () => ({ cwd: '/work' }))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start(START)
    await clock.settle()
    expect(of(calls, 'result')).toEqual([])

    await clock.advance(3_000)
    expect(of(calls, 'result').map((c) => c.args)).toEqual([
      ['mod', 'result', '--message', 'm-4', '--state', 'delivered'],
    ])
    // The next poll does not wait for the turn to end.
    await clock.advance(1_000)
    expect(of(calls, 'poll').length).toBe(2)
    expect(logs).toEqual([])

    await clock.advance(56_000)
    expect(logs.length).toBe(1)
    expect(logs[0]).toContain('m-4')
    expect(logs[0]).toContain('too late')
    expect(of(calls, 'result').length).toBe(1)
  })

  test('the loops start only in an interactive terminal session', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls = fakeHub(on, (sub) => (sub === 'inbox-count' ? { stdout: '{}' } : {}))
    on('session.start', () => ({ cwd: '/work' }))
    on('classic.SessionStart', () => ({}))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start({ cwd: '/work', surface: 'desktop', isInteractive: true })
    await $.session.start({ cwd: '/work', surface: 'terminal', isInteractive: false })
    await $.session.start({ cwd: '/work', surface: null, isInteractive: false })
    // The restart after /clear keeps to the same rule.
    await $.classic.SessionStart({ source: 'clear' })
    await clock.advance(120_000)

    expect(calls).toEqual([])
  })

  test('a failed poll backs off for 30 seconds', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls = fakeHub(on, (sub) => (sub === 'poll' ? { exitCode: 1 } : { stdout: '{}' }))
    on('session.start', () => ({ cwd: '/work' }))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start(START)
    await clock.settle()
    expect(of(calls, 'poll').length).toBe(1)

    await clock.advance(29_000)
    expect(of(calls, 'poll').length).toBe(1)
    await clock.advance(1_000)
    expect(of(calls, 'poll').length).toBe(2)
  })

  test('an empty poll that held the long poll polls again a second later', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls: Call[] = []
    on('process.run', async ($, e) => {
      const args = e.argv.slice(1)
      calls.push({ args })
      if (args[1] === 'poll') await clock.sleep(25_000)
      return { value: { ...OK, stdout: args[1] === 'inbox-count' ? '{}' : '' } }
    })
    on('session.id', () => ({ value: SESSION }))
    on('session.start', () => ({ cwd: '/work' }))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start(START)
    await clock.settle()
    await clock.advance(25_000)
    expect(of(calls, 'poll').length).toBe(1)
    await clock.advance(1_000)
    expect(of(calls, 'poll').length).toBe(2)
  })

  test('session.end stops the loops', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls = fakeHub(on, (sub) => (sub === 'inbox-count' ? { stdout: '{}' } : {}))
    on('session.start', () => ({ cwd: '/work' }))
    on('session.end', ($, e) => ({ sessionId: e.sessionId }))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start(START)
    await clock.settle()
    const before = calls.length
    await $.session.end({ reason: 'prompt_input_exit', sessionId: SESSION, resume: { id: SESSION } } as never)
    await clock.advance(120_000)

    expect(calls.length).toBe(before)
  })

  test('the loops start again after /clear', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls = fakeHub(on, (sub) => (sub === 'inbox-count' ? { stdout: '{}' } : {}))
    on('session.start', () => ({ cwd: '/work' }))
    on('session.end', ($, e) => ({ sessionId: e.sessionId }))
    on('classic.SessionStart', () => ({}))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start(START)
    await clock.settle()
    await $.session.end({ reason: 'clear', sessionId: SESSION, resume: { id: SESSION } } as never)
    await $.classic.SessionStart({ source: 'clear' })
    await clock.settle()

    // The poll starts again; the inbox count comes from the fresh cache.
    expect(of(calls, 'poll').length).toBe(2)
    expect(of(calls, 'inbox-count').length).toBe(1)
  })

  test('a second session.start keeps one loop of each kind', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const calls = fakeHub(on, (sub) => (sub === 'inbox-count' ? { stdout: '{}' } : {}))
    on('session.start', () => ({ cwd: '/work' }))
    on('ui.status', () => ({ value: undefined }))

    await $.session.start(START)
    await $.session.start(START)
    await clock.settle()
    expect(of(calls, 'inbox-count').length).toBe(1)

    await clock.advance(60_000)
    expect(of(calls, 'inbox-count').length).toBe(2)
  })
})

describe('inbox status', () => {
  test('shows the counts every minute and clears when all are 0', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    const answers = [
      '{"blocked":2,"waiting":1,"finished":0}',
      '{"blocked":0,"waiting":0,"finished":3}',
      '{"blocked":0,"waiting":0,"finished":0}',
    ]
    let n = 0
    const calls = fakeHub(on, (sub) => (sub === 'inbox-count' ? { stdout: answers[n++] ?? '' } : {}))
    const lines: (string | undefined)[] = []
    on('ui.status', ($, e) => {
      lines.push(e.text)
      return { value: undefined }
    })
    on('session.start', () => ({ cwd: '/work' }))

    await $.session.start(START)
    await clock.settle()
    expect(lines).toEqual(['2 blocked · 1 waiting'])
    expect(of(calls, 'inbox-count')[0]?.args).toEqual(['mod', 'inbox-count'])
    expect(of(calls, 'inbox-count')[0]?.timeoutMs).toBe(10_000)

    await clock.advance(60_000)
    expect(lines).toEqual(['2 blocked · 1 waiting', '3 finished'])
    await clock.advance(60_000)
    expect(lines).toEqual(['2 blocked · 1 waiting', '3 finished', undefined])
  })

  test('a fresh cached count is used instead of running sessionhub', async ($, on) => {
    const clock = mock.clock(on, { now: 100_000 })
    const saved = new Map<string, unknown>([['inbox', { counts: { blocked: 5 }, at: 70_000 }]])
    on('store.get', ($, e) => ({ value: saved.get(e.key) }))
    on('store.set', ($, e) => {
      saved.set(e.key, e.value)
      return { value: undefined }
    })
    const calls = fakeHub(on, (sub) => (sub === 'inbox-count' ? { stdout: '{"blocked":1,"waiting":0,"finished":0}' } : {}))
    const lines: (string | undefined)[] = []
    on('ui.status', ($, e) => {
      lines.push(e.text)
      return { value: undefined }
    })
    on('session.start', () => ({ cwd: '/work' }))

    await $.session.start(START)
    await clock.settle()
    // Another session counted 30 s ago: use its counts.
    expect(lines).toEqual(['5 blocked'])
    expect(of(calls, 'inbox-count')).toEqual([])

    // A minute on, the cache is stale: run sessionhub and share the result.
    await clock.advance(60_000)
    expect(of(calls, 'inbox-count').length).toBe(1)
    expect(lines).toEqual(['5 blocked', '1 blocked'])
    expect(saved.get('inbox')).toEqual({ counts: { blocked: 1, waiting: 0, finished: 0 }, at: 160_000 })
  })

  test('a failing inbox-count clears the line', async ($, on) => {
    const clock = mock.clock(on)
    mock.store(on)
    fakeHub(on, (sub) => (sub === 'inbox-count' ? 'deny' : {}))
    const lines: (string | undefined)[] = []
    on('ui.status', ($, e) => {
      lines.push(e.text)
      return { value: undefined }
    })
    on('session.start', () => ({ cwd: '/work' }))

    await $.session.start(START)
    await clock.settle()
    expect(lines).toEqual([undefined])
  })

  test('statusText formats the counts', () => {
    expect(statusText({ blocked: 0, waiting: 0, finished: 0 })).toBeUndefined()
    expect(statusText({ blocked: 1, waiting: 2, finished: 3 })).toBe('1 blocked · 2 waiting · 3 finished')
    expect(statusText({ waiting: 4 })).toBe('4 waiting')
  })
})
