package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRegisterAndAll_PreservesOrder confirms All() returns Tools in the
// exact order they were passed to Register, since cmd/jev/main.go relies on that
// order matching Go's package-init order for its blank imports (not that
// cmd/jev/main.go's behavior actually depends on tool registration order today,
// but a silent reordering would be a surprising regression for this
// package to introduce).
func TestRegisterAndAll_PreservesOrder(t *testing.T) {
	// Save/restore the package-level slice so this test doesn't leak state
	// into any other test in this package (init() functions from other
	// packages are not linked into this package's own test binary, so
	// tools starts nil here regardless, but save/restore keeps this test
	// self-contained even if that ever changes).
	orig := tools
	tools = nil
	defer func() { tools = orig }()

	var order []int
	for i := range 3 {
		Register(Tool{RegisterMCP: func(_ *mcp.Server, _ *Deps) { order = append(order, i) }})
	}

	regs := All()
	if len(regs) != 3 {
		t.Fatalf("len(All()) = %d, want 3", len(regs))
	}
	for _, r := range regs {
		r.RegisterMCP(nil, nil)
	}

	want := []int{0, 1, 2}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order[%d] = %d, want %d", i, order[i], want[i])
		}
	}
}

type runnerIn struct {
	Name  string `json:"name"`
	Count int    `json:"count,omitempty" jsonschema:"count"`
}

type runnerOut struct {
	Echo  string `json:"echo"`
	Count int    `json:"count"`
}

func testRunner() RunFunc {
	return Runner(func(_ context.Context, _ *Deps, in runnerIn) (runnerOut, error) {
		if in.Name == "boom" {
			return runnerOut{}, errors.New("boom failed")
		}
		return runnerOut{Echo: in.Name, Count: in.Count}, nil
	}, func(o runnerOut) int { return o.Count })
}

func TestRunner(t *testing.T) {
	run := testRunner()
	tests := []struct {
		name     string
		input    string
		wantErr  string
		wantExit int
		wantOut  *runnerOut
	}{
		{"ok maps exit code", `{"name":"a","count":2}`, "", 2, &runnerOut{"a", 2}},
		{"ok exit zero", `{"name":"a"}`, "", 0, &runnerOut{"a", 0}},
		{"run error is exit 3", `{"name":"boom"}`, "boom failed", 3, nil},
		{"bad json", `{"name":`, "unmarshaling arguments", 3, nil},
		{"not an object", `[1]`, "unmarshaling arguments", 3, nil},
		{"unknown field rejected", `{"name":"a","extra":1}`, "validating", 3, nil},
		{"missing required", `{"count":1}`, "validating", 3, nil},
		{"wrong type", `{"name":5}`, "validating", 3, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, code, err := run(context.Background(), nil, json.RawMessage(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if out != nil {
					t.Errorf("out = %v, want nil on error", out)
				}
			} else if err != nil {
				t.Fatalf("unexpected err: %v", err)
			} else if got := out.(runnerOut); got != *tt.wantOut {
				t.Errorf("out = %+v, want %+v", got, *tt.wantOut)
			}
			if code != tt.wantExit {
				t.Errorf("exit = %d, want %d", code, tt.wantExit)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	orig := tools
	tools = nil
	defer func() { tools = orig }()

	Register(Tool{Name: "alpha", MCPName: "jev_alpha", Run: testRunner()})
	for _, name := range []string{"alpha", "jev_alpha"} {
		got, ok := Lookup(name)
		if !ok || got.Name != "alpha" || got.Run == nil {
			t.Errorf("Lookup(%q) = %+v, %v", name, got, ok)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup(nope) found a tool")
	}
}
