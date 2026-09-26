package capstring

import "testing"

func TestTruncate(t *testing.T) {
	cases := []struct {
		name          string
		s             string
		max           int
		wantOut       string
		wantTruncated bool
	}{
		{"under cap", "hello", 10, "hello", false},
		{"exactly at cap", "hello", 5, "hello", false},
		{"over cap", "hello world", 5, "hello", true},
		{"zero cap means no cap", "hello", 0, "hello", false},
		{"negative cap means no cap", "hello", -1, "hello", false},
		{"multi-byte runes counted, not bytes", "héllo wörld", 6, "héllo ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, truncated := Truncate(c.s, c.max)
			if out != c.wantOut || truncated != c.wantTruncated {
				t.Errorf("Truncate(%q, %d) = %q, %v; want %q, %v", c.s, c.max, out, truncated, c.wantOut, c.wantTruncated)
			}
		})
	}
}
