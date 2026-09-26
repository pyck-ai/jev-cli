package answers

import (
	"encoding/json"
	"testing"
)

func TestNoul(t *testing.T) {
	if v, ok := Noul(json.RawMessage(`{"type":"noul","noul":0.92}`)); !ok || v != 0.92 {
		t.Errorf("Noul(valid) = %v, %v; want 0.92, true", v, ok)
	}
	cases := map[string]string{
		"wrong type":   `{"type":"choice","noul":0.5}`,
		"malformed":    `not json`,
		"out of range": `{"type":"noul","noul":1.5}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := Noul(json.RawMessage(raw)); ok {
				t.Errorf("expected ok=false for %q", name)
			}
		})
	}
}

func TestNoulLabel(t *testing.T) {
	cases := []struct {
		value, autoAccept float64
		want              string
	}{
		{0.9, 0.85, "likely"},
		{0.85, 0.85, "likely"}, // boundary: >= autoAccept
		{0.1, 0.85, "unlikely"},
		{0.15, 0.85, "unlikely"}, // boundary: <= 1-autoAccept
		{0.5, 0.85, "uncertain"},
		{0.5, 0.8, "uncertain"},
	}
	for _, c := range cases {
		if got := NoulLabel(c.value, c.autoAccept); got != c.want {
			t.Errorf("NoulLabel(%v, %v) = %q, want %q", c.value, c.autoAccept, got, c.want)
		}
	}
}

func TestChoice(t *testing.T) {
	valid := map[string]bool{"a": true, "b": true}

	choice, confidence, probs, ok := Choice(
		json.RawMessage(`{"type":"choice","choice":"a","confidence":0.7,"probabilities":{"a":0.7,"b":0.3}}`),
		valid,
	)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if choice != "a" || confidence != 0.7 || probs["a"] != 0.7 || probs["b"] != 0.3 {
		t.Errorf("got choice=%q confidence=%v probs=%v", choice, confidence, probs)
	}

	cases := map[string]string{
		"wrong type":               `{"type":"score","choice":"a"}`,
		"malformed json":           `not json`,
		"choice not in criteria":   `{"type":"choice","choice":"z","confidence":0.5,"probabilities":{}}`,
		"probability key unknown":  `{"type":"choice","choice":"a","confidence":0.5,"probabilities":{"z":1.0}}`,
		"probability out of range": `{"type":"choice","choice":"a","confidence":0.5,"probabilities":{"a":1.5}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, ok := Choice(json.RawMessage(raw), valid); ok {
				t.Errorf("expected ok=false for %q", name)
			}
		})
	}
}

func TestChoice_PartialProbabilitiesAreTolerated(t *testing.T) {
	// Choice does not require every valid option to appear in
	// probabilities (undocumented either way) -- see the package doc
	// comment.
	valid := map[string]bool{"a": true, "b": true, "c": true}
	_, _, probs, ok := Choice(
		json.RawMessage(`{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9}}`),
		valid,
	)
	if !ok {
		t.Fatalf("expected ok=true for a partial (but internally consistent) probabilities map")
	}
	if len(probs) != 1 {
		t.Errorf("probs = %v, want exactly {\"a\":0.9}", probs)
	}
}

func TestScore(t *testing.T) {
	score, confidence, probs, ok := Score(
		json.RawMessage(`{"type":"score","score":1.99,"confidence":0.99,"probabilities":{"0":0,"1":0.01,"2":0.99}}`),
		3,
	)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if score != 1.99 || confidence != 0.99 || probs["2"] != 0.99 {
		t.Errorf("got score=%v confidence=%v probs=%v", score, confidence, probs)
	}

	cases := map[string]string{
		"wrong type":           `{"type":"choice","choice":"x"}`,
		"malformed json":       `not json`,
		"missing a level":      `{"type":"score","score":0.5,"confidence":0.5,"probabilities":{"0":0.5,"1":0.5}}`,
		"sum not 1":            `{"type":"score","score":1,"confidence":0.5,"probabilities":{"0":0.1,"1":0.1,"2":0.1}}`,
		"probability negative": `{"type":"score","score":1,"confidence":0.5,"probabilities":{"0":-0.5,"1":0.5,"2":1.0}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, ok := Score(json.RawMessage(raw), 3); ok {
				t.Errorf("expected ok=false for %q", name)
			}
		})
	}
}

func TestScore_Tolerance(t *testing.T) {
	// sum = 0.995, within the documented 0.01 tolerance of 1.0
	_, _, _, ok := Score(
		json.RawMessage(`{"type":"score","score":1,"confidence":0.5,"probabilities":{"0":0.005,"1":0.49,"2":0.5}}`),
		3,
	)
	if !ok {
		t.Errorf("expected sum-within-tolerance probabilities to be valid")
	}
}

func TestResolveThreshold(t *testing.T) {
	cases := []struct {
		v, def, want float64
	}{
		{0, 0.8, 0.8},
		{-1, 0.8, 0.8},
		{0.9, 0.8, 0.9},
	}
	for _, c := range cases {
		if got := ResolveThreshold(c.v, c.def); got != c.want {
			t.Errorf("ResolveThreshold(%v, %v) = %v, want %v", c.v, c.def, got, c.want)
		}
	}
}

func TestValidateAutoAccept(t *testing.T) {
	if err := ValidateAutoAccept("auto_accept", 0); err != nil {
		t.Errorf("0 (use default) should be valid, got %v", err)
	}
	if err := ValidateAutoAccept("auto_accept", 0.85); err != nil {
		t.Errorf("0.85 should be valid, got %v", err)
	}
	if err := ValidateAutoAccept("auto_accept", 1); err != nil {
		t.Errorf("1 should be valid, got %v", err)
	}
	cases := []float64{0.5, 0.3, 1.5, -0.1}
	for _, v := range cases {
		if err := ValidateAutoAccept("auto_accept", v); err == nil {
			t.Errorf("expected error for auto_accept=%v", v)
		}
	}
}
