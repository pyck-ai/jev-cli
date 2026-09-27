package cliinput_test

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/cliinput"
)

// testInput mirrors the shape of a real tool Input (score's, minus the
// domain meaning): all-scalar fields plus one structured field, so both
// flag conversion routes get exercised.
type testInput struct {
	State       string   `json:"state" jsonschema:"Text or data to be judged."`
	ScaleMin    int      `json:"scale_min"`
	ScaleMax    int      `json:"scale_max"`
	Threshold   float64  `json:"threshold,omitempty"`
	Verbose     bool     `json:"verbose,omitempty"`
	Ingredients []string `json:"ingredients,omitempty"`
}

func newBoundCmd(t *testing.T) (*cobra.Command, *cliinput.Binder[testInput]) {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	b := cliinput.Bind[testInput](cmd)
	return cmd, b
}

func TestParse_PerFieldFlags(t *testing.T) {
	cmd, b := newBoundCmd(t)
	if err := cmd.ParseFlags([]string{"--state", "hello world", "--scale-min", "0", "--scale-max", "2", "--threshold", "0.85", "--verbose", "true"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	in, err := b.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if in.State != "hello world" {
		t.Errorf("State = %q, want %q", in.State, "hello world")
	}
	if in.ScaleMin != 0 || in.ScaleMax != 2 {
		t.Errorf("Scale = [%d,%d], want [0,2]", in.ScaleMin, in.ScaleMax)
	}
	if in.Threshold != 0.85 {
		t.Errorf("Threshold = %v, want 0.85", in.Threshold)
	}
	if !in.Verbose {
		t.Errorf("Verbose = false, want true")
	}
}

func TestParse_StructuredFlagIsJSON(t *testing.T) {
	cmd, b := newBoundCmd(t)
	if err := cmd.ParseFlags([]string{"--state", "s", "--ingredients", `["a","b"]`}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	in, err := b.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if strings.Join(in.Ingredients, ",") != "a,b" {
		t.Errorf("Ingredients = %v, want [a b]", in.Ingredients)
	}
}

func TestParse_StructuredFlagRejectsNonJSON(t *testing.T) {
	cmd, b := newBoundCmd(t)
	if err := cmd.ParseFlags([]string{"--state", "s", "--ingredients", "not json"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	_, err := b.Parse()
	if err == nil {
		t.Fatal("expected error for non-JSON structured flag value")
	}
	if !strings.Contains(err.Error(), "--ingredients") {
		t.Errorf("error should name the flag: %v", err)
	}
}

func TestParse_BadIntRejected(t *testing.T) {
	cmd, b := newBoundCmd(t)
	if err := cmd.ParseFlags([]string{"--scale-min", "zero"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	_, err := b.Parse()
	if err == nil {
		t.Fatal("expected error for non-integer --scale-min")
	}
	if !strings.Contains(err.Error(), "--scale-min") {
		t.Errorf("error should name the flag: %v", err)
	}
}

func TestParse_JSONLiteral(t *testing.T) {
	cmd, b := newBoundCmd(t)
	if err := cmd.ParseFlags([]string{"--json", `{"state":"x","scale_min":0,"scale_max":1}`}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	in, err := b.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if in.State != "x" || in.ScaleMin != 0 || in.ScaleMax != 1 {
		t.Errorf("got %+v", in)
	}
}

func TestParse_JSONConflictWithFieldFlag(t *testing.T) {
	cmd, b := newBoundCmd(t)
	if err := cmd.ParseFlags([]string{"--json", `{"state":"x"}`, "--state", "y"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	_, err := b.Parse()
	if err == nil {
		t.Fatal("expected conflict error combining --json with a field flag")
	}
	if !strings.Contains(err.Error(), "--state") {
		t.Errorf("conflict error should name the conflicting flag: %v", err)
	}
}

func TestParse_JSONFlagUnusedWithNoFlags(t *testing.T) {
	// No flags set at all: empty input (tools' own validators reject it
	// with precise messages; cliinput deliberately doesn't duplicate
	// requiredness -- see the package doc comment).
	cmd, b := newBoundCmd(t)
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	in, err := b.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if in.State != "" {
		t.Errorf("State = %q, want empty", in.State)
	}
}

func TestFlagNamesAreKebabCase(t *testing.T) {
	cmd, _ := newBoundCmd(t)
	for _, name := range []string{"state", "scale-min", "scale-max", "threshold", "verbose", "ingredients", "json"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
	if cmd.Flags().Lookup("scale_min") != nil {
		t.Error("snake_case flag registered; should be kebab-case")
	}
}

func TestFlagUsageFromJsonschemaTag(t *testing.T) {
	cmd, _ := newBoundCmd(t)
	fl := cmd.Flags().Lookup("state")
	if fl == nil || !strings.Contains(fl.Usage, "Text or data") {
		t.Errorf("state flag usage should come from jsonschema tag, got %q", fl.Usage)
	}
}
