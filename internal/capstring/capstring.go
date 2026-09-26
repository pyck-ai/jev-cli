// Package capstring implements the one small piece of input hygiene most
// of jev-mcp's tools need: truncating a caller-supplied text field to a
// hard rune-count cap before it goes anywhere near a prompt, so a
// pathologically large document/passage/diff can't blow past a tool's
// documented size budget or balloon the cost of a single SystemOne call.
//
// Every cap named in the project brief for the batch of tools built on
// top of the plugin architecture (jev_compare's 20,000-char passages,
// jev_extract's 50,000-char document, jev_review/jev_gate's 50,000-char
// request/diff/tests, jev_gate's 200,000-char aggregate evidence,
// jev_rerank's 100,000-char aggregate candidates, jev_match/jev_rerank's
// 2,000-char per-candidate text, ...) is a count of characters, not
// bytes, so Truncate counts runes -- consistent with how a human reading
// "50,000 characters" would count them, and safe against splitting a
// multi-byte UTF-8 sequence in half.
package capstring

// Truncate returns s cut down to at most max runes, and whether any
// truncation actually happened. max <= 0 is treated as "no cap" (s is
// returned unchanged, truncated=false), since none of this codebase's
// caps are ever meant to be zero-length.
func Truncate(s string, max int) (out string, truncated bool) {
	if max <= 0 {
		return s, false
	}
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]), true
}
