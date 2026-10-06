package record

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// stat accumulates count, errors and latency for one tool or model.
type stat struct {
	n, errs int
	latMS   float64
	latN    int
}

func (s *stat) add(isErr bool, lat *float64) {
	s.n++
	if isErr {
		s.errs++
	}
	if lat != nil {
		s.latMS += *lat
		s.latN++
	}
}

// Summary is the aggregate of one or more recording files.
type Summary struct {
	Files     int
	Sessions  int
	Clients   map[string]int
	Tools     map[string]*stat
	Models    map[string]*stat
	BadLines  int
	ToolCalls int
}

// Summarize reads path (one .jsonl file, or every *.jsonl in a directory)
// and aggregates it. Unparseable lines are counted, not fatal.
func Summarize(path string) (*Summary, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	files := []string{path}
	if info.IsDir() {
		files, err = filepath.Glob(filepath.Join(path, "*.jsonl"))
		if err != nil {
			return nil, err
		}
	}
	s := &Summary{Clients: map[string]int{}, Tools: map[string]*stat{}, Models: map[string]*stat{}}
	for _, f := range files {
		if err := s.addFile(f); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Summary) addFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s.Files++
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<28)
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			s.BadLines++
			continue
		}
		switch rec.Kind {
		case KindSession:
			s.Sessions++
		case KindClient:
			if rec.Client != nil {
				s.Clients[rec.Client.Name+" "+rec.Client.Version]++
			}
		case KindToolCall:
			s.ToolCalls++
			// MCP records "jev_check", the CLI "check": count them as one tool.
			get(s.Tools, strings.TrimPrefix(rec.Tool, "jev_")).add(rec.IsError != nil && *rec.IsError, rec.LatencyMS)
		case KindSystemOne:
			get(s.Models, rec.Model).add(rec.Error != "", rec.LatencyMS)
		}
	}
	return sc.Err()
}

func get(m map[string]*stat, k string) *stat {
	if m[k] == nil {
		m[k] = &stat{}
	}
	return m[k]
}

// Render prints the summary as aligned tables.
func (s *Summary) Render(w io.Writer) {
	fmt.Fprintf(w, "files: %d  sessions: %d  tool calls: %d  skipped lines: %d\n", s.Files, s.Sessions, s.ToolCalls, s.BadLines)
	if len(s.Clients) > 0 {
		fmt.Fprintln(w, "\nclients:")
		for _, k := range sortedKeys(s.Clients) {
			fmt.Fprintf(w, "  %s: %d\n", k, s.Clients[k])
		}
	}
	table(w, "tool", s.Tools, s.ToolCalls)
	total := 0
	for _, m := range s.Models {
		total += m.n
	}
	table(w, "model", s.Models, total)
}

func table(w io.Writer, label string, m map[string]*stat, total int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]].n != m[keys[j]].n {
			return m[keys[i]].n > m[keys[j]].n
		}
		return keys[i] < keys[j]
	})
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tcalls\tshare\terrors\terror rate\tavg latency\n", label)
	for _, k := range keys {
		st := m[k]
		avg := "-"
		if st.latN > 0 {
			avg = fmt.Sprintf("%.0f ms", st.latMS/float64(st.latN))
		}
		fmt.Fprintf(tw, "%s\t%d\t%.0f%%\t%d\t%.0f%%\t%s\n", k, st.n,
			100*float64(st.n)/float64(max(total, 1)), st.errs, 100*float64(st.errs)/float64(st.n), avg)
	}
	tw.Flush()
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// NewCommand builds `jev record`, with its `summarize <file-or-dir>`
// subcommand. It reads local files only and needs no credentials.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Work with recordings made with --record",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "summarize <file-or-dir>",
		Short: "Print counts per tool and model, error rate and average latency",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := Summarize(args[0])
			if err != nil {
				return err
			}
			s.Render(cmd.OutOrStdout())
			return nil
		},
	})
	return cmd
}
