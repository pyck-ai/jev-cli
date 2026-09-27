// This file adds jev_classify's CLI subcommand (`jev classify ...`),
// built on top of the same run core as the MCP handler (see
// classify.go).
package classify

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
// `jev classify --help`.
const cliShortDescription = "Classify items into a fixed set of classes"

// exitCode maps a ClassifyOutput to the CLI's exit-code scheme: 1
// (needs review) if any item's Decision is "review" -- which
// ItemResult.Decision is already forced to whenever that item's Status
// is "invalid_response" (see classify.go's run), so this single check
// covers both "the model's answer for this item was malformed" and
// "the model answered but confidence/margin didn't clear the auto-accept
// bar" -- 0 otherwise. jev_classify has no whole-call failure state
// distinct from its per-item Status/Decision (unlike jev_rerank), so
// there is no 2 or 3 tier here.
func exitCode(out ClassifyOutput) int {
	for _, r := range out.Results {
		if r.Decision == DecisionReview {
			return 1
		}
	}
	return 0
}

// newCLICommand builds the `jev classify` subcommand. provider is
// called only when the command actually runs (never during flag parsing
// or --help), so `jev classify --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "classify",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[ClassifyInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("classify: %w", err)
		}

		deps := provider()
		h := NewClassifyHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
