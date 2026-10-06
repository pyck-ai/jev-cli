package main

import (
	"fmt"
	"sort"
)

// Verdicts, in decreasing order of goodness.
const (
	VerdictIdeal      = "ideal"      // chosen == case.ideal
	VerdictAcceptable = "acceptable" // chosen in case.acceptable, not ideal
	VerdictEscape     = "escape"     // chose ask although ask is not acceptable
	VerdictNone       = "none"       // no tool call
	VerdictWrong      = "wrong"      // any other tool (or an unknown name)
	VerdictError      = "error"      // request failed; excluded from rates
	// VerdictTruncated: no tool call and finish_reason "length" (max_tokens hit
	// before the model acted). Reported as trunc%, excluded from the
	// ideal/accept/escape/none denominators (n_effective).
	VerdictTruncated = "truncated"
)

// verdictFor scores a bare tool name ("none" for no call) against a case.
func verdictFor(c Case, chosen string) string {
	if chosen == "" {
		chosen = "none"
	}
	switch {
	case chosen == c.Ideal:
		return VerdictIdeal
	case chosen == "none":
		return VerdictNone
	case c.accepts(chosen):
		return VerdictAcceptable
	case chosen == "ask":
		return VerdictEscape
	default:
		return VerdictWrong
	}
}

// validateArgs checks args against a tool's input schema in the simple way the
// task asks for: required keys present and top-level property types right.
// It returns human-readable problems; empty means valid.
func validateArgs(schema map[string]any, args map[string]any) []string {
	var problems []string
	if args == nil {
		return []string{"arguments are not a JSON object"}
	}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if k, ok := r.(string); ok {
				if _, present := args[k]; !present {
					problems = append(problems, "missing required "+k)
				}
			}
		}
	}
	props, _ := schema["properties"].(map[string]any)
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p, ok := props[k].(map[string]any)
		if !ok {
			continue
		}
		if !typeMatches(p["type"], args[k]) {
			problems = append(problems, fmt.Sprintf("%s has wrong type (want %v)", k, p["type"]))
		}
	}
	return problems
}

// typeMatches checks a decoded JSON value against a JSON Schema "type", which
// may be a string or a list of strings. Missing or unknown type matches.
func typeMatches(typ any, v any) bool {
	switch t := typ.(type) {
	case string:
		return matchesOne(t, v)
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && matchesOne(s, v) {
				return true
			}
		}
		return false
	}
	return true
}

func matchesOne(t string, v any) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "null":
		return v == nil
	}
	return true
}

// Trial is one recorded (case, model, rep) result: one JSONL line.
type Trial struct {
	CaseID        string         `json:"case_id"`
	Category      string         `json:"category"`
	Model         string         `json:"model"`
	Rep           int            `json:"rep"`
	Chosen        string         `json:"chosen"`
	Args          map[string]any `json:"args,omitempty"`
	Ideal         string         `json:"ideal"`
	Acceptable    bool           `json:"acceptable"`
	Verdict       string         `json:"verdict"`
	SchemaValid   *bool          `json:"schema_valid,omitempty"`
	SchemaProblem []string       `json:"schema_problems,omitempty"`
	LatencyMs     int64          `json:"latency_ms"`
	CostUSD       float64        `json:"cost_usd,omitempty"`
	TokensIn      int            `json:"tokens_in,omitempty"`
	TokensOut     int            `json:"tokens_out,omitempty"`
	FinishReason  string         `json:"finish_reason,omitempty"`
	Text          string         `json:"text,omitempty"` // assistant text, kept only when no tool was called
	Error         string         `json:"error,omitempty"`
}

// ModelStats is the per-model aggregate.
type ModelStats struct {
	Model       string  `json:"model"`
	Trials      int     `json:"trials"`      // scored trials (errors excluded, truncated included)
	NEffective  int     `json:"n_effective"` // trials minus truncated: the denominator of every rate but trunc%
	Errors      int     `json:"errors"`
	Truncated   int     `json:"truncated"`
	TruncPct    float64 `json:"trunc_pct"` // truncated / trials
	IdealPct    float64 `json:"ideal_pct"`
	AcceptPct   float64 `json:"acceptable_pct"` // ideal + acceptable
	EscapePct   float64 `json:"escape_pct"`
	NonePct     float64 `json:"none_pct"`
	SchemaPct   float64 `json:"schema_valid_pct"` // of trials with a tool call
	CostUSD     float64 `json:"cost_usd"`
	MeanLatency float64 `json:"mean_latency_ms"`
}

// Confusion lists, per model, the non-acceptable picks: case -> chosen -> count.
type Confusion struct {
	Model  string `json:"model"`
	CaseID string `json:"case_id"`
	Ideal  string `json:"ideal"`
	Chosen string `json:"chosen"`
	Count  int    `json:"count"`
}

// Report is the full summary.
type Report struct {
	Models    []ModelStats `json:"models"`
	Confusion []Confusion  `json:"confusion"`
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

// summarize aggregates trials into the per-model table and the confusion list.
func summarize(trials []Trial) Report {
	type acc struct {
		ModelStats
		ideal, accept, escape, none, called, valid int
		latency                                    int64
	}
	byModel := map[string]*acc{}
	var order []string
	conf := map[[3]string]*Confusion{}
	for _, t := range trials {
		a := byModel[t.Model]
		if a == nil {
			a = &acc{ModelStats: ModelStats{Model: t.Model}}
			byModel[t.Model] = a
			order = append(order, t.Model)
		}
		a.CostUSD += t.CostUSD
		if t.Verdict == VerdictError {
			a.Errors++
			continue
		}
		a.Trials++
		a.latency += t.LatencyMs
		switch t.Verdict {
		case VerdictTruncated:
			a.Truncated++
		case VerdictIdeal:
			a.ideal++
			a.accept++
		case VerdictAcceptable:
			a.accept++
		case VerdictEscape:
			a.escape++
		case VerdictNone:
			a.none++
		}
		if t.Chosen != "none" && t.Chosen != "" {
			a.called++
			if t.SchemaValid != nil && *t.SchemaValid {
				a.valid++
			}
		}
		if t.Verdict != VerdictIdeal && t.Verdict != VerdictAcceptable && t.Verdict != VerdictTruncated {
			k := [3]string{t.Model, t.CaseID, t.Chosen}
			if conf[k] == nil {
				conf[k] = &Confusion{Model: t.Model, CaseID: t.CaseID, Ideal: t.Ideal, Chosen: t.Chosen}
			}
			conf[k].Count++
		}
	}
	var rep Report
	for _, m := range order {
		a := byModel[m]
		s := a.ModelStats
		s.NEffective = a.Trials - a.Truncated
		s.TruncPct = pct(a.Truncated, a.Trials)
		s.IdealPct = pct(a.ideal, s.NEffective)
		s.AcceptPct = pct(a.accept, s.NEffective)
		s.EscapePct = pct(a.escape, s.NEffective)
		s.NonePct = pct(a.none, s.NEffective)
		s.SchemaPct = pct(a.valid, a.called)
		if a.Trials > 0 {
			s.MeanLatency = float64(a.latency) / float64(a.Trials)
		}
		rep.Models = append(rep.Models, s)
	}
	for _, c := range conf {
		rep.Confusion = append(rep.Confusion, *c)
	}
	sort.Slice(rep.Confusion, func(i, j int) bool {
		a, b := rep.Confusion[i], rep.Confusion[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.CaseID != b.CaseID {
			return a.CaseID < b.CaseID
		}
		return a.Chosen < b.Chosen
	})
	return rep
}
