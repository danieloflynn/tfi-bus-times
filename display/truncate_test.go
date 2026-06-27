package display

import "testing"

// TestTruncate locks the rune-aware truncation behaviour (fitting strings pass
// through; longer strings keep maxRunes-1 runes plus an ellipsis), including
// multibyte runes where a naive byte slice would split a character.
func TestTruncate(t *testing.T) {
	cases := []struct {
		s        string
		maxRunes int
		want     string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abcde", 5, "abcde"},   // exactly fits
		{"abcdef", 5, "abcd…"},  // first maxRunes-1 + ellipsis
		{"abcdefghij", 3, "ab…"},
		{"x", 1, "x"},
		{"xy", 1, "…"},          // maxRunes-1 == 0 runes kept
		{"abc", 0, ""},          // non-positive
		{"Dún Laoghaire", 4, "Dún…"}, // multibyte: 'ú' must not be split
		{"café", 4, "café"},     // exactly fits with multibyte
		{"caféx", 4, "caf…"},
	}
	for _, c := range cases {
		if got := truncate(c.s, c.maxRunes); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.s, c.maxRunes, got, c.want)
		}
	}
}
