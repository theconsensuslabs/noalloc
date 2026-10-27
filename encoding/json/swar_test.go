package json_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/theconsensuslabs/noalloc/bounded"
	"github.com/theconsensuslabs/noalloc/encoding/json"
)

// TestEveryByteAtEveryWordOffset puts each kind of byte the 8-byte scans stop
// at, at each offset of a 24-byte string, so it falls at every place in a
// word and across word and buffer edges, and checks the escapers by the
// round trip and the Reader against json.Unmarshal, from memory and through
// a 16-byte buffer one byte a read.
func TestEveryByteAtEveryWordOffset(t *testing.T) {
	specials := []string{`"`, `\`, "\x00", "\x01", "\x1f", "\x7f", "<", ">", "&", "\x80", "\xff", "é", " ",
		" ", "�", "\U0001F600", "\xe2\x80"}
	for _, sp := range specials {
		for at := range 24 {
			s := strings.Repeat("a", at) + sp + strings.Repeat("b", 24-at)
			var want string
			marshaled, _ := stdjson.Marshal(s)
			if err := stdjson.Unmarshal(marshaled, &want); err != nil {
				t.Fatal(err)
			}

			got := json.AppendString(nil, s)
			var back string
			if err := stdjson.Unmarshal(got, &back); err != nil || back != want {
				t.Fatalf("%q at %d: AppendString %s decodes to %q (%v), want %q", sp, at, got, back, err, want)
			}
			if bytes.ContainsAny(got, "<>&\u2028\u2029") {
				t.Fatalf("%q at %d: AppendString %s holds markup", sp, at, got)
			}
			var buf bounded.Buf
			buf.Reset(make([]byte, 256), nil)
			json.WriteString(&buf, s)
			if !bytes.Equal(buf.Bytes(), got) {
				t.Fatalf("%q at %d: WriteString %s, AppendString %s", sp, at, buf.Bytes(), got)
			}
			if n := json.EscapedLen(s); n < len(got)-2 || n > 6*len(s) {
				t.Fatalf("%q at %d: EscapedLen %d, AppendEscaped wrote %d", sp, at, n, len(got)-2)
			}

			for _, doc := range [][]byte{got, marshaled} {
				var r json.Reader
				var whole, streamed string
				r.ResetBytes(doc)
				giveArena(&r)
				r.String(&whole, "")
				if err := r.Finish(); err != nil || whole != want {
					t.Fatalf("%q at %d: Reader decodes %s as %q (%v), want %q", sp, at, doc, whole, err, want)
				}
				r.Reset(iotest.OneByteReader(bytes.NewReader(doc)), make([]byte, json.MinBuffer))
				giveArena(&r)
				r.String(&streamed, "")
				if err := r.Finish(); err != nil || streamed != want {
					t.Fatalf("%q at %d: streamed Reader decodes %s as %q (%v), want %q", sp, at, doc, streamed, err, want)
				}
			}
		}
	}
}
