// The sessionhub mod's $.state contract: what the inbox pane draws from.

// One open permission request on a blocked item, cleaned for drawing.
export type PanePermission = {
  // The request ID (`pr_...`), checked before it reaches an argv.
  id: string
  tool: string
  // One line of what the tool will do: a command, a path, or compact JSON.
  summary: string
  truncated: boolean
}

// One inbox item, cleaned for drawing. Every string came from the server
// and holds no control characters.
export type PaneItem = {
  group: 'blocked' | 'waiting' | 'finished'
  sessionId: string
  // The session title, or its first 8 characters when it has none.
  title: string
  machine: string
  // The item's trigger time in milliseconds, or 0 when unknown.
  since: number
  // The trigger time as the server sent it (RFC 3339), or "" when unknown.
  // Dismiss passes it back so a newer event stays in the inbox.
  sinceText: string
  blockedOn: string
  waitingOn: string[]
  permission: PanePermission | null
}

export type PaneCounts = { blocked: number; waiting: number; finished: number }

export type InboxPaneState = {
  items: PaneItem[]
  counts: PaneCounts
  // Why the last read failed, or "" when it worked.
  error: string
  // When the last read worked, in milliseconds; 0 before the first.
  at: number
}

declare module 'claude-code' {
  interface PluginState {
    sessionhub: { pane: InboxPaneState }
  }
}
