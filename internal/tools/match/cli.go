// This file adds jev_match's CLI subcommand (`jev match ...`), built on
// top of the same run core as the MCP handler (see match.go).
package match

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-mcp/internal/cliformat"
	"github.com/pyck-ai/jev-mcp/internal/cliinput"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

const cliShortDescription = "Find the best-matching candidate for a query"

// exitCode maps a MatchOutput to the CLI's exit-code scheme: 3 when the
// call's Status is StatusInvalidResponse (a hard, whole-call failure --
// see MatchOutput's doc comment: match fails closed at the whole-call
// level, not per-item, so ExistsVerdict is "" in that case and cannot be
// mapped via the table below), else via ExistsVerdict: VerdictAbsent ->
// 2, VerdictPartial -> 1, VerdictAnswered -> 0.
func exitCode(out MatchOutput) int {
	if out.Status == StatusInvalidResponse {
		return 3
	}
	switch out.ExistsVerdict {
	case VerdictAbsent:
		return 2
	case VerdictPartial:
		return 1
	default: // VerdictAnswered
		return 0
	}
}

// newCLICommand builds the `jev match` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev match --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "match",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[MatchInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("match: %w", err)
		}

		deps := provider()
		h := NewMatchHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
