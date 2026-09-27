// This file adds jev_verify's CLI subcommand (`jev verify ...`), built on
// top of the same run core as the MCP handler (see verify.go).
package verify

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-mcp/internal/cliformat"
	"github.com/pyck-ai/jev-mcp/internal/cliinput"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

const cliShortDescription = "Batch-verify claims against evidence"

// exitCode maps a VerifyOutput to the CLI's exit-code scheme: 2 when any
// claim's Verdict is VerdictContradicts, else 1 when (short of that) any
// claim needs review (Action == ActionReview -- which also covers every
// claim whose Status == StatusInvalidResponse, since VerifyHandler.run
// forces Action to ActionReview whenever a claim's answer is invalid; see
// ClaimResult's doc comment), else 0. Worst case across every claim wins.
//
// Deviation from the project brief's literal exit-code table: the brief
// names the verdict "contradicted"; this tool's actual verdict constant
// (see VerdictContradicts) is "contradicts". This function checks the
// real constant, not the brief's spelling. VerifyOutput has no top-level
// hard-failure status field (unlike e.g. match.MatchOutput.Status), so
// there is no exit-3 case here.
func exitCode(out VerifyOutput) int {
	reviewSeen := false
	for _, r := range out.Results {
		if r.Verdict == VerdictContradicts {
			return 2
		}
		if r.Action == ActionReview {
			reviewSeen = true
		}
	}
	if reviewSeen {
		return 1
	}
	return 0
}

// newCLICommand builds the `jev verify` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev verify --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[VerifyInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("verify: %w", err)
		}

		deps := provider()
		h := NewVerifyHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
