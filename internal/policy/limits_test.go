// Clipping of untrusted text: bounded length, never a split UTF-8 sequence.

package policy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClip(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"short text unchanged", "orders", "orders"},
		{"exactly the limit unchanged", strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{"long text cut at the limit", strings.Repeat("a", 300), strings.Repeat("a", 256) + "…"},
		{"sequence across the limit kept whole", strings.Repeat("a", 255) + "é" + strings.Repeat("a", 10), strings.Repeat("a", 255) + "…"},
		{"sequence ending at the limit kept", strings.Repeat("a", 254) + "é" + "b", strings.Repeat("a", 254) + "é" + "…"},
	}
	for _, c := range cases {
		got := Clip(c.text)
		if got != c.want || !utf8.ValidString(got) {
			t.Errorf("%s: got %q", c.name, got)
		}
	}
}
