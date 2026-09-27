// This file adds jev_ask's CLI subcommand (`jev ask ...`), built on top
// of the same run core as the MCP handler (see ask.go).
package ask

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/cliformat"
	"github.com/pyck-ai/jev-cli/internal/cliinput"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

const cliShortDescription = "Escape hatch: ask arbitrary noul/choice/score questions"

// exitCode maps an AskOutput to the CLI's exit-code scheme: 2 if any
// requested question's answer came back status="invalid_response" (see
// AskAnswer.Status), else 0. Unlike jev_review/jev_gate, jev_ask has no
// auto/review/escalate verdict tier of its own to map to exit 1 -- each
// answer either parsed (ok) or it didn't (fail closed).
func exitCode(out AskOutput) int {
	for _, a := range out.Answers {
		if a.Status == StatusInvalidResponse {
			return 2
		}
	}
	return 0
}

// newCLICommand builds the `jev ask` subcommand. provider is called only
// when the command actually runs (never during flag parsing or --help),
// so `jev ask --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ask",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[AskInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("ask: %w", err)
		}

		deps := provider()
		h := NewAskHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
