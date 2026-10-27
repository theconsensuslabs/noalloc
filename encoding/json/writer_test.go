package json

import (
	"math"
	"strconv"
	"testing"
	"time"
)

// TestWriterTimeMatchesMarshalJSONAtEveryOffset writes a time at each
// offset of whole seconds from a day west to a day east, where
// MarshalJSON's refusals begin, and compares it with MarshalJSON.
func TestWriterTimeMatchesMarshalJSONAtEveryOffset(t *testing.T) {
	var w Writer
	mem := make([]byte, 64)
	at := time.Date(2026, 10, 27, 12, 34, 56, 789000000, time.UTC)
	for offset := -24 * 3600; offset <= 24*3600; offset++ {
		tm := at.In(time.FixedZone("", offset))
		want, err := tm.MarshalJSON()
		w.Reset(mem, nil)
		w.Time(tm, "")
		if gotErr := w.Err(); (err == nil) != (gotErr == nil) || err == nil && string(w.Bytes()) != string(want) {
			t.Fatalf("offset %ds: Time wrote %s (%v), MarshalJSON %s (%v)", offset, w.Bytes(), gotErr, want, err)
		}
	}
}

// TestWriterIntsMatchStrconv writes the edges of each int size, past where
// an int of 32 bits ends, and compares them with strconv.
func TestWriterIntsMatchStrconv(t *testing.T) {
	ints := []int64{0, 1, -1, math.MaxInt8, math.MinInt8, math.MaxInt16, math.MinInt16, math.MaxInt32, math.MinInt32,
		math.MaxInt32 + 1, math.MinInt32 - 1, math.MaxInt64, math.MinInt64, math.MinInt64 + 1}
	uints := []uint64{0, 1, math.MaxUint8, math.MaxUint16, math.MaxUint32, math.MaxUint32 + 1, math.MaxUint64}
	var w Writer
	mem := make([]byte, 32)
	for _, v := range ints {
		w.Reset(mem, nil)
		w.Int(v)
		if got := string(w.Bytes()); got != strconv.FormatInt(v, 10) {
			t.Errorf("Int(%d) wrote %s", v, got)
		}
	}
	for _, v := range uints {
		w.Reset(mem, nil)
		w.Uint(v)
		if got := string(w.Bytes()); got != strconv.FormatUint(v, 10) {
			t.Errorf("Uint(%d) wrote %s", v, got)
		}
	}
}
