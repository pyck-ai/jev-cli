package models

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/cliformat"
)

// ListOutput is the `jev models -o json` document.
type ListOutput struct {
	FetchedAt time.Time `json:"fetched_at"`
	Source    Source    `json:"source"`
	Models    []Model   `json:"models"`
}

// NewCommand builds `jev models`. provider is called lazily, only when
// the command runs, to obtain the Getter (normally the *openrouter.Client
// for the active route); an error from it is returned as the command's
// error. Flags: -o/--output text|json, --refresh.
func NewCommand(provider func() (Getter, error)) *cobra.Command {
	return NewCommandWithOptions(provider, Options{})
}

// NewCommandWithOptions is NewCommand with base Load options (for
// example a test cache dir). --refresh is OR-ed into base.Refresh.
func NewCommandWithOptions(provider func() (Getter, error), base Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "List the available SystemOne decision models",
		Long: "List the SystemOne decision models OpenRouter offers, with context\n" +
			"length, input price, input modalities and creation date. The list comes\n" +
			"from the API and is cached for 24h (see --refresh, which also clears the\n" +
			"request limits jev learned from provider rejections).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := provider()
			if err != nil {
				return err
			}
			opts := base
			refresh, _ := cmd.Flags().GetBool("refresh")
			opts.Refresh = opts.Refresh || refresh
			if opts.Refresh {
				// A refresh also forgets learned request limits (limits.json).
				if err := ClearLimits(opts); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not clear learned limits: %v\n", err)
				}
			}
			res, err := Load(cmd.Context(), g, opts)
			if err != nil {
				return err
			}
			if res.Warning != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", res.Warning)
			}
			models := append([]Model(nil), res.Catalog.Models...)
			sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
			if cliformat.JSONRequested(cmd) {
				return cliformat.Emit(cmd.OutOrStdout(), ListOutput{FetchedAt: res.Catalog.FetchedAt, Source: res.Source, Models: models}, true)
			}
			return renderTable(cmd.OutOrStdout(), models)
		},
	}
	cliformat.AddOutputFlag(cmd)
	cmd.Flags().Bool("refresh", false, "Ignore the cached model list and fetch it again; also clears learned request limits")
	return cmd
}

func renderTable(w io.Writer, models []Model) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tCONTEXT\t$/1M IN\tINPUTS\tCREATED")
	for _, m := range models {
		slug := m.ID
		if m.AliasTarget != "" {
			slug += " -> " + m.AliasTarget
		}
		ctxCol := "unknown"
		if m.ContextLength > 0 {
			ctxCol = strconv.Itoa(m.ContextLength)
		}
		created := "-"
		if t := m.CreatedTime(); !t.IsZero() {
			created = t.Format("2006-01-02")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", slug, ctxCol, formatPrice(m.PromptPricePerMillion()), strings.Join(m.InputModalities, ","), created)
	}
	return tw.Flush()
}

// formatPrice renders USD with up to 4 decimals, trailing zeros trimmed.
func formatPrice(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "0" {
		return "0"
	}
	return s
}
