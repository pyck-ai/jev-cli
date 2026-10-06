package main

import (
	"encoding/json"
	"strings"
)

// priceUSDPerMTok is a rough (input, output) price table keyed by a substring
// of the model id. It only drives the pre-run cost estimate and is deliberately
// conservative; actual cost is read from the response when reported.
var priceUSDPerMTok = []struct {
	match   string
	in, out float64
}{
	{"opus", 15, 75},
	{"sonnet", 3, 15},
	{"haiku", 1, 5},
	{"gpt-5-mini", 0.25, 2},
	{"flash", 0.3, 1.5},
}

// unknownPrice is used for models not in the table.
var unknownPrice = struct{ in, out float64 }{3, 15}

// estimateTokens is the usual ~4 chars/token rule of thumb.
func estimateTokens(s string) int { return len(s)/4 + 1 }

// estimateUSD predicts the run cost: per trial, the whole tool list plus the
// system prompt and case prompt as input, and an assumed 400 output tokens
// (tool call plus some reasoning).
func estimateUSD(models []string, fns []Function, system string, cases []Case, reps int) float64 {
	tb, _ := json.Marshal(fns)
	toolTok := estimateTokens(string(tb))
	sysTok := estimateTokens(system)
	var promptTok int
	for _, c := range cases {
		promptTok += estimateTokens(c.Prompt)
	}
	var total float64
	for _, m := range models {
		in, out := unknownPrice.in, unknownPrice.out
		for _, p := range priceUSDPerMTok {
			if strings.Contains(m, p.match) {
				in, out = p.in, p.out
				break
			}
		}
		inTok := float64(len(cases)*(toolTok+sysTok) + promptTok)
		outTok := float64(len(cases) * 400)
		total += (inTok*in + outTok*out) / 1e6 * float64(reps)
	}
	return total
}
