package url

import (
	stdurl "net/url"
	"strings"
	"testing"
)

var stringSeeds = []string{
	"", "plain", "a b+c~d-e_f.g", "!#$&*+,/:;=?@[]", "%41%zz%4%", "%", "\x00", "\xff", "bad\xffbyte\xc3",
	"\u2028\u2029", "日本語", "http://example.com/a b?c=d&e=f#g", "\t\n\r\b\f\x01\x1f\x7f", strings.Repeat("&=", 40),
}

// FuzzQueryEscaped checks AppendQueryEscaped against url.QueryEscape.
func FuzzQueryEscaped(f *testing.F) {
	for _, s := range stringSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := string(AppendQueryEscaped([]byte("keep"), s)), "keep"+stdurl.QueryEscape(s); got != want {
			t.Errorf("AppendQueryEscaped(%q) = %q, url.QueryEscape = %q", s, got[4:], want[4:])
		}
	})
}

func TestQueryEscapedAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates for its own bookkeeping")
	}
	text := "plain <a href=\"x\">&'+</a>\x00 日本\xff\u2028 %41%zz !#$*,/:;=?@[] -._~\t\n\r\b\f\x01\\"
	dst := make([]byte, 0, 4096)
	if allocs := testing.AllocsPerRun(100, func() { dst = AppendQueryEscaped(dst[:0], text) }); allocs != 0 {
		t.Errorf("AppendQueryEscaped allocates %v times per call into a buffer with room", allocs)
	}
}
