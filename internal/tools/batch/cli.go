// This file adds jev_batch's CLI subcommand (`jev batch ...`), built on the
// same run core as the MCP handler (see batch.go).
package batch

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/cliformat"
	"github.com/pyck-ai/jev-cli/internal/cliinput"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

const cliShortDescription = "Run several independent jev tool calls in one call"

// newCLICommand builds the `jev batch` subcommand. Input is `--items
// '<json array>'` or the whole object via `-j '{"items": [...]}'` / `-j -`.
// Output defaults to JSON (not the text format): results[].output holds
// heterogeneous per-tool outputs that the struct-based text renderer cannot
// show. The process exits with summary.exit_code.
func newCLICommand(provider registry.DepsProvider, longDescription string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "batch",
		Short: cliShortDescription,
		Long:  longDescription,
	}
	binder := cliinput.Bind[BatchInput](cmd)
	cliformat.AddOutputFlag(cmd)
	// cliformat.Render cannot render the `any` output field; default to JSON.
	if f := cmd.Flags().Lookup("output"); f != nil {
		f.DefValue = "json"
		_ = f.Value.Set("json")
	}

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in, err := binder.Parse()
		if err != nil {
			return fmt.Errorf("batch: %w", err)
		}

		out, err := NewBatchHandler(provider()).run(cmd.Context(), in)
		if err != nil {
			return err
		}

		if err := cliformat.Emit(cmd.OutOrStdout(), out, cliformat.JSONRequested(cmd)); err != nil {
			return err
		}

		exitFunc(out.Summary.ExitCode)
		return nil
	}

	return cmd
}

// exitFunc is os.Exit by default; tests swap it for a recording stub.
var exitFunc = os.Exit
