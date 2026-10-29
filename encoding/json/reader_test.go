package json_test

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/theconsensuslabs/noalloc/encoding/json"
)

// longKey is set by decodeAny on a key longer than the Reader holds, which
// it cannot put in a map.
var longKey bool

// decodeAny decodes the next value as json.Unmarshal decodes into an any.
func decodeAny(r *json.Reader) any {
	switch r.Peek() {
	case json.ObjectKind:
		m := map[string]any{}
		if r.ObjectStart("") {
			for r.ObjectNext() {
				k := strings.Clone(r.Key())
				longKey = longKey || k == "\x00"
				m[k] = decodeAny(r)
			}
		}
		return m
	case json.ArrayKind:
		a := []any{}
		if r.ArrayStart("") {
			for r.ArrayNext() {
				a = append(a, decodeAny(r))
			}
		}
		return a
	case json.StringKind:
		var s string
		r.String(&s, "")
		return s
	case json.NumberKind:
		f, _ := r.Float("", 64)
		return f
	case json.BoolKind:
		b, _ := r.Bool("")
		return b
	case json.NullKind:
		r.Null()
		return nil
	}
	r.Skip()
	return nil
}

// giveArena gives r's Arena a little memory and a More that makes more.
func giveArena(r *json.Reader) {
	r.Arena.More = func(n int) []byte { return make([]byte, 0, n) }
	r.Arena.Reset(make([]byte, 0, 8))
}

// readerBuffer is the buffer the streamed decodes read through: short, so
// tokens cross its end, and long enough for the numbers the seeds hold.
const readerBuffer = 64

// decodeBoth decodes data from memory and from a reader one byte a read,
// through readerBuffer bytes. Streamed is false for input with a number or
// key too long for that buffer.
func decodeBoth(data []byte) (whole, streamed any, wholeErr, streamErr error, compared bool) {
	var r json.Reader
	r.ResetBytes(data)
	giveArena(&r)
	whole = decodeAny(&r)
	wholeErr = r.Finish()

	r.Reset(iotest.OneByteReader(bytes.NewReader(data)), make([]byte, readerBuffer))
	giveArena(&r)
	streamed = decodeAny(&r)
	streamErr = r.Finish()
	var e *json.Error
	compared = !(errors.As(streamErr, &e) && e.Kind == json.TooLong)
	return
}

var readerSeeds = []string{
	`null`, `true`, `false`, `0`, `-0`, `1.5e3`, `-1E-2`, `""`, `"a\"b\\c\/\b\f\n\r\t"`, `"é☃"`,
	`"😀"`, `"\ud83d"`, `"\ude00x"`, `"\ud83dA"`, "\"\xff\xfe\"", "\"caf\xc3\xa9\"", `[]`, `{}`,
	`[1,2,[3,{"a":[]}]]`, `{"a":1,"a":2}`, `{"a":{"b":{"c":"d"}}}`, ` { "k" : [ true , null ] } `, `1e400`, `-1e400`,
	`1e-400`, `123456789012345678901234567890`, `[1,]`, `{"a":1,}`, `[,1]`, `{,}`, `01`, `1.`, `.5`, `-`, `+1`, `1e`,
	`"abc`, `[1,2`, `{"a"`, `{"a":`, `tru`, `nul`, `"\x"`, `"\u12"`, "\"\x01\"", `[1 2]`, `{"a" 1}`, `1 2`, `"a" x`,
	`{"a":1}}`, `]`, `}`, `,`, "\" \"", `{"` + strings.Repeat("k", 300) + `":1}`,
}

// FuzzReaderMatchesUnmarshal decodes JSON into an any through the Reader,
// from memory and streamed, and compares both with json.Unmarshal: both fail
// or neither does, and they decode the same value. Validity is compared with
// json.Valid and jsontext.
func FuzzReaderMatchesUnmarshal(f *testing.F) {
	for _, s := range readerSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var want any
		wantErr := stdjson.Unmarshal(data, &want)
		longKey = false
		whole, streamed, wholeErr, streamErr, compared := decodeBoth(data)
		if longKey {
			return
		}
		if (wholeErr == nil) != (wantErr == nil) {
			t.Fatalf("%q: Reader error %v, json.Unmarshal error %v", data, wholeErr, wantErr)
		}
		if wantErr == nil && !reflect.DeepEqual(whole, want) {
			t.Fatalf("%q: Reader decodes %#v, json.Unmarshal %#v", data, whole, want)
		}
		if compared {
			if (streamErr == nil) != (wholeErr == nil) || streamErr != nil && kindOf(streamErr) != kindOf(wholeErr) {
				t.Fatalf("%q: streamed error %v, from memory %v", data, streamErr, wholeErr)
			}
			if streamErr == nil && !reflect.DeepEqual(streamed, whole) {
				t.Fatalf("%q: streamed %#v, from memory %#v", data, streamed, whole)
			}
		}

		var r json.Reader
		r.ResetBytes(data)
		r.Skip()
		valid := r.Finish() == nil
		if std := stdjson.Valid(data); valid != std {
			t.Fatalf("%q: Skip finds it valid %v, json.Valid %v", data, valid, std)
		}
		if text, ok := textValid(data); ok && valid != text {
			t.Fatalf("%q: Skip finds it valid %v, jsontext %v", data, valid, text)
		}
	})
}

func kindOf(err error) json.ErrorKind {
	var e *json.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// FuzzNumbersMatchUnmarshal reads a number into each int, uint, and float
// size and compares each with json.Unmarshal into that type.
func FuzzNumbersMatchUnmarshal(f *testing.F) {
	for _, s := range []string{"0", "-0", "1", "-1", "127", "128", "-128", "-129", "255", "256", "32767", "65535", "65536",
		"2147483647", "2147483648", "-2147483648", "-2147483649", "4294967295", "4294967296", "9223372036854775807",
		"9223372036854775808", "-9223372036854775808", "-9223372036854775809", "18446744073709551615", "18446744073709551616",
		"1.0", "1e2", "1E+2", "-1e-2", "3.4028234663852886e38", "3.4028235677973366e38", "1.7976931348623157e308",
		"1.7976931348623159e308", "4.9e-324", "2e-324", "1e-46", "0.1", "123456789012345678901234567890e-10"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !stdjson.Valid([]byte(s)) || len(s) == 0 || !(s[0] == '-' || '0' <= s[0] && s[0] <= '9') {
			return
		}
		check := func(name string, got any, gotOK bool, dst any) {
			err := stdjson.Unmarshal([]byte(s), dst)
			want := reflect.ValueOf(dst).Elem().Interface()
			if gotOK != (err == nil) {
				t.Fatalf("%s %s: Reader ok %v (%v), json.Unmarshal error %v (%v)", name, s, gotOK, got, err, want)
			}
			if gotOK && !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %s: Reader %v, json.Unmarshal %v", name, s, got, want)
			}
		}
		var r json.Reader
		read := func() *json.Reader { r.ResetBytes([]byte(s)); return &r }
		v8, ok := read().Int("", 8)
		check("int8", int8(v8), ok, new(int8))
		v16, ok := read().Int("", 16)
		check("int16", int16(v16), ok, new(int16))
		v32, ok := read().Int("", 32)
		check("int32", int32(v32), ok, new(int32))
		v64, ok := read().Int("", 64)
		check("int64", v64, ok, new(int64))
		u8, ok := read().Uint("", 8)
		check("uint8", uint8(u8), ok, new(uint8))
		u16, ok := read().Uint("", 16)
		check("uint16", uint16(u16), ok, new(uint16))
		u32, ok := read().Uint("", 32)
		check("uint32", uint32(u32), ok, new(uint32))
		u64, ok := read().Uint("", 64)
		check("uint64", u64, ok, new(uint64))
		f32, ok := read().Float("", 32)
		check("float32", float32(f32), ok, new(float32))
		f64, ok := read().Float("", 64)
		check("float64", f64, ok, new(float64))
		if ok && math.Signbit(f64) != (s[0] == '-') {
			t.Fatalf("float64 %s: sign of %v", s, f64)
		}
	})
}

// strictTime is the form of RFC 3339 that time.Time's UnmarshalJSON parses
// before falling back on time.Parse, and the only one the Reader decodes. The
// offset is held to RFC 3339's range because time.Parse accepts up to +24:60.
var strictTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]([01]\d|2[0-3]):[0-5]\d)$`)

// FuzzTimeMatchesUnmarshal reads a string as a time and compares it with
// json.Unmarshal into a time.Time: the same instant, in UTC, for every time in
// strictTime's form.
func FuzzTimeMatchesUnmarshal(f *testing.F) {
	for _, s := range []string{"2026-10-27T12:34:56Z", "2026-10-27T12:34:56.123456789Z", "2026-10-27T12:34:56.1Z",
		"2024-02-29T00:00:00Z", "2023-02-29T00:00:00Z", "0000-01-01T00:00:00Z", "9999-12-31T23:59:59.999999999Z",
		"2026-10-27T24:00:00Z", "2026-10-27T12:34:60Z", "2026-10-27t12:34:56z", "2026-10-27T12:34:56,5Z",
		"2026-10-27T12:34:56.Z", "2026-10-27T12:34:56.1234567890Z", "2026-10-27T12:34:56+00:00", "2026-1-27T12:34:56Z",
		"2026-10-27T12:34:56+00:60", "2026-10-27T12:34:56-24:60", "2026-10-27T12:34:56+25:00", "2026-10-27T12:34:56+00:61"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		data, _ := stdjson.Marshal(s)
		var want time.Time
		wantErr := stdjson.Unmarshal(data, &want)
		var r json.Reader
		r.ResetBytes(data)
		var got time.Time
		r.Time(&got, "")
		gotErr := r.Finish()
		switch {
		case gotErr == nil && wantErr != nil:
			t.Fatalf("%q: Reader %v, json.Unmarshal error %v", s, got, wantErr)
		case gotErr == nil && (!got.Equal(want) || got.Location() != time.UTC):
			t.Fatalf("%q: Reader %v, json.Unmarshal %v", s, got, want)
		case gotErr != nil && wantErr == nil && strictTime.MatchString(s):
			t.Fatalf("%q: Reader error %v, json.Unmarshal %v", s, gotErr, want)
		}
	})
}

// TestKeysUpTo256BytesAreHeldWhole pins the key length jsongen refuses
// fields past: a key of 256 bytes, escaped or not, reads whole, and one of
// 257 reads as "\x00", which matches no field.
func TestKeysUpTo256BytesAreHeldWhole(t *testing.T) {
	for _, c := range []struct {
		json, key string
	}{
		{strings.Repeat("k", 256), strings.Repeat("k", 256)},
		{strings.Repeat("k", 255) + `é`, ""},
		{strings.Repeat("k", 254) + `é`, strings.Repeat("k", 254) + "é"},
		{strings.Repeat("k", 257), "\x00"},
	} {
		var r json.Reader
		r.ResetBytes([]byte(`{"` + c.json + `":1}`))
		if !r.ObjectStart("") || !r.ObjectNext() {
			t.Fatalf("%d-byte key: no member read: %v", len(c.json), r.Err())
		}
		want := c.key
		if want == "" {
			want = "\x00"
		}
		if got := r.Key(); got != want {
			t.Errorf("key %.10q... of %d bytes read as %d bytes", c.json, len(c.json), len(got))
		}
	}
}
