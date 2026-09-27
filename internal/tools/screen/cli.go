// This file adds jev_screen's CLI subcommand (`jev screen ...`), built on
// top of the same run core as the MCP handler (see screen.go).
package screen

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/cliformat"
	"github.com/pyck-ai/jev-cli/internal/cliinput"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

const cliShortDescription = "Screen text for prompt injection, substance, and relevance"

// exitCode maps a ScreenOutput to the CLI's exit-code scheme via its sole
// verdict field, Recommendation.Action: ActionBlock -> 2, ActionReview ->
// 1, ActionPass/ActionSkip -> 0, exactly as specified. ScreenOutput has no
// top-level hard-failure status field (see package doc comment: a
// missing/invalid injection signal already fails closed to
// Recommendation.Action == ActionReview via recommend, never a separate
// status), so there is no exit-3 case here.
func exitCode(out ScreenOutput) int {
	switch out.Recommendation.Action {
	case ActionBlock:
		return 2
	case ActionReview:
		return 1
	default: // ActionPass, ActionSkip
		return 0
	}
}

// newCLICommand builds the `jev screen` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev screen --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "screen",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[ScreenInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("screen: %w", err)
		}

		deps := provider()
		h := NewScreenHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
