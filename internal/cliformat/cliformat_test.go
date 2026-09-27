package cliformat_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-mcp/internal/cliformat"
)

// outShape exercises every renderable shape: scalars, a map, a slice of
// scalars, a slice of structs, a nested struct, a nil pointer, and a
// non-nil pointer to scalar.
type outShape struct {
	Status        string             `json:"status"`
	Confidence    float64            `json:"confidence"`
	LatencyMs     int64              `json:"latency_ms"`
	Probabilities map[string]float64 `json:"probabilities"`
	Tags          []string           `json:"tags"`
	Results       []resultRow        `json:"results"`
	Usage         *usageBlock        `json:"usage"`
	ErrorMsg      *string            `json:"error"`
	Hidden        string             `json:"-"`
}

type resultRow struct {
	Claim      string  `json:"claim"`
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
}

type usageBlock struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func sampleOut() outShape {
	errMsg := "probe failed"
	return outShape{
		Status:        "ok",
		Confidence:    0.98,
		LatencyMs:     467,
		Probabilities: map[string]float64{"0": 0.99, "1": 0.01},
		Tags:          []string{"a", "b"},
		Results: []resultRow{
			{Claim: "the sky is green", Verdict: "contradicted", Confidence: 1},
			{Claim: "grass is green", Verdict: "supported", Confidence: 0.97},
		},
		Usage:    &usageBlock{InputTokens: 12, OutputTokens: 34},
		ErrorMsg: &errMsg,
	}
}

func TestRender_Text(t *testing.T) {
	var buf bytes.Buffer
	if err := cliformat.Render(&buf, sampleOut()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"status:",
		"confidence:",
		"latency_ms:",
		"probabilities:",
		"0:",
		"0.99",
		"tags:",
		"a, b",
		"results:",
		"claim",
		"the sky is green",
		"contradicted",
		"usage:",
		"input_tokens",
		"error:",
		"probe failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}

	// Scalar and map lines carry the colon convention, values follow.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "status:") && !strings.Contains(line, "ok") {
			t.Errorf("status line malformed: %q", line)
		}
	}

	// No trailing whitespace anywhere (tabwriter cells are tab-joined
	// without a trailing tab).
	for _, line := range strings.Split(got, "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line has trailing whitespace: %q", line)
		}
	}

	// Hidden (json:"-") fields must not render.
	if strings.Contains(got, "Hidden") || strings.Contains(got, "hidden") {
		t.Errorf("json:\"-\" field rendered:\n%s", got)
	}
}

func TestRender_NilPointerRendersNull(t *testing.T) {
	var buf bytes.Buffer
	out := outShape{Status: "ok"}
	if err := cliformat.Render(&buf, out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(buf.String(), "usage:") || !strings.Contains(buf.String(), "null") {
		t.Errorf("nil pointer should render as null, got:\n%s", buf.String())
	}
}

func TestRender_MapSortedByKey(t *testing.T) {
	var buf bytes.Buffer
	out := outShape{Probabilities: map[string]float64{"10": 0.1, "2": 0.2, "1": 0.7}}
	if err := cliformat.Render(&buf, out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	// Keys sort as strings (documented behavior): "1" < "10" < "2".
	i1 := strings.Index(got, "1:")
	i10 := strings.Index(got, "10:")
	i2 := strings.Index(got, "2:")
	if !(i1 < i10 && i10 < i2) {
		t.Errorf("map keys not sorted by string key:\n%s", got)
	}
}

func TestEmit_JSON(t *testing.T) {
	var buf bytes.Buffer
	out := sampleOut()
	if err := cliformat.Emit(&buf, out, true); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// JSON output must round-trip to the same struct (byte-compatibility
	// with the MCP tool's own JSON output shape).
	var back outShape
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Status != out.Status || back.Confidence != out.Confidence {
		t.Errorf("round-trip mismatch: %+v", back)
	}
	if len(back.Results) != 2 || back.Results[0].Verdict != "contradicted" {
		t.Errorf("results round-trip mismatch: %+v", back.Results)
	}
	if back.Hidden != "" {
		t.Errorf("json:\"-\" field should not round-trip, got %q", back.Hidden)
	}
}

func TestEmit_TextMatchesRender(t *testing.T) {
	var jsonBuf, textBuf bytes.Buffer
	out := sampleOut()
	if err := cliformat.Emit(&textBuf, out, false); err != nil {
		t.Fatalf("Emit text: %v", err)
	}
	if err := cliformat.Render(&jsonBuf, out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if textBuf.String() != jsonBuf.String() {
		t.Errorf("Emit(false) differs from Render:\n%q\n%q", textBuf.String(), jsonBuf.String())
	}
}

func TestOutputFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cliformat.AddOutputFlag(cmd)
	if err := cmd.ParseFlags([]string{"-o", "json"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !cliformat.JSONRequested(cmd) {
		t.Error("JSONRequested = false after -o json")
	}
	cmd2 := &cobra.Command{Use: "test"}
	cliformat.AddOutputFlag(cmd2)
	if cliformat.JSONRequested(cmd2) {
		t.Error("JSONRequested = true with default text output")
	}
}
