package json

import (
	"bytes"
	stdjson "encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/theconsensuslabs/noalloc/bounded"
)

var stringSeeds = []string{
	"", "plain", `<script>alert('x' + "y")</script>`, "a&b&amp;c", "\x00", "nul\x00inside", "\xff",
	"bad\xffbyte\xc3", "  ", "﷐￿�", "日本語", "\t\n\r\b\f\x01\x1f\x7f",
	`back\slash"quote`, "caf\xc3\xa9", "x\xe2\x80",
}

// FuzzAppendString checks that what AppendString writes decodes to what
// json.Marshal's output for the string decodes to, holds nothing a script
// would read as markup, and holds what AppendEscaped writes between its
// quotes.
func FuzzAppendString(f *testing.F) {
	for _, s := range stringSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		marshaled, _ := stdjson.Marshal(s)
		got := AppendString(nil, s)
		var want, decoded string
		if err := stdjson.Unmarshal(marshaled, &want); err != nil {
			t.Fatal(err)
		}
		if err := stdjson.Unmarshal(got, &decoded); err != nil || decoded != want {
			t.Fatalf("AppendString(%q) = %s, which decodes to %q (%v); json.Marshal's decodes to %q", s, got, decoded, err, want)
		}
		if bytes.ContainsAny(got, "<>&\u2028\u2029") {
			t.Fatalf("AppendString(%q) = %s holds markup", s, got)
		}
		if esc := AppendEscaped([]byte("keep"), s); !bytes.Equal(esc[4:], got[1:len(got)-1]) {
			t.Fatalf("AppendEscaped(%q) = %s, AppendString %s", s, esc[4:], got)
		}
	})
}

// FuzzEscapedLen checks the length a fixed buffer is sized by: what
// AppendEscaped writes, and no more than six bytes an input byte, which a
// caller cutting a string into pieces relies on.
func FuzzEscapedLen(f *testing.F) {
	for _, s := range stringSeeds {
		f.Add(s)
	}
	f.Add(strings.Repeat("\xff< \b", 50))
	// One byte of each class, so a class counted past six bytes shows alone.
	for _, s := range []string{`"`, `\`, "\n", "\r", "\t", "\b", "\x01", "<", ">", "&", "\xff", " ", " ", "a", "é"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		bound, wrote := EscapedLen(s), len(AppendEscaped(nil, s))
		if wrote != bound || bound > 6*len(s) {
			t.Fatalf("EscapedLen(%q) = %d; AppendEscaped wrote %d, and the most allowed is %d", s, bound, wrote, 6*len(s))
		}
	})
}

var floatSeeds = []float64{
	0, math.Copysign(0, -1), 1, -1, 0.1, 123.456, 1e-6, 9.999999e-7, 1e-7, 1e20, 1e21, 1.5e300,
	math.MaxFloat64, math.SmallestNonzeroFloat64, math.MaxFloat32, math.SmallestNonzeroFloat32,
	math.Inf(1), math.Inf(-1), math.NaN(), 16777217, 1e-9, 5e-324,
}

// FuzzAppendFloat checks that AppendFloat refuses what json.Marshal refuses
// and that what it writes decodes to the same float.
func FuzzAppendFloat(f *testing.F) {
	for _, v := range floatSeeds {
		f.Add(math.Float64bits(v))
		f.Add(uint64(math.Float32bits(float32(v))))
	}
	f.Fuzz(func(t *testing.T, bits uint64) {
		f64 := math.Float64frombits(bits)
		f32 := math.Float32frombits(uint32(bits))
		for _, c := range []struct {
			v    any
			f    float64
			size int
			back any
		}{{f64, f64, 64, new(float64)}, {f32, float64(f32), 32, new(float32)}} {
			_, err := stdjson.Marshal(c.v)
			got, ok := AppendFloat([]byte("keep"), c.f, c.size)
			if ok != (err == nil) {
				t.Fatalf("AppendFloat(%v, %d) ok %v, json.Marshal error %v", c.f, c.size, ok, err)
			}
			if !ok {
				if string(got) != "keep" {
					t.Fatalf("AppendFloat(%v, %d) appended %q to a refusal", c.f, c.size, got)
				}
				continue
			}
			if err := stdjson.Unmarshal(got[4:], c.back); err != nil {
				t.Fatalf("AppendFloat(%v, %d) = %s: %v", c.f, c.size, got[4:], err)
			}
			back := reflect.ValueOf(c.back).Elem().Interface()
			if back != c.v || math.Signbit(reflect.ValueOf(back).Float()) != math.Signbit(c.f) {
				t.Fatalf("AppendFloat(%v, %d) = %s, which decodes to %v", c.f, c.size, got[4:], back)
			}
		}
	})
}

func TestEscapersAllocateNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates for its own bookkeeping")
	}
	// The input reaches every branch of the functions it is given to.
	text := "plain <a href=\"x\">&'+</a>\x00 日本\xff  %41%zz !#$*,/:;=?@[] -._~\t\n\r\b\f\x01\\"
	dst := make([]byte, 0, 4096)
	var buf bounded.Buf
	for name, run := range map[string]func(){
		"AppendString": func() { dst = AppendString(dst[:0], text) },
		"EscapedLen":   func() { dst = dst[:EscapedLen(text)%cap(dst)] },
		"WriteString": func() {
			buf.Reset(dst[:cap(dst)], nil)
			WriteString(&buf, text)
		},
		"AppendFloat": func() {
			for _, f := range floatSeeds {
				dst, _ = AppendFloat(dst[:0], f, 64)
				dst, _ = AppendFloat(dst, float64(float32(f)), 32)
			}
		},
	} {
		if allocs := testing.AllocsPerRun(100, run); allocs != 0 {
			t.Errorf("%s allocates %v times per call into a buffer with room", name, allocs)
		}
	}
}

// TestWriteStringWritesWhatAppendStringAppends compares the two on strings of
// each kind of byte they escape and on random bytes, and checks that a Buf
// too small for the result is full.
func TestWriteStringWritesWhatAppendStringAppends(t *testing.T) {
	cases := []string{"", "plain", `a"b\c`, "<a&b>", "\b\f\n\r\t\x00\x1f", "  ", "caf\xc3\xa9", "\xff\xfe", "x\xe2\x80"}
	r := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		b := make([]byte, r.IntN(24))
		for i := range b {
			b[i] = byte(r.IntN(256))
		}
		cases = append(cases, string(b))
	}
	var buf bounded.Buf
	mem := make([]byte, 6*24+2)
	for _, s := range cases {
		buf.Reset(mem, nil)
		WriteString(&buf, s)
		want := string(AppendString(nil, s))
		if got := string(buf.Bytes()); got != want || buf.Full() {
			t.Fatalf("WriteString(%q) wrote %q (full %v), AppendString appends %q", s, got, buf.Full(), want)
		}
		if len(want) > 1 {
			buf.Reset(mem[:len(want)-1], nil)
			WriteString(&buf, s)
			if !buf.Full() {
				t.Fatalf("WriteString(%q) into %d bytes, one short, is not full", s, len(want)-1)
			}
		}
	}
}

// text is a string written through AppendText and MarshalText.
type text string

func (s text) MarshalText() ([]byte, error) { return []byte(s), nil }

// FuzzEveryEscaperMatchesMarshal holds each way the package escapes a string
// to the bytes json.Marshal writes for it: AppendString, WriteString into
// memory and through a flush writer, a Writer's String and TextEnd, and
// QuotedString against a ",string" field.
func FuzzEveryEscaperMatchesMarshal(f *testing.F) {
	for _, s := range stringSeeds {
		f.Add(s)
	}
	for c := range utf8.RuneSelf {
		f.Add(string(rune(c)) + "x")
	}
	f.Fuzz(func(t *testing.T, s string) {
		want, err := stdjson.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		check := func(name string, got []byte, want []byte) {
			t.Helper()
			if !bytes.Equal(got, want) {
				t.Fatalf("%s(%q) = %s, json.Marshal %s", name, s, got, want)
			}
		}
		check("AppendString", AppendString(nil, s), want)

		var buf bounded.Buf
		buf.Reset(make([]byte, len(want)), nil)
		WriteString(&buf, s)
		check("WriteString", buf.Bytes(), want)
		var out bytes.Buffer
		buf.Reset(make([]byte, 8), &out)
		WriteString(&buf, s)
		buf.Flush()
		check("WriteString streamed", out.Bytes(), want)

		var w Writer
		w.Reset(make([]byte, len(want)), nil)
		w.String(s)
		check("Writer.String", w.Bytes(), want)

		if len(s) <= textBytes {
			wantText, err := stdjson.Marshal(text(s))
			if err != nil {
				t.Fatal(err)
			}
			w.Reset(make([]byte, len(wantText)), nil)
			w.TextEnd(append(w.TextBegin(), s...), nil, "")
			check("TextEnd", w.Bytes(), wantText)
		}

		wantQuoted, err := stdjson.Marshal(struct {
			S string `json:",string"`
		}{s})
		if err != nil {
			t.Fatal(err)
		}
		w.Reset(make([]byte, len(wantQuoted)), nil)
		w.Raw(`{"S":`)
		w.QuotedString(s)
		w.Raw(`}`)
		check("QuotedString", w.Bytes(), wantQuoted)
	})
}

// TestEscapedLenIsExact checks EscapedLen against what AppendEscaped appends
// over every byte and the multibyte runes escaped.
func TestEscapedLenIsExact(t *testing.T) {
	cases := []string{" ", " ", "\xff", "\xe2\x80", "日"}
	for c := range 256 {
		cases = append(cases, string([]byte{byte(c)}), strings.Repeat(string([]byte{byte(c)}), 9))
	}
	for _, s := range cases {
		if n, wrote := EscapedLen(s), len(AppendEscaped(nil, s)); n != wrote {
			t.Errorf("EscapedLen(%q) = %d, AppendEscaped wrote %d", s, n, wrote)
		}
	}
}
