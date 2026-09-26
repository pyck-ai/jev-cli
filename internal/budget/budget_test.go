package budget

import "testing"

func TestSessionBudgetExceeded_PositiveCap(t *testing.T) {
	tr := NewTracker(1.0)
	if tr.SessionBudgetExceeded() {
		t.Fatal("fresh tracker with no spend must not report exceeded")
	}
	tr.Add(0.5)
	if tr.SessionBudgetExceeded() {
		t.Fatal("spend below cap must not report exceeded")
	}
	tr.Add(0.5) // total == cap exactly
	if !tr.SessionBudgetExceeded() {
		t.Fatal("spend >= cap must report exceeded (threshold is inclusive)")
	}
	tr.Add(10) // well past cap
	if !tr.SessionBudgetExceeded() {
		t.Fatal("spend well past cap must report exceeded")
	}
	if got := tr.Total(); got != 11.0 {
		t.Errorf("Total() = %v, want 11.0", got)
	}
}

func TestSessionBudgetExceeded_ZeroOrNegativeCapMeansUnlimited(t *testing.T) {
	for _, cap := range []float64{0, -1, -100} {
		tr := NewTracker(cap)
		tr.Add(1_000_000)
		if tr.SessionBudgetExceeded() {
			t.Errorf("cap=%v: expected <= 0 cap to mean unlimited, but SessionBudgetExceeded() = true after huge spend", cap)
		}
	}
}

func TestAdd_IgnoresNonPositiveCost(t *testing.T) {
	tr := NewTracker(1.0)
	tr.Add(0)
	tr.Add(-5)
	if got := tr.Total(); got != 0 {
		t.Errorf("Total() = %v, want 0 after adding only non-positive costs", got)
	}
}
