package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Case is one labelled, synthetic agent-side task.
type Case struct {
	ID         string   `json:"id"`
	Category   string   `json:"category"`
	Prompt     string   `json:"prompt"`
	Ideal      string   `json:"ideal"`
	Acceptable []string `json:"acceptable"`
	// Parallel lists '+'-joined sets of bare tool names, e.g. "decide+check".
	// A response with 2+ tool calls whose set of distinct tool names equals an
	// entry is acceptable (order and duplicates ignored).
	Parallel []string `json:"parallel,omitempty"`
	Notes    string   `json:"notes"`
}

// CaseFile is the on-disk shape of cases.json. The system prompt lives next to
// the cases so it is versioned with them.
type CaseFile struct {
	SystemPrompt string `json:"system_prompt"`
	Cases        []Case `json:"cases"`
}

// accepts reports whether tool (bare name) is an acceptable pick; the ideal
// tool is always acceptable.
func (c Case) accepts(tool string) bool {
	if tool == c.Ideal {
		return true
	}
	for _, a := range c.Acceptable {
		if a == tool {
			return true
		}
	}
	return false
}

// parallelSet splits a Parallel entry into its sorted, de-duplicated names.
func parallelSet(entry string) []string {
	return distinctSorted(strings.Split(entry, "+"))
}

func distinctSorted(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// acceptsParallel reports whether the distinct set of tools (bare names, from
// 2+ calls) equals one of the case's Parallel entries.
func (c Case) acceptsParallel(tools []string) bool {
	if len(tools) < 2 {
		return false
	}
	got := strings.Join(distinctSorted(tools), "+")
	for _, p := range c.Parallel {
		if strings.Join(parallelSet(p), "+") == got {
			return true
		}
	}
	return false
}

func loadCases(path string) (*CaseFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf CaseFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cf, nil
}

// validateCases checks structure: unique ids, non-empty fields, tool names
// that exist in known, and prompts that do not name a tool.
func validateCases(cf *CaseFile, known []string) error {
	if strings.TrimSpace(cf.SystemPrompt) == "" {
		return fmt.Errorf("system_prompt is empty")
	}
	set := map[string]bool{"none": true} // "none": a control case where no tool call is correct
	for _, k := range known {
		set[k] = true
	}
	seen := map[string]bool{}
	for _, c := range cf.Cases {
		if c.ID == "" || seen[c.ID] {
			return fmt.Errorf("case id %q is empty or duplicated", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Prompt) == "" || c.Category == "" || c.Notes == "" {
			return fmt.Errorf("case %s: prompt, category and notes are required", c.ID)
		}
		if !set[c.Ideal] {
			return fmt.Errorf("case %s: ideal %q is not a known tool", c.ID, c.Ideal)
		}
		for _, a := range c.Acceptable {
			if !set[a] {
				return fmt.Errorf("case %s: acceptable %q is not a known tool", c.ID, a)
			}
		}
		for _, p := range c.Parallel {
			names := parallelSet(p)
			if len(names) < 2 {
				return fmt.Errorf("case %s: parallel %q needs at least two distinct tools", c.ID, p)
			}
			for _, n := range names {
				if n == "none" || !set[n] {
					return fmt.Errorf("case %s: parallel %q names %q, which is not a known tool", c.ID, p, n)
				}
			}
		}
		if strings.Contains(strings.ToLower(c.Prompt), "jev") {
			return fmt.Errorf("case %s: prompt must not name a tool or the server", c.ID)
		}
	}
	return nil
}
