// This file adds jev_gate's CLI subcommand (`jev gate ...`), built on
// top of the same run core as the MCP handler (see gate.go).
package gate

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-mcp/internal/cliformat"
	"github.com/pyck-ai/jev-mcp/internal/cliinput"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

const cliShortDescription = "jev_review plus evidence-only claim verification"

// exitCode maps a GateOutput's top-level Action to the CLI's exit-code
// scheme, identically to jev_review's own mapping (see
// internal/tools/review/cli.go's exitCode): 2 for escalate, 1 for
// review, 0 for auto. GateOutput.Action is always exactly one of
// ActionAuto/ActionReview/ActionEscalate (see computeAction), so there
// is no fourth "hard error" tier to map here.
func exitCode(out GateOutput) int {
	switch out.Action {
	case ActionEscalate:
		return 2
	case ActionReview:
		return 1
	default:
		return 0
	}
}

// newCLICommand builds the `jev gate` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev gate --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gate",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[GateInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("gate: %w", err)
		}

		deps := provider()
		h := NewGateHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

		out, err := h.run(cmd.Context(), in)
		if err != nil {
			return err
		}

		if err := cliformat.Emit(cmd.OutOrStdout(), out, cliformat.JSONRequested(cmd)); err != nil {
			return err
		}

		exitFunc(exitCode(out))
		return nil
	}

	return cmd
}

// exitFunc is os.Exit by default; tests swap it for a recording stub so
// they can assert the chosen exit code without killing the test binary.
var exitFunc = os.Exit
