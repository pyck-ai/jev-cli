// Package reviewcore is the shared code-review scoring logic used by both
// internal/tools/review (the jev_review tool) and internal/tools/gate (the
// jev_gate tool, which is jev_review plus claim verification against
// caller-supplied evidence, all in one SystemOne call).
//
// The project brief for jev_gate explicitly asked for this logic to be
// shared rather than duplicated, offering two options: gate imports
// review's exported logic/types directly, or both depend on a small shared
// helper package. This package is that second option -- chosen because
// gate needs to fold review's five questions into the SAME single
// client.Ask call as its own claim-verification questions (one HTTP
// request total, per the brief's "batched/composite tools ... one HTTP
// call, many questions" requirement), which is a more natural shape for
// "both tools build their request/response handling from a shared,
// dependency-free helper" than for "gate imports review's own handler and
// somehow injects extra questions into it".
//
// # What is and isn't independently verified
//
// The "score" and "noul" question/answer shapes this package relies on
// (via internal/answers) are the same ones documented in
// internal/openrouter's package doc comment; nothing in this package
// itself was independently re-verified against a live OpenRouter call (no
// OPENROUTER_API_KEY was available in this implementation environment).
package reviewcore

import (
	"encoding/json"

	"github.com/pyck-ai/jev-mcp/internal/answers"
	"github.com/pyck-ai/jev-mcp/internal/capstring"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
)

// Hard caps on Request/Diff/Tests, in runes (see internal/capstring),
// named verbatim in the project brief for both jev_review and jev_gate
// ("each capped 50,000 chars").
const (
	MaxRequestChars = 50_000
	MaxDiffChars    = 50_000
	MaxTestsChars   = 50_000
)

// Default thresholds, both named "default 0.8"/"default 0.7" in the
// project brief. Both are exposed as optional input fields by
// internal/tools/review and internal/tools/gate (see answers.ResolveThreshold
// for why "optional input field, default applies at the zero value" is
// this codebase's consistent reading of "default 0.NN" throughout the
// brief for this whole batch of tools).
const (
	DefaultAutoAccept     = 0.8
	DefaultCompositeFloor = 0.7
)

// Question keys used both in the SystemOne request's "questions" map and
// (unchanged) as this package's own answers-map lookup keys.
const (
	QuestionCorrectness = "correctness"
	QuestionSpecMatch   = "spec_match"
	QuestionTestGap     = "test_gap"
	QuestionBlastRadius = "blast_radius"
	QuestionSafeToApply = "safe_to_apply"
)

// Status values for RubricResult.Status and SafeToApplyResult.Status.
const (
	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"
)

// Action values for Assessment.Action.
const (
	ActionAuto     = "auto"
	ActionReview   = "review"
	ActionEscalate = "escalate"
)

// Weights controls how much each of the four "score"-typed rubrics
// contributes to Assessment.Composite. The project brief calls these
// "default weights", so -- consistent with every other threshold in this
// batch of tools -- they are caller-overridable; see Weights.Normalize.
type Weights struct {
	Correctness float64 `json:"correctness,omitempty"`
	SpecMatch   float64 `json:"spec_match,omitempty"`
	TestGap     float64 `json:"test_gap,omitempty"`
	BlastRadius float64 `json:"blast_radius,omitempty"`
}

// DefaultWeights returns the project brief's literal defaults:
// correctness 0.4, spec_match 0.3, test_gap 0.15, blast_radius 0.15.
func DefaultWeights() Weights {
	return Weights{Correctness: 0.4, SpecMatch: 0.3, TestGap: 0.15, BlastRadius: 0.15}
}

// Normalize returns w scaled so its four fields sum to 1 (so Composite
// stays meaningfully comparable to CompositeFloor's [0,1] range regardless
// of what a caller passes), or DefaultWeights() if w is the zero value or
// all-non-positive (nothing meaningful to scale).
func (w Weights) Normalize() Weights {
	sum := w.Correctness + w.SpecMatch + w.TestGap + w.BlastRadius
	if sum <= 0 {
		return DefaultWeights()
	}
	return Weights{
		Correctness: w.Correctness / sum,
		SpecMatch:   w.SpecMatch / sum,
		TestGap:     w.TestGap / sum,
		BlastRadius: w.BlastRadius / sum,
	}
}

// Input is the truncated, ready-to-send form of a review's three text
// fields (see Prepare).
type Input struct {
	Request string
	Diff    string
	Tests   string
}

// Prepare truncates request/diff/tests to their hard caps (see
// Max*Chars) and reports whether any of the three actually got cut --
// which, per the project brief, "forces truncated: true and blocks auto"
// (see ParseAssessment).
func Prepare(request, diff, tests string) (in Input, truncated bool) {
	var reqCut, diffCut, testsCut bool
	in.Request, reqCut = capstring.Truncate(request, MaxRequestChars)
	in.Diff, diffCut = capstring.Truncate(diff, MaxDiffChars)
	in.Tests, testsCut = capstring.Truncate(tests, MaxTestsChars)
	return in, reqCut || diffCut || testsCut
}

// State builds the SystemOne "state" value shared by all five of
// Questions()'s questions: a single object carrying request/diff/tests,
// so every rubric judges the same fixed context. tests is omitted from the
// object entirely when empty (it's an optional field throughout this
// codebase's review-shaped tools).
func State(in Input) any {
	m := map[string]any{"request": in.Request, "diff": in.Diff}
	if in.Tests != "" {
		m["tests"] = in.Tests
	}
	return m
}

// levelCriteria is the shared 0/1/2 "score" criteria shape: a 3-element
// array of level descriptions, per internal/openrouter's documented
// "score" criteria shape.
func levelCriteria(level0, level1, level2 string) []string {
	return []string{level0, level1, level2}
}

// Questions returns the five named SystemOne questions (four "score",
// one "noul") that make up a review assessment, keyed by the Question*
// constants above. Callers (internal/tools/review, internal/tools/gate)
// merge this map with any additional questions of their own (gate adds
// one "choice" question per claim) before making a single client.Ask call
// -- see this package's doc comment.
func Questions() map[string]openrouter.Question {
	return map[string]openrouter.Question{
		QuestionCorrectness: {
			Type: "score",
			Instructions: "Rate how correctly the diff implements what the request asked for, on the given " +
				"0-2 scale, considering the request, diff, and tests together.",
			Criteria: levelCriteria(
				"Incorrect: the diff does not do what the request asked, or does something substantially different/wrong.",
				"Partially correct: the diff does some but not all of what was asked, or is correct but incomplete.",
				"Fully correct: the diff fully and correctly implements what the request asked for.",
			),
		},
		QuestionSpecMatch: {
			Type: "score",
			Instructions: "Rate how closely the diff matches the specification/requirements described in the " +
				"request, on the given 0-2 scale, considering the request, diff, and tests together.",
			Criteria: levelCriteria(
				"Does not match: the diff does not follow the specification/requirements described in the request.",
				"Partially matches: some aspects of the diff follow the specification, others deviate from it.",
				"Fully matches: the diff fully follows the specification/requirements described in the request.",
			),
		},
		QuestionTestGap: {
			Type: "score",
			Instructions: "Rate how large the gap is between what the diff changed and what the tests actually " +
				"cover, on the given 0-2 scale (0 = no gap, 2 = large gap), considering the request, diff, and " +
				"tests together.",
			Criteria: levelCriteria(
				"No meaningful test gap: tests (if any) fully cover the changed behavior.",
				"Partial test gap: tests exist but some changed behavior is untested.",
				"Large test gap: little or no testing covers the changed behavior.",
			),
		},
		QuestionBlastRadius: {
			Type: "score",
			Instructions: "Rate how large this diff's blast radius is, on the given 0-2 scale (0 = small, " +
				"2 = large), considering the request, diff, and tests together.",
			Criteria: levelCriteria(
				"Small blast radius: the change is well-contained, unlikely to affect anything beyond its immediate target.",
				"Moderate blast radius: the change could plausibly affect adjacent behavior or callers.",
				"Large blast radius: the change touches shared/critical code paths or could affect many callers.",
			),
		},
		QuestionSafeToApply: {
			Type: "noul",
			Instructions: "Considering the request, diff, and tests together, would it be safe to apply/merge " +
				"this diff as-is, without further human review?",
			Criteria: map[string]string{
				"true":  "it would be safe to apply/merge this diff as-is",
				"false": "it would NOT be safe to apply/merge this diff as-is",
			},
		},
	}
}

// RubricResult is one "score"-typed rubric's parsed result. Status is
// StatusInvalidResponse (never a fabricated score) when that rubric's
// answer was missing or malformed; Score/Confidence/Probabilities are
// zero-valued placeholders in that case.
type RubricResult struct {
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Status        string             `json:"status"`
}

// SafeToApplyResult is the parsed "safe_to_apply" noul answer.
// Probability is the raw noul value; Label is NoulLabel(Probability,
// autoAccept) ("likely"/"unlikely"/"uncertain" -- see
// internal/answers.NoulLabel), which is what ParseAssessment actually
// gates the "safe" boolean on (see that function's doc comment).
type SafeToApplyResult struct {
	Probability float64 `json:"probability"`
	Label       string  `json:"label"`
	Status      string  `json:"status"`
}

// Assessment is a full review judgment: the four rubrics, the
// safe-to-apply signal, the weighted composite, and the derived
// auto/review/escalate action.
type Assessment struct {
	Correctness RubricResult      `json:"correctness"`
	SpecMatch   RubricResult      `json:"spec_match"`
	TestGap     RubricResult      `json:"test_gap"`
	BlastRadius RubricResult      `json:"blast_radius"`
	SafeToApply SafeToApplyResult `json:"safe_to_apply"`
	// Composite is the weighted composite "goodness" score in [0,1] (see
	// ParseAssessment): higher is better, regardless of which raw rubrics
	// were inverted to get there.
	Composite float64 `json:"composite"`
	// Truncated is true if any of Request/Diff/Tests exceeded its cap
	// (see Prepare) -- which, per the project brief, blocks Action ==
	// ActionAuto regardless of how good Composite is.
	Truncated bool `json:"truncated,omitempty"`
	// Action is one of ActionAuto, ActionReview, or ActionEscalate -- see
	// ParseAssessment's doc comment for the exact decision rule.
	Action string `json:"action"`
	// ReasonCodes explains why Action is what it is: e.g.
	// "low_confidence:correctness", "invalid_response:safe_to_apply",
	// "unsafe_to_apply", "composite_below_floor", "truncated_input". Not
	// an exhaustive documented enum -- these are diagnostic strings for a
	// human/log reader, not a contract callers should switch on.
	ReasonCodes []string `json:"reason_codes,omitempty"`
}

// ItemCount is the number of named questions a review assessment always
// asks (the four rubrics plus safe_to_apply), for audit.Entry.ItemCount
// bookkeeping by callers.
func (a Assessment) ItemCount() int { return 5 }

// InvalidCount is how many of those 5 questions came back
// StatusInvalidResponse, for audit.Entry.InvalidCount bookkeeping by
// callers.
func (a Assessment) InvalidCount() int {
	n := 0
	for _, status := range [...]string{
		a.Correctness.Status, a.SpecMatch.Status, a.TestGap.Status, a.BlastRadius.Status, a.SafeToApply.Status,
	} {
		if status != StatusOK {
			n++
		}
	}
	return n
}

func parseRubric(answersMap map[string]json.RawMessage, key string) RubricResult {
	raw, ok := answersMap[key]
	if !ok {
		return RubricResult{Status: StatusInvalidResponse}
	}
	score, confidence, probs, valid := answers.Score(raw, 3)
	if !valid {
		return RubricResult{Status: StatusInvalidResponse}
	}
	return RubricResult{Score: score, Confidence: confidence, Probabilities: probs, Status: StatusOK}
}

// goodness maps a RubricResult's raw [0,2] Score onto a [0,1] "higher is
// better" scale, inverting when invert is true (for test_gap/blast_radius,
// whose raw scale runs the opposite direction -- see the project brief:
// "inverting test_gap/blast_radius since higher is worse for those two
// before weighting"). A StatusInvalidResponse rubric contributes 0 (the
// worst possible goodness) rather than being excluded from the weighted
// sum or treated as a neutral midpoint: an unusable rubric must never
// silently improve Composite, since Action's "auto" branch is exactly the
// branch this is trying to keep untrustworthy data out of.
func goodness(r RubricResult, invert bool) float64 {
	if r.Status != StatusOK {
		return 0
	}
	g := r.Score / 2
	if invert {
		g = 1 - g
	}
	return g
}

// ParseAssessment parses a SystemOne response's Answers map (see
// Questions, which built the request these answers correspond to) into a
// full Assessment.
//
// autoAccept and compositeFloor are resolved against DefaultAutoAccept/
// DefaultCompositeFloor via answers.ResolveThreshold (0 means "use the
// default"); weights is resolved against DefaultWeights via
// Weights.Normalize. Callers are responsible for validating any
// caller-supplied override before calling this (see
// answers.ValidateAutoAccept), exactly as every other threshold-accepting
// tool in this codebase does.
//
// # Action decision rule (verbatim from the project brief)
//
// safe: SafeToApply.Label == "likely" (i.e. the noul value cleared the
// SAME autoAccept confidence bar used for the four rubrics -- "noul" has
// no separate confidence field to check, see internal/openrouter's
// package doc comment, so this is this implementation's chosen way to
// require safe_to_apply be answered *confidently* true, not just >0.5;
// documented here as an interpretation, not a verified wire fact).
// allConfident: every one of the four rubrics is StatusOK AND has
// Confidence >= autoAccept (a StatusInvalidResponse rubric counts as
// "not confident", per goodness's doc comment, so it cannot silently
// satisfy this either).
//
//   - Action = ActionAuto if safe && allConfident && Composite >= compositeFloor.
//   - Action = ActionReview if Composite < compositeFloor but safe && allConfident
//     ("review if composite is below floor but nothing else failed").
//   - Action = ActionEscalate otherwise (safe or allConfident failed, regardless
//     of Composite).
//   - Finally, regardless of the above, truncated forces Action away from
//     ActionAuto down to ActionReview (never up to ActionEscalate: a
//     truncated-but-otherwise-healthy review just needs a second look, it
//     isn't necessarily alarming) -- "truncated input ... blocks auto".
func ParseAssessment(answersMap map[string]json.RawMessage, truncated bool, autoAccept, compositeFloor float64, weights Weights) Assessment {
	autoAccept = answers.ResolveThreshold(autoAccept, DefaultAutoAccept)
	compositeFloor = answers.ResolveThreshold(compositeFloor, DefaultCompositeFloor)
	weights = weights.Normalize()

	a := Assessment{
		Correctness: parseRubric(answersMap, QuestionCorrectness),
		SpecMatch:   parseRubric(answersMap, QuestionSpecMatch),
		TestGap:     parseRubric(answersMap, QuestionTestGap),
		BlastRadius: parseRubric(answersMap, QuestionBlastRadius),
		Truncated:   truncated,
	}

	if raw, ok := answersMap[QuestionSafeToApply]; ok {
		if v, ok2 := answers.Noul(raw); ok2 {
			a.SafeToApply = SafeToApplyResult{Probability: v, Label: answers.NoulLabel(v, autoAccept), Status: StatusOK}
		} else {
			a.SafeToApply = SafeToApplyResult{Status: StatusInvalidResponse}
		}
	} else {
		a.SafeToApply = SafeToApplyResult{Status: StatusInvalidResponse}
	}

	a.Composite = weights.Correctness*goodness(a.Correctness, false) +
		weights.SpecMatch*goodness(a.SpecMatch, false) +
		weights.TestGap*goodness(a.TestGap, true) +
		weights.BlastRadius*goodness(a.BlastRadius, true)

	var reasons []string
	allConfident := true
	checkRubric := func(name string, r RubricResult) {
		switch {
		case r.Status != StatusOK:
			reasons = append(reasons, "invalid_response:"+name)
			allConfident = false
		case r.Confidence < autoAccept:
			reasons = append(reasons, "low_confidence:"+name)
			allConfident = false
		}
	}
	checkRubric(QuestionCorrectness, a.Correctness)
	checkRubric(QuestionSpecMatch, a.SpecMatch)
	checkRubric(QuestionTestGap, a.TestGap)
	checkRubric(QuestionBlastRadius, a.BlastRadius)

	safe := false
	switch {
	case a.SafeToApply.Status != StatusOK:
		reasons = append(reasons, "invalid_response:safe_to_apply")
	case a.SafeToApply.Label == "likely":
		safe = true
	default:
		reasons = append(reasons, "unsafe_to_apply")
	}

	if a.Composite < compositeFloor {
		reasons = append(reasons, "composite_below_floor")
	}
	if truncated {
		reasons = append(reasons, "truncated_input")
	}

	switch {
	case safe && allConfident && a.Composite >= compositeFloor:
		a.Action = ActionAuto
	case safe && allConfident:
		a.Action = ActionReview // composite < compositeFloor is the only remaining possibility here
	default:
		a.Action = ActionEscalate
	}
	if truncated && a.Action == ActionAuto {
		a.Action = ActionReview
	}

	a.ReasonCodes = reasons
	return a
}
