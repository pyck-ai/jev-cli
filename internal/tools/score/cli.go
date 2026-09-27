// This file adds jev_score's CLI subcommand (`jev score ...`), built on
// top of the same run core as the MCP handler (see score.go).
package score

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
// `jev score --help`.
const cliShortDescription = "Judge text/data against a numeric rubric"

// exitCode maps a ScoreOutput to the CLI's exit-code scheme: 0 for a
// valid judgment, 3 for a hard failure. score has no auto/review/
// escalate verdict tiers, so this is the two-value case in that scheme
// (see the project's exit-code table).
func exitCode(out ScoreOutput) int {
	if out.Status != StatusOK {
		return 3
	}
	return 0
}

// newCLICommand builds the `jev score` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev score --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "score",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[ScoreInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("score: %w", err)
		}

		deps := provider()
		h := NewScoreHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
