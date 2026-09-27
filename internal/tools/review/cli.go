// This file adds jev_review's CLI subcommand (`jev review ...`), built
// on top of the same run core as the MCP handler (see review.go).
package review

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/cliformat"
	"github.com/pyck-ai/jev-cli/internal/cliinput"
	"github.com/pyck-ai/jev-cli/internal/registry"
	"github.com/pyck-ai/jev-cli/internal/tools/reviewcore"
)

const cliShortDescription = "Assess a diff against a request on four weighted rubrics"

// exitCode maps a ReviewOutput's Action to the CLI's exit-code scheme: 2
// for escalate, 1 for review, 0 for auto. reviewcore.Assessment.Action is
// always exactly one of those three values (see
// reviewcore.ParseAssessment's decision rule), so there is no fourth
// "hard error" tier to map here -- a run that couldn't produce an
// Assessment at all returns a Go error instead (see Handle/run) and never
// reaches exitCode.
func exitCode(out ReviewOutput) int {
	switch out.Action {
	case reviewcore.ActionEscalate:
		return 2
	case reviewcore.ActionReview:
		return 1
	default:
		return 0
	}
}

// newCLICommand builds the `jev review` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev review --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[ReviewInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("review: %w", err)
		}

		deps := provider()
		h := NewReviewHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
