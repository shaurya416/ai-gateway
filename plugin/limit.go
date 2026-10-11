package plugin

// LimitKind names what a LimitExceeded notice is about.
type LimitKind string

// LimitSpend is a cumulative spend cap, such as the budget plugin's
// spend_limit_usd.
const LimitSpend LimitKind = "spend"

// LimitExceeded is a plugin's report that a limit it enforces has just begun
// refusing requests for one key: the first refusal since that key was last
// admitted. Repeated refusals of the same key are not reported again until a
// request for it has been admitted in between, so a key held at its cap by a
// retrying client produces one notice, not one per request.
//
// It carries the opaque id of the credential the limit is scoped to — never
// the credential itself — and no request or prompt content.
type LimitExceeded struct {
	// Plugin is the reporting plugin's name.
	Plugin string
	// Kind is what was exceeded.
	Kind LimitKind
	// KeyID is the opaque id of the API key the limit is scoped to.
	KeyID string
	// Scope names the limit's own partition where it has one — the budget
	// plugin's store_id — so two budgets refusing the same key are told apart.
	Scope string
	// Limit is the configured limit; Observed the value found at or past it.
	Limit    float64
	Observed float64
}

// LimitReporter is implemented by a plugin that enforces a limit and can
// report when it starts refusing a key. The gateway installs the sink before
// Init and never changes it afterwards; the plugin calls it synchronously from
// Execute, so it must not block — the gateway's sink only enqueues.
type LimitReporter interface {
	SetLimitSink(func(LimitExceeded))
}
