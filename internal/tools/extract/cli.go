// This file adds jev_extract's CLI subcommand (`jev extract ...`), built
// on top of the same run core as the MCP handler (see extract.go).
package extract

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/cliformat"
	"github.com/pyck-ai/jev-cli/internal/cliinput"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

const cliShortDescription = "Extract named fields from a document via regex + model choice"

// exitCode maps an ExtractOutput to the CLI's exit-code scheme, taking
// the worst case across every field result: 2 if any field's status is
// "invalid_response" or "invalid_pattern" (something genuinely went
// wrong), else 1 if any field's status is "review" (a human should look,
// e.g. the model confidently picked "none of them"), else 0.
// "not_found" is a normal outcome (the regex simply found nothing), not
// an error, so it never raises the exit code on its own.
func exitCode(out ExtractOutput) int {
	sawReview := false
	for _, f := range out.Fields {
		switch f.Status {
		case StatusInvalidResponse, StatusInvalidPattern:
			return 2
		case StatusReview:
			sawReview = true
		}
	}
	if sawReview {
		return 1
	}
	return 0
}

// newCLICommand builds the `jev extract` subcommand. provider is called
// only when the command actually runs (never during flag parsing or
// --help), so `jev extract --help` needs no credentials configured.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "extract",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[ExtractInput](cmd)
	cliformat.AddOutputFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("extract: %w", err)
		}

		deps := provider()
		h := NewExtractHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)

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
