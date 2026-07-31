package cost

import (
	"sync"
	"testing"
)

func TestBudgetSpendsUpToMaxAndStops(t *testing.T) {
	b := NewBudget(3)
	for i := 1; i <= 3; i++ {
		if !b.take() {
			t.Fatalf("take %d refused with %d remaining", i, b.Remaining())
		}
	}
	if b.take() {
		t.Error("took a fourth request from a budget of three")
	}
	if got := b.Spent(); got != 3 {
		t.Errorf("Spent() = %d, want 3 — a refused take must not count as spend", got)
	}
	if got := b.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d, want 0", got)
	}
}

// The property the type exists for. Two passes each honouring
// --cost-max-requests independently would let one run spend twice what the user
// authorized, so the second pass has to see the first pass's spend.
func TestBudgetIsSharedAcrossPasses(t *testing.T) {
	b := NewBudget(5)

	rollup := 0
	for b.Remaining() > 2 && b.take() {
		rollup++
	}
	if rollup != 3 {
		t.Fatalf("first pass took %d, want 3", rollup)
	}
	if got := b.Remaining(); got != 2 {
		t.Fatalf("second pass sees %d remaining, want 2 — it is reading its own counter", got)
	}

	overlay := 0
	for b.take() {
		overlay++
	}
	if overlay != 2 {
		t.Errorf("second pass took %d, want 2", overlay)
	}
	if got := b.Spent(); got != 5 {
		t.Errorf("the run spent %d against a budget of 5", got)
	}
}

// Zero is a real answer: --cost-max-requests is validated at the flag, but a
// budget the rollup exhausted arrives here at zero and must permit nothing
// rather than being read as "unset, so unlimited".
func TestBudgetExhaustedPermitsNothing(t *testing.T) {
	b := NewBudget(1)
	if !b.take() {
		t.Fatal("first take refused")
	}
	if b.take() || b.Remaining() != 0 {
		t.Errorf("an exhausted budget still permits requests: remaining %d", b.Remaining())
	}
}

// A negative max clamps to zero rather than wrapping into a huge allowance —
// the failure mode here spends real money.
func TestBudgetNegativeMaxPermitsNothing(t *testing.T) {
	b := NewBudget(-5)
	if b.take() {
		t.Error("a negative budget authorized a billed request")
	}
	if got := b.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d, want 0", got)
	}
}

// A nil budget is readable but unspendable, so a caller inspecting a pass that
// never ran does not panic and does not read an allowance that does not exist.
func TestNilBudgetReadsAsEmpty(t *testing.T) {
	var b *Budget
	if b.Spent() != 0 || b.Remaining() != 0 {
		t.Errorf("nil budget reads as spent %d / remaining %d, want 0 / 0", b.Spent(), b.Remaining())
	}
}

// Passes are sequential today, but the budget is the one object that would make
// a concurrent one overspend, so the guarantee is pinned rather than assumed
// from the mutex being there.
func TestBudgetNeverOverspendsUnderConcurrency(t *testing.T) {
	const max = 50
	b := NewBudget(max)

	var wg sync.WaitGroup
	var mu sync.Mutex
	taken := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.take() {
				mu.Lock()
				taken++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if taken != max {
		t.Errorf("%d requests authorized against a budget of %d", taken, max)
	}
	if got := b.Spent(); got != max {
		t.Errorf("Spent() = %d, want %d", got, max)
	}
}
