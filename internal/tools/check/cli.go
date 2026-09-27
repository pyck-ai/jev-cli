// This file adds jev_check's CLI subcommand (`jev check ...`), built on
// top of the same run core as the MCP handler (see check.go).
package check

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-mcp/internal/cliformat"
	"github.com/pyck-ai/jev-mcp/internal/cliinput"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

const cliShortDescription = "Batch-check propositions for truth"

// exitCode maps a CheckOutput to the CLI's exit-code scheme: 1 if any
// proposition's Action is ActionReview (which also covers every
// proposition whose Status == StatusInvalidResponse, since
// CheckHandler.run forces Action to ActionReview whenever a proposition's
// answer is invalid; see PropositionResult's doc comment), else 0. check
// has no verdict tier analogous to verify's "contradicts" or screen's
// "block", so this is the two-value (0/1) case of the scheme;
// CheckOutput also has no top-level hard-failure status field, so there
// is no exit-3 case either.
func exitCode(out CheckOutput) int {
	for _, r := range out.Results {
		if r.Action == ActionReview {
			return 1
		}
	}
	return 0
}

// newCLICommand builds the `jev check` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev check --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[CheckInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("check: %w", err)
		}

		deps := provider()
		h := NewCheckHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
