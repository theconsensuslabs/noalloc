// Package jsontest checks the WriteJSON and ReadJSON methods jsongen
// generates against encoding/json, by the round trip, and fills values of any
// type from fuzz input, so a user of the generator can hold their own types
// to its contract.
package jsontest

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/theconsensuslabs/noalloc/encoding/json"
)

// Writable is a type with a WriteJSON method.
type Writable interface{ WriteJSON(*json.Writer) }

// Readable is a type with a ReadJSON method.
type Readable interface{ ReadJSON(*json.Reader) }

// Compare encodes v, a pointer, with WriteJSON and checks the round trip
// against encoding/json's: WriteJSON fails where json.Marshal fails, and
// otherwise writes valid JSON with nothing a script would read as markup,
// which decodes, with encoding/json into v's type and into any, to what
// json.Marshal's output decodes to. When v also has a ReadJSON method,
// decoding the output with it gives that value too.
func Compare(v Writable) error {
	want, wantErr := stdjson.Marshal(v)
	var out bytes.Buffer
	var w json.Writer
	w.Reset(make([]byte, 4096), &out)
	v.WriteJSON(&w)
	gotErr := w.Flush()
	got := out.Bytes()
	if (wantErr == nil) != (gotErr == nil) {
		return fmt.Errorf("%T: json.Marshal error %v, WriteJSON error %v; WriteJSON wrote %s", v, wantErr, gotErr, got)
	}
	if wantErr != nil {
		return nil
	}
	if !stdjson.Valid(got) {
		return fmt.Errorf("%T: invalid JSON %s", v, got)
	}
	if bytes.ContainsAny(got, "<>&\u2028\u2029") {
		return fmt.Errorf("%T: unescaped markup in %s", v, got)
	}
	typ := reflect.TypeOf(v).Elem()
	a, b := reflect.New(typ), reflect.New(typ)
	errA, errB := stdjson.Unmarshal(want, a.Interface()), stdjson.Unmarshal(got, b.Interface())
	if (errA == nil) != (errB == nil) {
		return fmt.Errorf("%T: decoding json.Marshal's %s: %v; decoding WriteJSON's %s: %v", v, want, errA, got, errB)
	}
	if errA == nil && !reflect.DeepEqual(a.Interface(), b.Interface()) {
		return fmt.Errorf("%T decodes differently:\njson.Marshal %s\nWriteJSON    %s", v, want, got)
	}
	var x, y any
	errX, errY := stdjson.Unmarshal(want, &x), stdjson.Unmarshal(got, &y)
	if (errX == nil) != (errY == nil) || errX == nil && !reflect.DeepEqual(x, y) {
		return fmt.Errorf("%T decodes into any differently:\njson.Marshal %s\nWriteJSON    %s", v, want, got)
	}
	if _, ok := v.(Readable); ok && errA == nil {
		// encoding/json's decode gives every slice and pointer the room
		// ReadJSON decodes into; decoding the same output again must leave
		// the value as it is.
		c := reflect.New(typ)
		if err := stdjson.Unmarshal(got, c.Interface()); err != nil {
			return err
		}
		var r json.Reader
		r.Arena.More = func(n int) []byte { return make([]byte, 0, n) }
		r.ResetBytes(got)
		c.Interface().(Readable).ReadJSON(&r)
		if err := r.Finish(); err != nil {
			return fmt.Errorf("%T: ReadJSON of %s: %v", v, got, err)
		}
		if !Same(c.Interface(), a.Interface()) {
			return fmt.Errorf("%T: ReadJSON of %s decodes\n%+v\nencoding/json\n%+v", v, got, c.Interface(), a.Interface())
		}
	}
	return nil
}

// Same is reflect.DeepEqual of a and b with times compared as instants, as
// ReadJSON decodes a time as the same instant in UTC.
func Same(a, b any) bool { return sameValue(reflect.ValueOf(a), reflect.ValueOf(b)) }

func sameValue(a, b reflect.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	if a.Type() == timeType {
		return a.Interface().(time.Time).Equal(b.Interface().(time.Time))
	}
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return sameValue(a.Elem(), b.Elem())
	case reflect.Slice:
		if a.IsNil() != b.IsNil() {
			return false
		}
		fallthrough
	case reflect.Array:
		if a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !sameValue(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := range a.NumField() {
			if !sameValue(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(valueOf(a), valueOf(b))
}

// valueOf is v as an interface, reached through unsafe for an unexported
// field, which reflect will not hand out.
func valueOf(v reflect.Value) any {
	if !v.CanInterface() {
		v = reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	}
	return v.Interface()
}

// Check is Compare, failing t.
func Check(t testing.TB, v Writable) {
	t.Helper()
	if err := Compare(v); err != nil {
		t.Fatal(err)
	}
}

// A Filler sets values from fuzz input, reaching every kind and the types
// encoding/json treats specially, with values encoding/json refuses among
// them. Nesting stops at a fixed depth.
type Filler struct {
	// Interfaces holds, for an interface type with methods, the values a
	// field of it may hold; each is filled in turn. One with none stays nil.
	// The empty interface holds JSON's own kinds unless it has an entry.
	Interfaces map[reflect.Type][]any
}

// Fill sets the value v points to from data.
func (f Filler) Fill(v any, data []byte) {
	f.fill(reflect.ValueOf(v).Elem(), &source{data}, 0)
}

// source hands out the fuzzer's bytes as values, zeros once it runs out.
type source struct{ b []byte }

func (s *source) next() byte {
	if len(s.b) == 0 {
		return 0
	}
	c := s.b[0]
	s.b = s.b[1:]
	return c
}

func (s *source) uint64() uint64 {
	var v uint64
	for range 8 {
		v = v<<8 | uint64(s.next())
	}
	return v
}

func (s *source) string() string {
	n := min(int(s.next()%24), len(s.b))
	out := string(s.b[:n])
	s.b = s.b[n:]
	return out
}

var (
	timeType       = reflect.TypeFor[time.Time]()
	rawMessageType = reflect.TypeFor[stdjson.RawMessage]()
	numberType     = reflect.TypeFor[stdjson.Number]()
	emptyInterface = reflect.TypeFor[any]()

	raws = []stdjson.RawMessage{nil, {}, stdjson.RawMessage(` {"a" : [1, "<&>"] } `), stdjson.RawMessage(`" "`),
		stdjson.RawMessage("\" \""), stdjson.RawMessage(`[1,`), stdjson.RawMessage(`true false`), stdjson.RawMessage(`-0.0e+1`)}
	numbers = []stdjson.Number{"", "0", "-0", "12.5e-3", "1E400", "01", "1.", ".5", "+1", "-", "9007199254740993"}
	zones   = []*time.Location{time.UTC, time.FixedZone("half", -7*3600-30*60), time.FixedZone("secs", 3600+30),
		time.FixedZone("day", 24*3600), time.FixedZone("east", 14*3600)}
	years = []int{0, 1, 1970, 2026, 9999, 10000, -1}
	jsons = []any{nil, 0.0, "", true, []any{}, map[string]any{}, stdjson.Number("")}
)

// zone is one of zones, or an offset of any seconds: often under a minute
// either way, sometimes up to a day and past it.
func (s *source) zone() *time.Location {
	switch s.next() % 3 {
	case 0:
		return zones[int(s.next())%len(zones)]
	case 1:
		return time.FixedZone("", int(int8(s.next())))
	}
	return time.FixedZone("", int(int32(s.uint64())%(25*3600)))
}

func (f Filler) fill(v reflect.Value, s *source, depth int) {
	switch v.Type() {
	case timeType:
		at := time.Date(years[int(s.next())%len(years)], time.Month(1+s.next()%12), int(1+s.next()%28),
			int(s.next()%24), int(s.next()%60), int(s.next()%60), int(s.uint64()%1e9), s.zone())
		if s.next()%4 == 0 {
			at = time.Time{}
		}
		v.Set(reflect.ValueOf(at))
		return
	case rawMessageType:
		v.Set(reflect.ValueOf(raws[int(s.next())%len(raws)]))
		return
	case numberType:
		v.Set(reflect.ValueOf(numbers[int(s.next())%len(numbers)]))
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(s.next()%2 == 1)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(s.uint64()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(s.uint64())
	case reflect.Float32, reflect.Float64:
		x := math.Float64frombits(s.uint64())
		if s.next()%8 != 0 {
			// Mostly finite and short, sometimes any bits at all.
			x = float64(int64(s.uint64())%100000) / float64(1+s.next()%8)
		}
		v.SetFloat(x)
	case reflect.Complex64, reflect.Complex128:
		if s.next()%8 == 0 {
			v.SetComplex(1)
		}
	case reflect.String:
		v.SetString(s.string())
	case reflect.Chan:
		if s.next()%8 == 0 {
			v.Set(reflect.MakeChan(v.Type(), 0))
		}
	case reflect.Func:
		if s.next()%8 == 0 {
			v.Set(reflect.MakeFunc(v.Type(), func([]reflect.Value) []reflect.Value { return nil }))
		}
	case reflect.Pointer:
		if depth < 4 && s.next()%3 != 0 {
			p := reflect.New(v.Type().Elem())
			f.fill(p.Elem(), s, depth+1)
			v.Set(p)
		}
	case reflect.Slice:
		if depth < 4 && s.next()%4 != 0 {
			n := int(s.next() % 4)
			sl := reflect.MakeSlice(v.Type(), n, n)
			for i := range n {
				f.fill(sl.Index(i), s, depth+1)
			}
			v.Set(sl)
		}
	case reflect.Array:
		for i := range v.Len() {
			f.fill(v.Index(i), s, depth+1)
		}
	case reflect.Map:
		if depth < 4 && s.next()%4 != 0 {
			m := reflect.MakeMap(v.Type())
			for range s.next() % 5 {
				k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
				f.fill(k, s, depth+1)
				f.fill(e, s, depth+1)
				m.SetMapIndex(k, e)
			}
			v.Set(m)
		}
	case reflect.Interface:
		f.fillInterface(v, s, depth)
	case reflect.Struct:
		for i := range v.NumField() {
			fv := v.Field(i)
			// Unexported embedded structs carry exported fields, which an
			// address reached through unsafe can set.
			f.fill(reflect.NewAt(fv.Type(), unsafe.Pointer(fv.UnsafeAddr())).Elem(), s, depth+1)
		}
	}
}

func (f Filler) fillInterface(v reflect.Value, s *source, depth int) {
	choices, ok := f.Interfaces[v.Type()]
	if !ok && v.Type() == emptyInterface {
		choices = jsons
	}
	if len(choices) == 0 || depth >= 4 {
		return
	}
	c := choices[int(s.next())%len(choices)]
	if c == nil {
		return
	}
	e := reflect.New(reflect.TypeOf(c)).Elem()
	f.fill(e, s, depth+1)
	v.Set(e)
}
