// This file adds jev_decide's CLI subcommand (`jev decide ...`), built
// on top of the same run core as the MCP handler (see decide.go).
package decide

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
// `jev decide --help`.
const cliShortDescription = "Recommend the best candidate for a decision"

// exitCode maps a DecideOutput to the CLI's exit-code scheme.
//
// DecideOutput's shape does not match the generic "one Status +
// Decision per item" pattern classify/compare use, so this is not the
// project's terse "recommendation.escaped == true -> 1; else 0" line
// verbatim -- it is extended in two ways, both noted in the task report:
//
//  1. Recommendation carries ONLY a Status field (ok/invalid_response),
//     no Decision field at all (see decide.go's package doc comment:
//     "jev_decide's own project brief names NO confidence/auto-accept
//     threshold for its Recommendation... this implementation adds
//     none") -- structurally that makes it a single-verdict field like
//     jev_score/jev_rerank's Status, not a per-item Status+Decision pair
//     like classify.ItemResult/compare.OverallResult. A malformed main
//     "decision" answer leaves Selected="" and Escaped=false (Go zero
//     values), which the literal one-line spec would misreport as exit 0
//     ("good"). Following the score/rerank precedent for a Status-only
//     verdict, this is instead the hard-failure tier: 3.
//  2. Checks[i].Answer == AnswerInvalidResponse -- a per-(candidate,
//     requirement) fail-closed sentinel folded into the SAME string enum
//     as Answer's real values (see Check's doc comment: "mirrors
//     internal/tools/extract's FieldResult.Status") -- is treated as a
//     per-item invalid_response and mapped to the review tier (1),
//     per this mapping's own instruction to treat any per-item
//     invalid_response as at least 1.
func exitCode(out DecideOutput) int {
	if out.Recommendation.Status != StatusOK {
		return 3
	}
	if out.Recommendation.Escaped {
		return 1
	}
	for _, c := range out.Checks {
		if c.Answer == AnswerInvalidResponse {
			return 1
		}
	}
	return 0
}

// newCLICommand builds the `jev decide` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev decide --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decide",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[DecideInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("decide: %w", err)
		}

		deps := provider()
		h := NewDecideHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
