// Package openrouter is a minimal client for OpenRouter's SystemOne API,
// which is how OpenRouter exposes TypeSafe's Jev judgment models
// (typesafe/jev-*).
//
// # Endpoint and wire format (verified against OpenRouter's live docs)
//
// This client targets:
//
//	POST https://openrouter.ai/api/v1/systemone
//
// This was confirmed on 2026-09-26 by fetching OpenRouter's own
// documentation site (openrouter.ai/docs -> API Reference -> SystemOne ->
// "Submit a System One request", rendered at
// /docs/api/api-reference/systemone/submit-a-system-one-request). That page
// documents the request/response shapes below with a worked example using
// model "typesafe/jev-1.13" and a "score" question type. This is a
// dedicated endpoint distinct from the general-purpose
// /api/v1/chat/completions endpoint: it is NOT OpenAI-chat-shaped
// (no "messages" array); it takes "model", "questions", and "state"
// directly. This confirms (rather than the task's speculative alternative)
// that OpenRouter does expose a dedicated System-One/decisions-style
// endpoint for the typesafe/jev-* model family, and that it is the correct
// one to use rather than shoehorning a Jev request into
// /chat/completions. There is also a separate "alpha.decisions" entry in
// OpenRouter's API reference sidebar; it was not fetched because the
// SystemOne page above is explicitly the documented, non-alpha path for
// "a System One model such as Jev" and matches the request/response shape
// referenced in the task brief, so it was used instead of the alpha path.
//
// # What is NOT independently verified
//
// No live call was made against this endpoint (no OPENROUTER_API_KEY was
// available in the implementation environment). Everything in this file is
// transcribed from OpenRouter's published documentation, not confirmed
// against a real response. In particular:
//   - The exact shape of the "score" answer type (fields: type, score,
//     confidence, probabilities, legend) is taken from the documented
//     example response and is assumed stable.
//   - Whether "criteria" for a "score" question may be given as an object
//     (label -> description) as well as an array of labels is not
//     confirmed; only the array form appears in the documented example, so
//     that is the only form this client produces.
//   - Whether non-zero-based scales are natively supported by the
//     "score" question type is not confirmed either way; the documented
//     example only shows a 0-based 3-level scale. This client assumes
//     scores/probabilities returned by OpenRouter for a "score" question
//     are always 0-based array indices into the submitted "criteria" list
//     (consistent with the documented example, where a 3-element criteria
//     array yields probabilities/legend keyed "0","1","2" and a score of
//     1.99), and therefore submits a "criteria" array of length
//     (scale_max-scale_min+1) and re-offsets returned indices/score by
//     scale_min in internal/tools. See internal/tools/score/score.go for
//     the remapping logic and further discussion.
//
// # Generalization beyond "score" (multi-question, "noul"/"choice")
//
// This client was originally score-only (a single Client.Score method).
// It was generalized to send an arbitrary map of named questions of mixed
// types in one call (Client.Ask) once jev-cli grew tools built on the
// "noul" and "choice" question types, and on batching more than one
// question (e.g. one per claim/candidate/item) into a single HTTP round
// trip -- SystemOne's request shape already supported this ("questions"
// is a map, not a single question) even though the original
// jev_score-only client only ever populated one key of it. Client.Score
// is now a thin wrapper around Client.Ask, preserving jev_score's exact
// historical wire format byte-for-byte (see internal/tools/score, which
// required no changes for this generalization).
//
// The "noul" and "choice" response shapes (NoulAnswer, ChoiceAnswer
// below) were supplied as already-verified-live wire facts (confirmed
// against OpenRouter on 2026-09-26, per the task brief that introduced
// jev-cli's second batch of tools) rather than independently re-verified
// here, for the same reason as everything else in this section: no
// OPENROUTER_API_KEY was available in this implementation environment.
// Notably, per that verification, a "noul" answer has NO "confidence" and
// NO "probabilities" field -- just {"type":"noul","noul":<float>} -- unlike
// "choice" and "score", both of which report a "confidence" and a full
// "probabilities" map.
package openrouter

import "encoding/json"

// Endpoint is the OpenRouter SystemOne API endpoint used for all Jev calls.
const Endpoint = "https://openrouter.ai/api/v1/systemone"

// Question is the request shape for a single named SystemOne question, of
// any of the three known types: "noul", "choice", or "score" (see this
// package's doc comment). Criteria's required shape depends on Type:
//   - "noul": object, {"<value>":"<description>"}
//   - "choice": object, {"<option_id>":"<description>"}
//   - "score": array of level-description strings, 0-indexed
//
// This client does not itself validate that shape against Type -- see
// internal/tools/ask, the one tool whose whole job is accepting a
// caller-supplied question map and therefore validating it before
// sending; every other tool package in this codebase builds Criteria
// itself (from its own typed input) and is trusted to build it correctly
// for the Type it sets.
//
// This is a generalization of what was originally a "score"-only,
// []string-criteria-only ScoreQuestion type; encoding an `any` holding a
// []string produces byte-for-byte identical JSON to encoding a concrete
// []string field, so this rename+generalization did not change
// jev_score's wire format (see the package doc comment's "Generalization"
// section).
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"`
}

// Request is the POST body for /api/v1/systemone.
//
// State is typed as `any` because OpenRouter's docs say it accepts "a plain
// string, or a JSON object or array of related context"; most tools in
// this codebase send a plain string, but the field is `any` rather than
// `string` to stay faithful to the documented union type for tools that
// do pass structured state (e.g. jev_ask, which passes through whatever
// shape its caller supplied verbatim).
type Request struct {
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
	State     any                 `json:"state"`
}

// Usage is the token/cost accounting block OpenRouter returns for a
// SystemOne call. Per OpenRouter's docs, `usage` (including `cost`) is a
// *required* field of the SystemOne response — unlike the general
// /chat/completions endpoint, where cost reporting is not always present.
// That is why this package reports usage.cost as the authoritative,
// actual cost of a call rather than computing an estimate from a separate
// per-model pricing table: OpenRouter itself already tells us the real
// billed cost for this endpoint.
type Usage struct {
	Cost         float64 `json:"cost"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
}

// ScoreAnswer is the response shape for a "score"-type answer, per
// OpenRouter's documented example.
type ScoreAnswer struct {
	Type          string             `json:"type"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Legend        map[string]string  `json:"legend"`
}

// NoulAnswer is the response shape for a "noul"-type answer: notably,
// per the task brief's verified-live wire facts (see this package's doc
// comment), NO "confidence" and NO "probabilities" field -- just a single
// float. A confidence-like label, if a tool wants one, is derived
// client-side from how far Noul is from 0.5 (see
// internal/answers.NoulLabel).
type NoulAnswer struct {
	Type string  `json:"type"`
	Noul float64 `json:"noul"`
}

// ChoiceAnswer is the response shape for a "choice"-type answer, per the
// task brief's verified-live wire facts.
type ChoiceAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Response is the top-level SystemOne response envelope. Answers is kept as
// raw JSON per question key because different question types (score,
// choice, noul, ...) have different answer shapes; this client only ever
// parses the "score" key (into ScoreAnswer), done in internal/tools.
type Response struct {
	Answers  map[string]json.RawMessage `json:"answers"`
	ID       string                     `json:"id"`
	Model    string                     `json:"model"`
	Provider string                     `json:"provider"`
	Usage    *Usage                     `json:"usage"`
}

// errorEnvelope is OpenRouter's standard error response shape, documented
// on the "Errors and Debugging" page and matching the error examples shown
// on the SystemOne endpoint's own docs (400/401/402/403/404/413/429/
// 500/502/503/524/529):
//
//	{"error": {"code": number, "message": string, "metadata"?: object}}
type errorEnvelope struct {
	Error struct {
		Code     int             `json:"code"`
		Message  string          `json:"message"`
		Metadata json.RawMessage `json:"metadata,omitempty"`
	} `json:"error"`
}
