// Package answers implements shared parsing/validation helpers for
// OpenRouter SystemOne answers -- the "noul", "choice", and "score"
// question/answer types documented in internal/openrouter's package doc
// comment -- reused across every tool package under internal/tools/ so
// each one does not have to reimplement, and potentially get subtly
// wrong, the same fail-closed validation rules.
//
// Every parser here returns ok=false (never partial or fabricated data)
// on any malformed input: a wrong/missing "type" field, JSON that doesn't
// parse, a missing or out-of-[0,1]-range probability, or (for "score")
// probabilities that don't sum to ~1. Every tool built on this package
// maps ok=false to its own "invalid_response"-style status for that
// item/question, per the project's fail-closed requirement -- see each
// tool package's own doc comment for specifics.
//
// # Provenance of the per-type wire shapes
//
// The "score" shape and its validation rules (probabilities must cover
// every expected 0-based index and sum to ~1, tolerance 0.01) are exactly
// what internal/tools/score's jev_score implementation already validated
// before this package existed -- extracted here, not re-derived, so
// jev_score and every other score-typed question in this codebase share
// one implementation instead of two independently-maintained copies (see
// internal/tools/score's parseScoreAnswer, which now delegates its core
// validation to Score below).
//
// The "noul" and "choice" shapes were supplied as already-verified-live
// wire facts (confirmed against OpenRouter on 2026-09-26, per the task
// brief that introduced this package) rather than independently
// re-verified in this implementation environment, which has no
// OPENROUTER_API_KEY available -- consistent with internal/openrouter's
// own package doc comment's practice of saying explicitly what was and
// wasn't independently confirmed here.
package answers

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

// probTolerance is the slack applied to individual probability values
// (accepted range [-probTolerance, 1+probTolerance]) and, for "score"
// answers, to how close the full distribution must sum to 1
// (|sum-1| <= probTolerance*10, i.e. 0.01 for a 0.001 per-value
// tolerance) -- unchanged from the tolerance internal/tools/score's
// parseScoreAnswer already used before this package existed.
const probTolerance = 0.001

// scoreSumTolerance is the tolerance applied to how close a "score"
// answer's probabilities must sum to 1.0, matching
// internal/tools/score's original, already-tested tolerance.
const scoreSumTolerance = 0.01

func probInRange(v float64) bool {
	return v >= -probTolerance && v <= 1+probTolerance
}

// Noul parses a "noul"-type answer. Per the task brief's verified-live
// wire facts, the response shape is exactly
// {"type":"noul","noul":<float 0..1>} -- deliberately no "confidence" and
// no "probabilities" field (unlike "choice" and "score"). ok is false if
// the JSON doesn't parse, "type" isn't "noul", or "noul" is outside the
// tolerant [-0.001, 1.001] range.
func Noul(raw json.RawMessage) (value float64, ok bool) {
	var ans openrouter.NoulAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return 0, false
	}
	if ans.Type != "noul" {
		return 0, false
	}
	if !probInRange(ans.Noul) {
		return 0, false
	}
	return ans.Noul, true
}

// NoulLabel classifies a validated noul probability into "likely"
// (value >= autoAccept), "unlikely" (value <= 1-autoAccept), or
// "uncertain" (otherwise) -- per jkudish's jev_noul docs cited in the
// task brief. autoAccept must be > 0.5 for "likely"/"unlikely" to be
// disjoint and cover a non-empty range; callers are responsible for
// validating/defaulting autoAccept themselves (see e.g.
// internal/tools/check.validateInput).
func NoulLabel(value, autoAccept float64) string {
	switch {
	case value >= autoAccept:
		return "likely"
	case value <= 1-autoAccept:
		return "unlikely"
	default:
		return "uncertain"
	}
}

// Choice parses a "choice"-type answer, additionally checking that the
// returned Choice and every key of Probabilities are members of
// validOptions (the exact option-id set this question's criteria
// offered): a choice answer naming an option that was never offered is
// treated the same as a malformed answer, fail-closed.
//
// Unlike Score, Choice does NOT require every validOptions member to be
// present in Probabilities, and does not require Probabilities to sum to
// 1: neither is documented as guaranteed for "choice" answers (only
// "score" answers are documented as a full distribution that sums to 1,
// per the worked example the task brief cites), so this package does not
// invent a stricter rule than what was actually specified.
func Choice(raw json.RawMessage, validOptions map[string]bool) (choice string, confidence float64, probabilities map[string]float64, ok bool) {
	var ans openrouter.ChoiceAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return "", 0, nil, false
	}
	if ans.Type != "choice" {
		return "", 0, nil, false
	}
	if !validOptions[ans.Choice] {
		return "", 0, nil, false
	}
	for k, v := range ans.Probabilities {
		if !validOptions[k] || !probInRange(v) {
			return "", 0, nil, false
		}
	}
	return ans.Choice, ans.Confidence, ans.Probabilities, true
}

// Score parses a "score"-type answer against exactly levels 0-based
// criteria entries (indices "0".."levels-1"), returning the raw 0-based
// score/confidence/probabilities. Every index in [0,levels) must be
// present in probabilities and in [-0.001,1.001], and the probabilities
// must sum to ~1 (tolerance 0.01) -- exactly the validation
// internal/tools/score's jev_score has always applied.
//
// Callers needing a non-zero-based scale (jev_score itself: an arbitrary
// caller-supplied [scale_min, scale_max]) remap the returned 0-based
// score/probabilities afterwards; this function has no opinion about
// that remapping (see internal/tools/score.parseScoreAnswer).
func Score(raw json.RawMessage, levels int) (score, confidence float64, probabilities map[string]float64, ok bool) {
	var ans openrouter.ScoreAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return 0, 0, nil, false
	}
	if ans.Type != "score" {
		return 0, 0, nil, false
	}

	probabilities = make(map[string]float64, levels)
	sum := 0.0
	for i := range levels {
		v, present := ans.Probabilities[strconv.Itoa(i)]
		if !present || !probInRange(v) {
			return 0, 0, nil, false
		}
		probabilities[strconv.Itoa(i)] = v
		sum += v
	}
	if math.Abs(sum-1.0) > scoreSumTolerance {
		return 0, 0, nil, false
	}
	return ans.Score, ans.Confidence, probabilities, true
}

// ResolveThreshold returns v if v > 0, else def. Added for the second batch
// of tools (jev_verify, jev_screen, jev_check, jev_classify, jev_compare,
// jev_review, jev_gate, jev_extract), every one of which exposes at least
// one "default 0.NN"-style threshold (auto_accept, composite_floor,
// block_at, review_at, minimum_margin, ...) as an OPTIONAL input field: per
// the project brief's own description of jkudish's jev_noul auto_accept as
// "a configurable auto_accept threshold (e.g. auto_accept=0.85 default...)",
// every one of these fields is designed to be caller-overridable, with the
// documented default applying whenever the field is omitted or left at its
// Go zero value. Since none of these thresholds is ever meaningfully 0 or
// negative (see ValidateAutoAccept and each tool's own input validation for
// the tighter, threshold-specific bounds actually enforced), "v > 0" is an
// unambiguous "was this field set at all" test with no separate sentinel
// needed.
func ResolveThreshold(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

// ValidateAutoAccept validates a caller-supplied auto_accept-style
// threshold override before it's resolved against its default: 0 (meaning
// "use the default", see ResolveThreshold) is always fine; any explicit
// non-zero value must be strictly greater than 0.5 and at most 1.
//
// The lower bound is stated verbatim in the project brief for jev_check's
// auto_accept ("must be > 0.5"); every other tool in this codebase that
// exposes an auto_accept-style field applies the same rule for consistency
// even where the brief didn't repeat it verbatim for that specific tool --
// a bar at or below 50% confidence cannot meaningfully separate "confident
// enough to act on" from "not", and would make NoulLabel's "likely"
// (value >= autoAccept) and "unlikely" (value <= 1-autoAccept) branches
// overlap or cover the whole [0,1] range. name is the field name to use in
// the returned error message (e.g. "auto_accept", "composite_floor").
func ValidateAutoAccept(name string, v float64) error {
	if v == 0 {
		return nil
	}
	if v <= 0.5 || v > 1 {
		return fmt.Errorf("%s must be > 0.5 and <= 1 if set, got %v", name, v)
	}
	return nil
}
