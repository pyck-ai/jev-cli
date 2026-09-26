package registry

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRegisterAndAll_PreservesOrder confirms All() returns Registrars in
// the exact order they were passed to Register, since main.go relies on
// that order matching Go's package-init order for its blank imports (not
// that main.go's behavior actually depends on tool registration order
// today, but a silent reordering would be a surprising regression for this
// package to introduce).
func TestRegisterAndAll_PreservesOrder(t *testing.T) {
	// Save/restore the package-level slice so this test doesn't leak state
	// into any other test in this package (init() functions from other
	// packages are not linked into this package's own test binary, so
	// registrars starts nil here regardless, but save/restore keeps this
	// test self-contained even if that ever changes).
	orig := registrars
	registrars = nil
	defer func() { registrars = orig }()

	var order []int
	for i := range 3 {
		Register(func(_ *mcp.Server, _ *Deps) { order = append(order, i) })
	}

	regs := All()
	if len(regs) != 3 {
		t.Fatalf("len(All()) = %d, want 3", len(regs))
	}
	for _, r := range regs {
		r(nil, nil)
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
