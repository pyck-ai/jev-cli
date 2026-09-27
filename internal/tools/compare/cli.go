// This file adds jev_compare's CLI subcommand (`jev compare ...`), built
// on top of the same run core as the MCP handler (see compare.go).
package compare

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-mcp/internal/cliformat"
	"github.com/pyck-ai/jev-mcp/internal/cliinput"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

// cliShortDescription is a one-line summary for `jev --help`'s
// subcommand list; cliLongDescription (the tool's registered
// Description, shared with the MCP tool) is the full text shown by
// `jev compare --help`.
const cliShortDescription = "Compare two passages' factual relation"

// exitCode maps a CompareOutput to the CLI's exit-code scheme: 1 (needs
// review) if Overall.Decision or any Aspects[i].Decision is "review" --
// which Decision is already forced to whenever that result's Status is
// "invalid_response" (see compare.go's run), so this single check
// covers both "the model's answer was malformed" and "the model answered
// but confidence didn't clear the auto-accept bar" for both Overall and
// every aspect -- 0 otherwise. CompareOutput has no whole-call failure
// state distinct from Overall's own Status/Decision, so there is no 2 or
// 3 tier here.
func exitCode(out CompareOutput) int {
	if out.Overall.Decision == DecisionReview {
		return 1
	}
	for _, a := range out.Aspects {
		if a.Decision == DecisionReview {
			return 1
		}
	}
	return 0
}

// newCLICommand builds the `jev compare` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev compare --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compare",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[CompareInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("compare: %w", err)
		}

		deps := provider()
		h := NewCompareHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
