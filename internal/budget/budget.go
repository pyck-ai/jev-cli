// Package budget tracks cumulative USD spend for the lifetime of one
// jev-mcp server process ("session") and enforces the session-level cap
// from config.Budget.MaxUSDPerSession.
//
// # Why only the session cap is enforced pre-call
//
// The project brief asks for both a per-call cap (max_usd_per_call) and a
// session cap (max_usd_per_session) to be enforced either by refusing the
// call before it is sent (if cost is predictable) or by rejecting/flagging
// the result after the fact (if cost is only knowable post-hoc) — whichever
// is actually feasible.
//
// Per OpenRouter's documented response shape for the SystemOne endpoint
// (see internal/openrouter), the cost of a given call is only known once
// that call's response has been received (usage.cost is a response field;
// nothing in OpenRouter's docs exposes a pre-call price quote for a
// SystemOne/Jev request, and estimating it ourselves would require
// guessing both a token count for arbitrary caller-supplied `state` text
// and this model's per-token pricing, neither of which we could verify).
// So:
//
//   - max_usd_per_session IS enforced pre-call, because by the time a new
//     call is about to be made we know the *exact* cost of every previous
//     call in the session (summed here). If that running total already
//     meets or exceeds the cap, the new call is refused before it is sent
//     (see Tracker.SessionBudgetExceeded), and no cost is incurred.
//   - max_usd_per_call CANNOT be enforced pre-call for the reason above, so
//     it is enforced post-hoc: internal/tools compares the actual
//     usage.cost of a completed call against the cap and sets
//     ScoreOutput.BudgetExceeded accordingly. The call has already been
//     paid for by that point, so the result is flagged rather than
//     discarded — throwing away a judgment we already paid for would waste
//     the spend without helping the budget.
//
// The running total is in-memory only and resets when the process
// restarts; it is not persisted across server restarts.
package budget

import "sync"

// Tracker accumulates spend across calls made by this process.
type Tracker struct {
	mu               sync.Mutex
	total            float64
	maxUSDPerSession float64 // <= 0 means unlimited
}

// NewTracker creates a Tracker enforcing the given session cap.
func NewTracker(maxUSDPerSession float64) *Tracker {
	return &Tracker{maxUSDPerSession: maxUSDPerSession}
}

// SessionBudgetExceeded reports whether cumulative spend so far has already
// reached or exceeded the configured session cap. Call this BEFORE making a
// new API call; it does not account for the (unknown) cost of the call
// about to be made, only for calls already completed.
func (t *Tracker) SessionBudgetExceeded() bool {
	if t.maxUSDPerSession <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total >= t.maxUSDPerSession
}

// Add records the actual cost of a completed call.
func (t *Tracker) Add(costUSD float64) {
	if costUSD <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += costUSD
}

// Total returns cumulative spend recorded so far.
func (t *Tracker) Total() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total
}
