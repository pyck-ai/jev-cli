// This file adds jev_doctor's CLI subcommand (`jev doctor ...`), built
// on top of the same run core as the MCP handler (see doctor.go).
package doctor

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/cliformat"
	"github.com/pyck-ai/jev-cli/internal/cliinput"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

const cliShortDescription = "Check OpenRouter/SystemOne connectivity and current config"

// exitCode maps a DoctorOutput to the CLI's exit-code scheme: 3
// (hard-error) when the probe couldn't reach the endpoint, 0 otherwise.
// Unreachability is jev_doctor's whole diagnostic point (see package doc
// comment: it's never a Go error), but for the CLI it's still the "this
// invocation didn't succeed" case, hence 3 rather than a verdict tier.
func exitCode(out DoctorOutput) int {
	if !out.Reachable {
		return 3
	}
	return 0
}

// newCLICommand builds the `jev doctor` subcommand. provider is called
// only when the command actually runs, never during flag parsing or
// --help.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[DoctorInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("doctor: %w", err)
		}

		deps := provider()
		h := NewDoctorHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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

// exitFunc is os.Exit by default; tests swap it for a recording stub.
var exitFunc = os.Exit
