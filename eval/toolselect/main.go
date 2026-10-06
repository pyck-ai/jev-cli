// Command toolselect is an offline tool-selection eval for the jev MCP server.
//
// It builds the tool list exactly as an MCP client sees it (in-process server,
// in-memory client, tools/list), then for every labelled case in cases.json
// asks one or more agent models which tool they would call, and scores the
// first tool call against the case's ideal/acceptable labels. Nothing here
// ever invokes a jev tool handler. See README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type config struct {
	baseURL     string
	apiKeyEnv   string
	direct      bool
	prefix      string
	models      []string
	casesPath   string
	filter      string
	out         string
	reps        int
	concurrency int
	timeout     time.Duration
	maxTokens   int
	jsonOut     bool
	yes         bool
	maxUSD      float64
	showTools   bool
}

const (
	defaultProxy   = "http://127.0.0.1:53986"
	openRouterBase = "https://openrouter.ai/api/v1"
)

// setSchemaCompat mirrors the first statement of cmd/jev/main.go: without it
// google/jsonschema-go infers optional slices as "type":["null","array"],
// which is not what MCP clients of the real binary see. Must run before any
// schema inference.
func setSchemaCompat() {
	if err := os.Setenv("JSONSCHEMAGODEBUG", "typeschemasnull=1"); err != nil {
		fmt.Fprintf(os.Stderr, "toolselect: warning: could not set JSONSCHEMAGODEBUG: %v\n", err)
	}
}

func main() {
	setSchemaCompat()
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "toolselect:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	var c config
	var models string
	fs := flag.NewFlagSet("toolselect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.baseURL, "base-url", "", "API base URL (default $PYCKLLM_BASE_URL or "+defaultProxy+"; with --openrouter: "+openRouterBase+")")
	fs.StringVar(&c.apiKeyEnv, "api-key-env", "", "env var holding the API key (default PYCKLLM_API_KEY; with --openrouter: OPENROUTER_API_KEY)")
	fs.BoolVar(&c.direct, "openrouter", false, "direct OpenRouter mode: every model goes to <base>/chat/completions")
	fs.StringVar(&c.prefix, "prefix", "jev_", "prefix the agent host adds to MCP tool names (opencode: server name + _)")
	fs.StringVar(&models, "models", "~anthropic/claude-haiku-latest", "comma-separated agent model ids")
	fs.StringVar(&c.casesPath, "cases", "eval/toolselect/cases.json", "path to the cases file")
	fs.StringVar(&c.filter, "filter", "", "only run cases whose id contains this substring")
	fs.StringVar(&c.out, "out", "", "JSONL output path (default /tmp/opencode/jev-eval/runs/<timestamp>.jsonl)")
	fs.IntVar(&c.reps, "reps", 1, "repetitions per case and model")
	fs.IntVar(&c.concurrency, "concurrency", 4, "parallel requests")
	fs.DurationVar(&c.timeout, "timeout", 90*time.Second, "per-request timeout")
	fs.IntVar(&c.maxTokens, "max-tokens", 4096, "max_tokens per request (reasoning models need headroom)")
	fs.BoolVar(&c.jsonOut, "json", false, "print the report as JSON instead of tables")
	fs.BoolVar(&c.yes, "yes", false, "run even when the cost estimate exceeds --max-usd")
	fs.Float64Var(&c.maxUSD, "max-usd", 1.0, "cost estimate above which --yes is required")
	fs.BoolVar(&c.showTools, "show-tools", false, "print the tool list as the agent sees it and exit (no network)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	for _, m := range strings.Split(models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			c.models = append(c.models, m)
		}
	}
	if c.baseURL == "" {
		switch {
		case c.direct:
			c.baseURL = openRouterBase
		case os.Getenv("PYCKLLM_BASE_URL") != "":
			c.baseURL = os.Getenv("PYCKLLM_BASE_URL")
		default:
			c.baseURL = defaultProxy
		}
	}
	if c.apiKeyEnv == "" {
		c.apiKeyEnv = "PYCKLLM_API_KEY"
		if c.direct {
			c.apiKeyEnv = "OPENROUTER_API_KEY"
		}
	}
	if c.reps < 1 || c.concurrency < 1 || len(c.models) == 0 {
		return c, fmt.Errorf("--reps and --concurrency must be >= 1 and --models must not be empty")
	}
	return c, nil
}

func run(args []string, stdout, stderr io.Writer) error {
	c, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	ctx := context.Background()
	fns, err := loadTools(ctx, c.prefix)
	if err != nil {
		return err
	}
	if c.showTools {
		return json.NewEncoder(stdout).Encode(fns)
	}
	cf, err := loadCases(c.casesPath)
	if err != nil {
		return err
	}
	known := make([]string, len(fns))
	for i, f := range fns {
		known[i] = bareName(f.Name, c.prefix)
	}
	if err := validateCases(cf, known); err != nil {
		return err
	}
	var cases []Case
	for _, cs := range cf.Cases {
		if c.filter == "" || strings.Contains(cs.ID, c.filter) {
			cases = append(cases, cs)
		}
	}
	if len(cases) == 0 {
		return fmt.Errorf("no cases match --filter %q", c.filter)
	}

	n := len(cases) * len(c.models) * c.reps
	est := estimateUSD(c.models, fns, cf.SystemPrompt, cases, c.reps)
	fmt.Fprintf(stderr, "%d trials (%d cases x %d models x %d reps), estimated cost ~$%.2f\n", n, len(cases), len(c.models), c.reps, est)
	if est > c.maxUSD && !c.yes {
		return fmt.Errorf("estimate $%.2f exceeds --max-usd $%.2f; re-run with --yes to proceed", est, c.maxUSD)
	}

	key := os.Getenv(c.apiKeyEnv)
	if key == "" {
		return fmt.Errorf("API key env %s is empty", c.apiKeyEnv)
	}
	out := c.out
	if out == "" {
		out = filepath.Join("/tmp/opencode/jev-eval/runs", time.Now().Format("20060102-150405")+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()

	caller := &Caller{
		HTTP:      &http.Client{Timeout: c.timeout},
		Endpoint:  Endpoint{BaseURL: c.baseURL, APIKey: key, Direct: c.direct},
		Prefix:    c.prefix,
		MaxTokens: c.maxTokens,
		Retries:   2,
	}
	trials := runTrials(ctx, caller, cf.SystemPrompt, cases, c.models, c.reps, c.concurrency, fns, c.prefix, f)
	fmt.Fprintf(stderr, "wrote %s\n", out)

	rep := summarize(trials)
	if c.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printReport(stdout, rep)
	return nil
}

// runTrials executes every case x model x rep with bounded concurrency, writing
// each finished trial to w as a JSONL line, and returns all trials in a stable
// order.
func runTrials(ctx context.Context, caller *Caller, system string, cases []Case, models []string, reps, conc int, fns []Function, prefix string, w io.Writer) []Trial {
	schemas := map[string]map[string]any{}
	for _, fn := range fns {
		schemas[bareName(fn.Name, prefix)] = fn.Parameters
	}
	type job struct {
		c     Case
		model string
		rep   int
		idx   int
	}
	var jobs []job
	for _, c := range cases {
		for _, m := range models {
			for r := 1; r <= reps; r++ {
				jobs = append(jobs, job{c, m, r, len(jobs)})
			}
		}
	}
	results := make([]Trial, len(jobs))
	ch := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				start := time.Now()
				obs, err := caller.Call(ctx, j.model, system, j.c.Prompt, fns)
				t := buildTrial(j.c, j.model, j.rep, obs, err, schemas)
				t.LatencyMs = time.Since(start).Milliseconds()
				results[j.idx] = t
				mu.Lock()
				_ = enc.Encode(t)
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	return results
}

// buildTrial scores one observation. schemas maps bare tool name to its input
// schema.
func buildTrial(c Case, model string, rep int, obs Observation, err error, schemas map[string]map[string]any) Trial {
	t := Trial{CaseID: c.ID, Category: c.Category, Model: model, Rep: rep, Ideal: c.Ideal}
	if err != nil {
		t.Verdict = VerdictError
		t.Error = err.Error()
		t.Chosen = "none"
		return t
	}
	t.Chosen = obs.Tool
	t.Args = obs.Args
	t.CostUSD = obs.Cost
	t.TokensIn = obs.PromptTokens
	t.TokensOut = obs.CompletionTokens
	t.FinishReason = obs.FinishReason
	t.Verdict = verdictFor(c, obs.Tool)
	if obs.Tool == "none" && obs.FinishReason == "length" {
		t.Verdict = VerdictTruncated
	}
	if obs.Tool == "none" {
		t.Text = truncate(obs.Text, 400)
	}
	t.Acceptable = t.Verdict == VerdictIdeal || t.Verdict == VerdictAcceptable
	if obs.Tool != "none" {
		schema, known := schemas[obs.Tool]
		var problems []string
		if !known {
			problems = []string{"unknown tool " + obs.RawTool}
		} else {
			problems = validateArgs(schema, obs.Args)
		}
		ok := len(problems) == 0
		t.SchemaValid = &ok
		t.SchemaProblem = problems
	}
	return t
}

func printReport(w io.Writer, rep Report) {
	fmt.Fprintf(w, "%-38s %6s %6s %6s %6s %7s %7s %6s %6s %8s %8s %8s\n", "model", "trials", "n_eff", "errors", "trunc%", "ideal%", "accept%", "escape%", "none%", "schema%", "lat(ms)", "cost$")
	for _, s := range rep.Models {
		fmt.Fprintf(w, "%-38s %6d %6d %6d %6.1f %7.1f %7.1f %6.1f %6.1f %8.1f %8.0f %8.4f\n",
			s.Model, s.Trials, s.NEffective, s.Errors, s.TruncPct, s.IdealPct, s.AcceptPct, s.EscapePct, s.NonePct, s.SchemaPct, s.MeanLatency, s.CostUSD)
	}
	if len(rep.Confusion) == 0 {
		return
	}
	fmt.Fprintln(w, "\nNon-acceptable picks (case: ideal -> chosen xN):")
	cur := ""
	for _, c := range rep.Confusion {
		if c.Model != cur {
			cur = c.Model
			fmt.Fprintf(w, "  [%s]\n", cur)
		}
		fmt.Fprintf(w, "    %-34s %-9s -> %s x%d\n", c.CaseID, c.Ideal, c.Chosen, c.Count)
	}
	// Per-model, per-chosen-tool counts give a quick confusion overview.
	fmt.Fprintln(w, "\nWrong-pick totals by ideal -> chosen:")
	tot := map[string]int{}
	for _, c := range rep.Confusion {
		tot[c.Ideal+" -> "+c.Chosen] += c.Count
	}
	keys := make([]string, 0, len(tot))
	for k := range tot {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if tot[keys[i]] != tot[keys[j]] {
			return tot[keys[i]] > tot[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		fmt.Fprintf(w, "  %-28s %d\n", k, tot[k])
	}
}
