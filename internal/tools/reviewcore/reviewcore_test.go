package reviewcore

import (
	"encoding/json"
	"testing"
)

func goodAnswers() map[string]json.RawMessage {
	// All four rubrics score "2" (best) with high confidence; safe_to_apply
	// is confidently true.
	best := `{"type":"score","score":2,"confidence":0.95,"probabilities":{"0":0.02,"1":0.03,"2":0.95}}`
	return map[string]json.RawMessage{
		QuestionCorrectness: json.RawMessage(best),
		QuestionSpecMatch:   json.RawMessage(best),
		QuestionTestGap:     json.RawMessage(`{"type":"score","score":0,"confidence":0.95,"probabilities":{"0":0.95,"1":0.03,"2":0.02}}`),
		QuestionBlastRadius: json.RawMessage(`{"type":"score","score":0,"confidence":0.95,"probabilities":{"0":0.95,"1":0.03,"2":0.02}}`),
		QuestionSafeToApply: json.RawMessage(`{"type":"noul","noul":0.97}`),
	}
}

func TestParseAssessment_AllGood_Auto(t *testing.T) {
	a := ParseAssessment(goodAnswers(), false, 0, 0, Weights{})
	if a.Action != ActionAuto {
		t.Fatalf("Action = %q, want %q (reasons: %v)", a.Action, ActionAuto, a.ReasonCodes)
	}
	if a.Composite < 0.9 {
		t.Errorf("Composite = %v, want close to 1.0 for all-best answers", a.Composite)
	}
	if len(a.ReasonCodes) != 0 {
		t.Errorf("expected no reason codes for a fully healthy assessment, got %v", a.ReasonCodes)
	}
	if a.InvalidCount() != 0 || a.ItemCount() != 5 {
		t.Errorf("ItemCount/InvalidCount = %d/%d, want 5/0", a.ItemCount(), a.InvalidCount())
	}
}

func TestParseAssessment_TruncatedBlocksAuto(t *testing.T) {
	a := ParseAssessment(goodAnswers(), true, 0, 0, Weights{})
	if a.Action != ActionReview {
		t.Fatalf("Action = %q, want %q for a truncated-but-otherwise-healthy review", a.Action, ActionReview)
	}
	found := false
	for _, r := range a.ReasonCodes {
		if r == "truncated_input" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected \"truncated_input\" reason code, got %v", a.ReasonCodes)
	}
}

func TestParseAssessment_LowConfidenceEscalates(t *testing.T) {
	answersMap := goodAnswers()
	// Low-confidence correctness answer.
	answersMap[QuestionCorrectness] = json.RawMessage(`{"type":"score","score":2,"confidence":0.5,"probabilities":{"0":0.3,"1":0.2,"2":0.5}}`)
	a := ParseAssessment(answersMap, false, 0, 0, Weights{})
	if a.Action != ActionEscalate {
		t.Fatalf("Action = %q, want %q (reasons: %v)", a.Action, ActionEscalate, a.ReasonCodes)
	}
}

func TestParseAssessment_InvalidRubricEscalates(t *testing.T) {
	answersMap := goodAnswers()
	delete(answersMap, QuestionBlastRadius)
	a := ParseAssessment(answersMap, false, 0, 0, Weights{})
	if a.BlastRadius.Status != StatusInvalidResponse {
		t.Fatalf("BlastRadius.Status = %q, want %q", a.BlastRadius.Status, StatusInvalidResponse)
	}
	if a.Action != ActionEscalate {
		t.Fatalf("Action = %q, want %q (reasons: %v)", a.Action, ActionEscalate, a.ReasonCodes)
	}
	if a.InvalidCount() != 1 {
		t.Errorf("InvalidCount() = %d, want 1", a.InvalidCount())
	}
}

func TestParseAssessment_LowCompositeAloneIsReviewNotEscalate(t *testing.T) {
	// Confident but mediocre (score=1) across the board -> composite ~0.5,
	// below the 0.7 default floor, but nothing else failed.
	mediocre := `{"type":"score","score":1,"confidence":0.9,"probabilities":{"0":0.05,"1":0.9,"2":0.05}}`
	answersMap := map[string]json.RawMessage{
		QuestionCorrectness: json.RawMessage(mediocre),
		QuestionSpecMatch:   json.RawMessage(mediocre),
		QuestionTestGap:     json.RawMessage(mediocre),
		QuestionBlastRadius: json.RawMessage(mediocre),
		QuestionSafeToApply: json.RawMessage(`{"type":"noul","noul":0.9}`),
	}
	a := ParseAssessment(answersMap, false, 0, 0, Weights{})
	if a.Composite >= DefaultCompositeFloor {
		t.Fatalf("test setup: expected composite below floor, got %v", a.Composite)
	}
	if a.Action != ActionReview {
		t.Fatalf("Action = %q, want %q (reasons: %v)", a.Action, ActionReview, a.ReasonCodes)
	}
}

func TestParseAssessment_UnsafeToApplyEscalates(t *testing.T) {
	answersMap := goodAnswers()
	answersMap[QuestionSafeToApply] = json.RawMessage(`{"type":"noul","noul":0.05}`) // confidently UNsafe
	a := ParseAssessment(answersMap, false, 0, 0, Weights{})
	if a.SafeToApply.Label != "unlikely" {
		t.Fatalf("SafeToApply.Label = %q, want unlikely", a.SafeToApply.Label)
	}
	if a.Action != ActionEscalate {
		t.Fatalf("Action = %q, want %q (reasons: %v)", a.Action, ActionEscalate, a.ReasonCodes)
	}
}

func TestWeights_Normalize(t *testing.T) {
	w := Weights{Correctness: 2, SpecMatch: 2, TestGap: 0, BlastRadius: 0}.Normalize()
	if w.Correctness != 0.5 || w.SpecMatch != 0.5 || w.TestGap != 0 || w.BlastRadius != 0 {
		t.Errorf("Normalize() = %+v, want 0.5/0.5/0/0", w)
	}

	zero := Weights{}.Normalize()
	if zero != DefaultWeights() {
		t.Errorf("Normalize() of zero-value Weights = %+v, want DefaultWeights() %+v", zero, DefaultWeights())
	}
}
