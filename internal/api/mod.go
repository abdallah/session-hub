package api

// The Claude Code mod's routes. See docs/server.md, "The Claude Code mod".
const (
	// SourceMod marks events the server records for a mod's report.
	SourceMod = "mod"
	// KindBlockedOn is the event POST /v1/sessions/{id}/blocked-on records.
	KindBlockedOn = "blocked_on"

	// MaxBlockedOnRunes caps BlockedOnIn.Text after cleaning.
	MaxBlockedOnRunes = 300
	// MaxLiveCostUSD caps UsageIn.CostUSD.
	MaxLiveCostUSD = 1_000_000
)

// BlockedOnIn is the body of POST /v1/sessions/{id}/blocked-on. A non-empty
// Text is what the session waits on. An empty Text clears it: with Clears,
// only when the stored text is Clears (the question that was answered);
// without, only when the mod set the block. Text and Clears are not both
// set.
type BlockedOnIn struct {
	Text   string `json:"text"`
	Clears string `json:"clears,omitempty"`
}

// UsageIn is the body of PUT /v1/sessions/{id}/usage. A nil field leaves the
// stored value as it is; at least one must be set.
type UsageIn struct {
	ContextPercent *int     `json:"context_percent,omitempty"` // 0 to 100
	CostUSD        *float64 `json:"cost_usd,omitempty"`        // 0 to MaxLiveCostUSD
}

// ModMessage is a message a session's mod claimed: the body of a 200 from
// GET /v1/sessions/{id}/messages/next. Text is the prompt to submit, with
// the sender line MessagePrompt adds.
type ModMessage struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// MessageResultIn is the body of POST /v1/messages/{id}/result: State is
// MessageDelivered, MessageBusy (the message stays queued and is offered
// again), or MessageRefused.
type MessageResultIn struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}
