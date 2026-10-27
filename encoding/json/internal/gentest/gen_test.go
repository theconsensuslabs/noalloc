package gentest_test

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"testing/iotest"
	"time"

	"github.com/theconsensuslabs/noalloc/encoding/json"
	"github.com/theconsensuslabs/noalloc/encoding/json/internal/gentest"
	"github.com/theconsensuslabs/noalloc/encoding/json/jsontest"
)

type writable interface {
	WriteJSON(*json.Writer)
}

// write encodes v into memory, and streamed through a flush writer from 256
// bytes of memory, failing t when the two differ.
func write(t *testing.T, v writable) ([]byte, error) {
	var w json.Writer
	w.Reset(make([]byte, 1<<16), nil)
	v.WriteJSON(&w)
	err := w.Err()
	whole := bytes.Clone(w.Bytes())

	var out bytes.Buffer
	w.Reset(make([]byte, 256), &out)
	v.WriteJSON(&w)
	streamErr := w.Flush()
	if (err == nil) != (streamErr == nil) || err == nil && !bytes.Equal(whole, out.Bytes()) {
		t.Fatalf("%T: streamed %s (%v), into memory %s (%v)", v, out.Bytes(), streamErr, whole, err)
	}
	return whole, err
}

// FuzzWriteRoundTrips fills each type from fuzz input and checks that what
// WriteJSON writes decodes to what json.Marshal's output decodes to.
func FuzzWriteRoundTrips(f *testing.F) {
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{7, 200, 3, 0, 255, 1}, 40))
	f.Add([]byte("a string <&> with \u2028 and \xff bytes, enough of them to reach the deeper fields of each type"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, v := range []writable{new(gentest.Page), new(gentest.Written), new(gentest.Node), new(gentest.Cold),
			new(gentest.Cookie), new(gentest.Feed), new(gentest.Shapes)} {
			jsontest.Filler{}.Fill(v, data)
			if _, err := write(t, v); err != nil && !failsMarshal(v) {
				t.Fatalf("%T: WriteJSON fails with %v where json.Marshal succeeds", v, err)
			}
			jsontest.Check(t, v)
		}
	})
}

func failsMarshal(v any) bool {
	_, err := stdjson.Marshal(v)
	return err != nil
}

// newPage is a Page with memory to decode into: a pointer to fill, and
// slices with room.
func newPage() *gentest.Page {
	p := &gentest.Page{Next: new(string), Tags: make([]string, 0, 8)}
	p.Root.Children = make([]gentest.Node, 0, 8)
	return p
}

// giveArena gives r's Arena a little memory and a More that makes more.
func giveArena(r *json.Reader) {
	r.Arena.More = func(n int) []byte { return make([]byte, 0, n) }
	r.Arena.Reset(make([]byte, 0, 32))
}

// read decodes data into a new Page from memory and streamed one byte a read
// through 64 bytes, failing t when the two differ.
func read(t *testing.T, data []byte) (*gentest.Page, error) {
	got := newPage()
	var r json.Reader
	r.ResetBytes(data)
	giveArena(&r)
	got.ReadJSON(&r)
	err := r.Finish()

	streamed := newPage()
	r.Reset(iotest.OneByteReader(bytes.NewReader(data)), make([]byte, 64))
	giveArena(&r)
	streamed.ReadJSON(&r)
	streamErr := r.Finish()
	if kindOf(streamErr) != json.TooLong && (kindOf(streamErr) != kindOf(err) || err == nil && !reflect.DeepEqual(streamed, got)) {
		t.Fatalf("%q: streamed %+v (%v), from memory %+v (%v)", data, streamed, streamErr, got, err)
	}
	return got, err
}

func kindOf(err error) json.ErrorKind {
	var e *json.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// FuzzReadMatchesUnmarshal decodes fuzz input, and the JSON of a Page filled
// from it, into a Page with ReadJSON and with json.Unmarshal: both fail or
// neither does, and they decode the same value. Where ReadJSON fails for want
// of memory, or on a time it does not decode, nothing is compared.
func FuzzReadMatchesUnmarshal(f *testing.F) {
	for _, s := range []string{`{}`, `null`, `[]`, `{"title":"t","TITLE":"u","count":-3,"tags":["a","b"],"next":"n"}`,
		`{"id":"0a0b0c0d","created":"2026-10-27T12:34:56.5Z","root":{"name":"r","children":[{"name":"c","weight":1.5}]}}`,
		`{"pair":[1,2,3],"inline":{"a":1,"b":"x"},"-":"dash","Skipped":"no","level":-128,"u8":255,"f32":3.4e38}`,
		`{"count":1.5,"title":2,"ok":"true","u64":-1,"i16":40000}`, `{"tags":null,"next":null,"root":null}`,
		`{"id":"zz","created":"2026-10-27T12:34:56+01:00"}`} {
		f.Add([]byte(s), false)
	}
	f.Add([]byte("fill me"), true)
	f.Fuzz(func(t *testing.T, data []byte, fromValue bool) {
		if fromValue {
			var v gentest.Page
			jsontest.Filler{}.Fill(&v, data)
			var err error
			if data, err = stdjson.Marshal(&v); err != nil {
				return
			}
		}
		want := newPage()
		wantErr := stdjson.Unmarshal(data, want)
		got, err := read(t, data)
		if k := kindOf(err); k == json.Full || k == json.Unsupported {
			return
		}
		if (err == nil) != (wantErr == nil) {
			t.Fatalf("%q: ReadJSON error %v, json.Unmarshal error %v", data, err, wantErr)
		}
		if err == nil && !jsontest.Same(got, want) {
			t.Fatalf("%q:\nReadJSON       %+v\njson.Unmarshal %+v", data, got, want)
		}
	})
}

// TestReuseMatchesUnmarshal decodes two documents in turn into one Page, as a
// pooled value is reused, with ReadJSON and with json.Unmarshal, which
// decodes into a slice's elements as they were.
func TestReuseMatchesUnmarshal(t *testing.T) {
	docs := []string{
		`{"title":"one","tags":["a","b","c"],"root":{"name":"r","children":[{"name":"x","weight":1,"children":[]},{"name":"y"}]}}`,
		`{"tags":["d"],"root":{"children":[{"weight":2}]},"count":4}`,
	}
	// Each Page has room for children, and each child room for its own.
	pooled := func() *gentest.Page {
		p := newPage()
		children := make([]gentest.Node, 8)
		for i := range children {
			children[i].Children = make([]gentest.Node, 0, 4)
		}
		p.Root.Children = children[:0]
		return p
	}
	want, got := pooled(), pooled()
	var r json.Reader
	for _, doc := range docs {
		if err := stdjson.Unmarshal([]byte(doc), want); err != nil {
			t.Fatal(err)
		}
		r.ResetBytes([]byte(doc))
		giveArena(&r)
		got.ReadJSON(&r)
		if err := r.Finish(); err != nil {
			t.Fatal(err)
		}
		if !jsontest.Same(got, want) {
			t.Fatalf("after %s:\nReadJSON       %+v\njson.Unmarshal %+v", doc, got, want)
		}
	}
}

// sample is a Page that reaches every field.
func sample() *gentest.Page {
	next := "next page"
	p := &gentest.Page{Title: "Hello <world> & \u2028 caf\u00e9", Tags: []string{"rust", "go", "zig"}, Count: -12481,
		U8: 255, I16: -300, U64: 1 << 63, F32: 0.15625, OK: true, Level: 3, Kind: "post", Next: &next, Pair: [2]int{1, -1},
		Root: gentest.Node{Name: "root", Weight: 2.5, Children: []gentest.Node{{Name: "leaf", Weight: 1e-7}}}, Dash: "-"}
	p.ID = gentest.ID{0xde, 0xad, 0xbe, 0xef}
	p.Created = time.Date(2026, 10, 27, 12, 34, 56, 789000000, time.UTC)
	p.Inline.A, p.Inline.B = 7, "inline"
	return p
}

func TestSampleRoundTrips(t *testing.T) {
	p := sample()
	jsontest.Check(t, p)
	data, err := write(t, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := read(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("read back %+v, wrote %+v", got, p)
	}
}

// TestGeneratedCodeAllocatesNothing encodes and decodes a Page that reaches
// every field, into memory and streamed, and counts allocations.
func TestGeneratedCodeAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates for its own bookkeeping")
	}
	p := sample()
	data, _ := stdjson.Marshal(p)
	var w json.Writer
	mem := make([]byte, 4096)
	dst := newPage()
	var r json.Reader
	arenaMem := make([]byte, 0, 4096)
	var src bytes.Reader
	buf := make([]byte, 64)
	for name, run := range map[string]func(){
		"write": func() { w.Reset(mem, nil); p.WriteJSON(&w) },
		"write streamed": func() {
			w.Reset(mem[:256], discard{})
			p.WriteJSON(&w)
			w.Flush()
		},
		"read": func() {
			r.Arena.Reset(arenaMem)
			r.ResetBytes(data)
			dst.ReadJSON(&r)
			if err := r.Finish(); err != nil {
				t.Fatal(err)
			}
		},
		"read streamed": func() {
			r.Arena.Reset(arenaMem)
			src.Reset(data)
			r.Reset(&src, buf)
			dst.ReadJSON(&r)
			if err := r.Finish(); err != nil {
				t.Fatal(err)
			}
		},
	} {
		if allocs := testing.AllocsPerRun(100, run); allocs != 0 {
			t.Errorf("%s: %v allocations a run", name, allocs)
		}
	}
}

// TestLocalReadersAndWritersStayOnTheStack counts the allocations of an encode
// and decode whose Writer, Reader, and Arena are locals, which escape to the
// heap when the API keeps a pointer to one.
func TestLocalReadersAndWritersStayOnTheStack(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates for its own bookkeeping")
	}
	p, dst := sample(), newPage()
	maybe := 2.5
	written := &gentest.Written{Inner: &gentest.Inner{Depth: 3}, Quoted: 7, QuotedS: "a \"q\" <b>", Raw: []byte("raw bytes"),
		Maybe: &maybe, Optional: []int{1, 2}}
	mem, arenaMem := make([]byte, 4096), make([]byte, 0, 4096)
	if allocs := testing.AllocsPerRun(100, func() {
		if err := gentest.RoundTripLocals(p, dst, written, mem, arenaMem); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Errorf("%v allocations a round trip", allocs)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// TestFirstCallsAllocateNothing counts the allocations of the first encode
// and decode of Cold the process makes, which nothing warms up.
func TestFirstCallsAllocateNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates for its own bookkeeping")
	}
	c := &gentest.Cold{Name: "cold", Values: []int{1, 2, 3}, At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	mem, arenaMem, data := make([]byte, 512), make([]byte, 0, 512), []byte(`{"name":"x","values":[4,5],"at":"2026-01-02T03:04:05Z"}`)
	dst := &gentest.Cold{Values: make([]int, 0, 4)}
	var w json.Writer
	var r json.Reader
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	w.Reset(mem, nil)
	c.WriteJSON(&w)
	r.Arena.Reset(arenaMem)
	r.ResetBytes(data)
	dst.ReadJSON(&r)
	err := r.Finish()
	runtime.ReadMemStats(&after)
	if err != nil || w.Err() != nil {
		t.Fatal(err, w.Err())
	}
	if n := after.Mallocs - before.Mallocs; n != 0 {
		t.Errorf("the first encode and decode allocate %d times", n)
	}
}
