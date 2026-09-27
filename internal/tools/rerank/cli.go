// This file adds jev_rerank's CLI subcommand (`jev rerank ...`), built on
// top of the same run core as the MCP handler (see rerank.go).
package rerank

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/cliformat"
	"github.com/pyck-ai/jev-cli/internal/cliinput"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

// cliShortDescription is a one-line summary for `jev --help`'s
// subcommand list; cliLongDescription (the tool's registered
// Description, shared with the MCP tool) is the full text shown by
// `jev rerank --help`.
const cliShortDescription = "Rank candidates by relevance to a query"

// exitCode maps a RerankOutput to the CLI's exit-code scheme: 3
// (hard-error) when Status != "ok", 0 otherwise. jev_rerank fails closed
// at the WHOLE-CALL level (see package doc comment: one malformed
// candidate answer invalidates the entire ranking, Ranked stays nil) --
// there is no per-candidate review tier the way jev_classify/jev_compare
// have, so this is the same two-value case as internal/tools/score's
// exitCode.
func exitCode(out RerankOutput) int {
	if out.Status != StatusOK {
		return 3
	}
	return 0
}

// newCLICommand builds the `jev rerank` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev rerank --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rerank",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[RerankInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("rerank: %w", err)
		}

		deps := provider()
		h := NewRerankHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
