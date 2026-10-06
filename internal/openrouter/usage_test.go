package openrouter

import (
	"encoding/json"
	"testing"
)

func TestUsageCostUSD(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *float64
	}{
		{"present", `{"cost":0.5,"input_tokens":1}`, ptr(0.5)},
		{"explicit zero", `{"cost":0}`, ptr(0)},
		{"absent", `{"input_tokens":1}`, nil},
		{"null", `{"cost":null}`, nil},
	}
	for _, c := range cases {
		var u Usage
		if err := json.Unmarshal([]byte(c.in), &u); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := u.CostUSD()
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: CostUSD() = %v, want %v", c.name, got, c.want)
		}
	}
	var nilUsage *Usage
	if nilUsage.CostUSD() != nil {
		t.Error("nil Usage must give nil cost")
	}
}

func ptr(f float64) *float64 { return &f }
