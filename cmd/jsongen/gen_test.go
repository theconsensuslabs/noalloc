package main

import (
	"strings"
	"testing"
)

// TestGenerateRefusesWhatItCannotMatch generates each testdata/shapes type
// alone and checks jsongen refuses those whose encoding or decoding would
// differ from encoding/json's, naming why, and accepts the rest.
func TestGenerateRefusesWhatItCannotMatch(t *testing.T) {
	for _, c := range []struct {
		write, read string
		refusal     string
	}{
		{read: "ReadBytes", refusal: "reading a slice of bytes"},
		{read: "ReadOctets", refusal: "reading a slice of bytes"},
		{read: "ReadBlob", refusal: "reading a slice of bytes"},
		{read: "ReadOctetArray"},
		{write: "WriteAppendOnly", refusal: "has AppendText and no MarshalText"},
		{write: "WriteAppendOnlyBytes", refusal: "has AppendText and no MarshalText"},
		{write: "WriteMarshalOnly", refusal: "has MarshalText, whose result it allocates"},
		{write: "WriteOctets"},
		{read: "Key256"},
		{read: "Key257", refusal: "a key longer than 256 bytes"},
		{write: "Key257"},
	} {
		var write, read []string
		if c.write != "" {
			write = []string{c.write}
		}
		if c.read != "" {
			read = []string{c.read}
		}
		src, err := Generate("testdata/shapes", "json_gen.go", write, read)
		switch {
		case c.refusal == "" && err != nil:
			t.Errorf("write %q read %q: refused: %v", c.write, c.read, err)
		case c.refusal != "" && (err == nil || !strings.Contains(err.Error(), c.refusal)):
			t.Errorf("write %q read %q: error %v, want one naming %q", c.write, c.read, err, c.refusal)
		case c.write == "WriteOctets" && strings.Count(string(src), "w.Base64(nojson.Bytes(") != 2:
			t.Errorf("WriteOctets is not written as base64:\n%s", src)
		}
	}
}
