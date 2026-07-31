package cost

import "sync"

// Budget is the run's allowance of billed Cost Explorer requests, shared by
// every pass that spends against it.
//
// It exists because there is more than one paid pass now — the account rollup
// and the resource-level overlay — and the user set one number. Two independent
// counters, each honouring "--cost-max-requests", would let a run spend twice
// what was authorized. So the ceiling lives here, in one object the caller
// creates once and hands to each pass in turn.
//
// The order that hand-off happens in is the policy: the rollup runs first and
// takes what it needs, the overlay gets what is left. That is deliberate. The
// rollup is the census's account-level truth and its cost is bounded and known;
// the overlay's cost scales with how many services reported spend, and it
// degrades honestly when the budget runs out — an unprobed service is recorded
// as unprobed. Starving the rollup to probe services would trade a complete
// answer for a partial one.
//
// take is unexported on purpose: only this package may spend the user's money,
// so a caller can read the meter but cannot advance it.
type Budget struct {
	mu    sync.Mutex
	max   int
	spent int
}

// NewBudget returns a budget permitting max billed requests. A max below zero
// is clamped to zero, which permits nothing rather than everything.
func NewBudget(max int) *Budget {
	if max < 0 {
		max = 0
	}
	return &Budget{max: max}
}

// Spent reports how many billed requests the run has issued so far, across
// every pass.
func (b *Budget) Spent() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// Remaining reports how many billed requests the run may still issue.
func (b *Budget) Remaining() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max - b.spent
}

// take claims one request, reporting whether the budget allowed it.
func (b *Budget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spent >= b.max {
		return false
	}
	b.spent++
	return true
}
