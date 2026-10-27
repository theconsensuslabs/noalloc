package noalloc

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"hash/maphash"
	"io"
	"log"
	"math"
	"math/bits"
	"net/http"
	"net/netip"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"regexp/syntax"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/theconsensuslabs/noalloc/alloctest"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// Sinks the exercisers write to, so the compiler keeps the calls.
var (
	sinkInt    int
	sinkBool   bool
	sinkString string
	sinkBytes  []byte
	sinkAny    any
)

type intSorter []int

func (s *intSorter) Len() int           { return len(*s) }
func (s *intSorter) Less(i, j int) bool { return (*s)[i] < (*s)[j] }
func (s *intSorter) Swap(i, j int)      { (*s)[i], (*s)[j] = (*s)[j], (*s)[i] }

type ctxKey struct{}

// allowExercisers runs each allowlist entry the way the code it vouches for
// calls it, with its arguments made beforehand, so a run allocates only if
// the entry does.
func allowExercisers(t *testing.T) map[string]func() {
	text, data := "the quick brown fox jumps", []byte("the quick brown fox jumps")
	ints := []int{5, 3, 9, 1, 7}
	sorter := intSorter(slices.Clone(ints))
	room := make([]byte, 0, 256)
	var i32 atomic.Int32
	var i64 atomic.Int64
	var u32 atomic.Uint32
	var u64 atomic.Uint64
	var flag atomic.Bool
	var ptr atomic.Pointer[int]
	var mu sync.Mutex
	var rw sync.RWMutex
	var once sync.Once
	var wg sync.WaitGroup
	var smap sync.Map
	smap.Store("k", 1)
	m := map[string]int{"a": 1, "b": 2}
	mv := reflect.ValueOf(m)
	var iter reflect.MapIter
	key, val := reflect.New(reflect.TypeFor[string]()).Elem(), reflect.New(reflect.TypeFor[int]()).Elem()
	n := 42
	nv := reflect.NewAt(reflect.TypeFor[int](), unsafe.Pointer(&n)).Elem()
	sv := reflect.ValueOf(&ints).Elem()
	var nilPtr *int
	pv := reflect.ValueOf(&nilPtr).Elem()
	uv := reflect.ValueOf(&u64).Elem().Field(0)
	_ = uv
	var uns uint = 7
	unsv := reflect.NewAt(reflect.TypeFor[uint](), unsafe.Pointer(&uns)).Elem()
	var sb strings.Builder
	sb.Grow(64)
	sb.WriteString(text)
	var bb bytes.Buffer
	bb.Grow(64)
	bb.WriteString(text)
	re := regexp.MustCompile(`qu(i)ck`)
	parsed, err := syntax.Parse(`q`, syntax.Perl)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := syntax.Compile(parsed)
	if err != nil {
		t.Fatal(err)
	}
	inst := &prog.Inst[slices.IndexFunc(prog.Inst, func(in syntax.Inst) bool { return in.Op == syntax.InstRune1 })]
	crcTable := crc32.MakeTable(crc32.Castagnoli)
	sum := sha256.Sum256(data)
	seed := maphash.MakeSeed()
	enc := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
	var varint [binary.MaxVarintLen64]byte
	now := time.Now()
	timer := time.NewTimer(time.Hour)
	t.Cleanup(func() { timer.Stop() })
	bg := context.Background()
	cancelCtx, cancel := context.WithCancel(bg)
	t.Cleanup(cancel)
	timerCtx, cancelTimer := context.WithTimeout(bg, time.Hour)
	t.Cleanup(cancelTimer)
	valueCtx := context.WithValue(bg, ctxKey{}, 1)
	var asCtx context.Context = cancelCtx
	req, err := http.NewRequest("GET", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	errSentinel := errors.New("sentinel")
	wrapped := errors.Join(errors.New("other"), errSentinel)
	reader := bytes.NewReader(data)
	sreader := strings.NewReader(text)
	buffered := bufio.NewReaderSize(reader, 64)
	buf := make([]byte, 8)
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	loc := time.FixedZone("x", 3600)
	padded := "  " + text + " \t"
	addr := netip.MustParseAddr("2001:db8::1")
	logger := log.New(io.Discard, "", log.LstdFlags|log.Lmicroseconds)
	detached := context.WithoutCancel(valueCtx)

	return map[string]func(){
		"math.*":                  func() { sinkBool = math.IsNaN(math.Sqrt(float64(n))) },
		"math/bits.*":             func() { sinkInt = bits.OnesCount64(uint64(n)) + bits.Len(uint(n)) },
		"unicode.*":               func() { sinkBool = unicode.IsLetter(rune(n)) && unicode.SimpleFold('k') != 0 },
		"unicode/utf8.*":          func() { r, s := utf8.DecodeRuneInString(text); sinkInt = int(r) + s + utf8.RuneCountInString(text) },
		"sync/atomic.*":           func() { sinkInt = int(atomic.AddInt64(new(int64), 1)) },
		"(*sync/atomic.Int32).*":  func() { i32.Add(1); sinkInt = int(i32.Load()) },
		"(*sync/atomic.Int64).*":  func() { i64.Add(1); sinkInt = int(i64.Load()) },
		"(*sync/atomic.Uint32).*": func() { u32.Add(1); sinkInt = int(u32.Load()) },
		"(*sync/atomic.Uint64).*": func() { u64.Add(1); sinkInt = int(u64.Load()) },
		"(*sync/atomic.Bool).*":   func() { flag.Store(true); sinkBool = flag.Load() },
		"(*sync/atomic.Pointer[T]).*": func() {
			ptr.Store(&n)
			sinkInt = *ptr.Load()
		},
		"cmp.Compare":       func() { sinkInt = cmp.Compare(n, 7) },
		"cmp.Or":            func() { sinkString = cmp.Or("", text) },
		"cmp.Less":          func() { sinkBool = cmp.Less(n, 7) },
		"runtime.KeepAlive": func() { runtime.KeepAlive(&n) },
		"(*sync.Mutex).*":   func() { mu.Lock(); sinkInt = n; mu.Unlock() },
		"(*sync.RWMutex).*": func() {
			rw.RLock()
			sinkInt = n
			rw.RUnlock()
			rw.Lock()
			sinkInt = n
			rw.Unlock()
		},
		"(*sync.Once).Do":                   func() { once.Do(func() {}) },
		"(*sync.WaitGroup).*":               func() { wg.Add(1); wg.Done(); wg.Wait() },
		"reflect.TypeOf":                    func() { sinkBool = reflect.TypeOf(&n) != nil },
		"reflect.TypeFor":                   func() { sinkBool = reflect.TypeFor[int]() != nil },
		"reflect.NewAt":                     func() { sinkInt = int(reflect.NewAt(reflect.TypeFor[int](), unsafe.Pointer(&n)).Elem().Int()) },
		"(reflect.Value).Addr":              func() { sinkBool = nv.Addr().IsNil() },
		"(reflect.Value).Elem":              func() { sinkInt = int(pv.Kind()) + int(reflect.ValueOf(&n).Elem().Int()) },
		"(reflect.Value).Index":             func() { sinkInt = int(sv.Index(2).Int()) },
		"(reflect.Value).Int":               func() { sinkInt = int(nv.Int()) },
		"(reflect.Value).Kind":              func() { sinkInt = int(nv.Kind()) },
		"(reflect.Value).IsNil":             func() { sinkBool = pv.IsNil() },
		"(reflect.Value).IsZero":            func() { sinkBool = nv.IsZero() },
		"(reflect.Value).Len":               func() { sinkInt = sv.Len() + mv.Len() },
		"(reflect.Value).SetIterKey":        func() { iter.Reset(mv); iter.Next(); key.SetIterKey(&iter) },
		"(reflect.Value).SetIterValue":      func() { iter.Reset(mv); iter.Next(); val.SetIterValue(&iter) },
		"(reflect.Value).SetZero":           func() { val.SetZero() },
		"(reflect.Value).Uint":              func() { sinkInt = int(unsv.Uint()) },
		"(reflect.Value).UnsafePointer":     func() { sinkBool = sv.Index(0).Addr().UnsafePointer() != nil },
		"(*reflect.MapIter).Next":           func() { iter.Reset(mv); sinkBool = iter.Next() },
		"(*reflect.MapIter).Reset":          func() { iter.Reset(mv); iter.Reset(reflect.Value{}) },
		"(time.Duration).Seconds":           func() { sinkBool = time.Since(now).Seconds() > 0 },
		"(net/netip.Addr).IsValid":          func() { sinkBool = addr.IsValid() },
		"log.Default":                       func() { sinkBool = log.Default() != nil },
		"(*log.Logger).Output":              func() { _ = logger.Output(2, text) },
		"append: strconv.AppendQuote":       func() { sinkBytes = strconv.AppendQuote(room[:0], padded) },
		"append: (net/netip.Addr).AppendTo": func() { sinkBytes = addr.AppendTo(room[:0]) },
		"(time.Time).IsZero":                func() { sinkBool = now.IsZero() },
		"(*sync.Map).Load":                  func() { _, sinkBool = smap.Load("k") },
		"bytes.Compare":                     func() { sinkInt = bytes.Compare(data, data[1:]) },
		"bytes.Contains":                    func() { sinkBool = bytes.Contains(data, data[4:9]) },
		"bytes.ContainsAny":                 func() { sinkBool = bytes.ContainsAny(data, "xyz") },
		"bytes.ContainsRune":                func() { sinkBool = bytes.ContainsRune(data, 'q') },
		"bytes.Count":                       func() { sinkInt = bytes.Count(data, data[:1]) },
		"bytes.Cut":                         func() { _, _, sinkBool = bytes.Cut(data, data[3:4]) },
		"bytes.CutPrefix":                   func() { _, sinkBool = bytes.CutPrefix(data, data[:3]) },
		"bytes.CutSuffix":                   func() { _, sinkBool = bytes.CutSuffix(data, data[len(data)-3:]) },
		"bytes.Equal":                       func() { sinkBool = bytes.Equal(data, data) },
		"bytes.EqualFold":                   func() { sinkBool = bytes.EqualFold(data, data) },
		"bytes.HasPrefix":                   func() { sinkBool = bytes.HasPrefix(data, data[:3]) },
		"bytes.HasSuffix":                   func() { sinkBool = bytes.HasSuffix(data, data[3:]) },
		"bytes.Index":                       func() { sinkInt = bytes.Index(data, data[10:15]) },
		"bytes.IndexAny":                    func() { sinkInt = bytes.IndexAny(data, "xq") },
		"bytes.IndexByte":                   func() { sinkInt = bytes.IndexByte(data, 'q') },
		"bytes.IndexRune":                   func() { sinkInt = bytes.IndexRune(data, 'q') },
		"bytes.LastIndex":                   func() { sinkInt = bytes.LastIndex(data, data[:1]) },
		"bytes.LastIndexByte":               func() { sinkInt = bytes.LastIndexByte(data, 'o') },
		"bytes.TrimLeft":                    func() { sinkBytes = bytes.TrimLeft(data, "the ") },
		"bytes.TrimPrefix":                  func() { sinkBytes = bytes.TrimPrefix(data, data[:4]) },
		"bytes.TrimRight":                   func() { sinkBytes = bytes.TrimRight(data, "ps") },
		"bytes.TrimSpace":                   func() { sinkBytes = bytes.TrimSpace(data) },
		"bytes.TrimSuffix":                  func() { sinkBytes = bytes.TrimSuffix(data, data[20:]) },
		"(*bytes.Reader).*":                 func() { reader.Reset(data); _, _ = reader.Read(buf); sinkInt = reader.Len() },
		"(*strings.Builder).Cap":            func() { sinkInt = sb.Cap() },
		"(*strings.Builder).Len":            func() { sinkInt = sb.Len() },
		"(*strings.Builder).String":         func() { sinkString = sb.String() },
		"strings.Compare":                   func() { sinkInt = strings.Compare(text, text[1:]) },
		"strings.Contains":                  func() { sinkBool = strings.Contains(text, "brown") },
		"strings.ContainsAny":               func() { sinkBool = strings.ContainsAny(text, "xyz") },
		"strings.ContainsRune":              func() { sinkBool = strings.ContainsRune(text, 'q') },
		"strings.Count":                     func() { sinkInt = strings.Count(text, "o") },
		"strings.Cut":                       func() { _, _, sinkBool = strings.Cut(text, " ") },
		"strings.CutPrefix":                 func() { _, sinkBool = strings.CutPrefix(text, "the") },
		"strings.CutSuffix":                 func() { _, sinkBool = strings.CutSuffix(text, "jumps") },
		"strings.EqualFold":                 func() { sinkBool = strings.EqualFold(text, "THE quick brown fox jumps") },
		"strings.HasPrefix":                 func() { sinkBool = strings.HasPrefix(text, "the") },
		"strings.HasSuffix":                 func() { sinkBool = strings.HasSuffix(text, "jumps") },
		"strings.Index":                     func() { sinkInt = strings.Index(text, "fox") },
		"strings.IndexAny":                  func() { sinkInt = strings.IndexAny(text, "xq") },
		"strings.IndexByte":                 func() { sinkInt = strings.IndexByte(text, 'q') },
		"strings.IndexRune":                 func() { sinkInt = strings.IndexRune(text, 'q') },
		"strings.LastIndex":                 func() { sinkInt = strings.LastIndex(text, "o") },
		"strings.LastIndexByte":             func() { sinkInt = strings.LastIndexByte(text, 'o') },
		"strings.TrimLeft":                  func() { sinkString = strings.TrimLeft(text, "the ") },
		"strings.TrimPrefix":                func() { sinkString = strings.TrimPrefix(text, "the ") },
		"strings.TrimRight":                 func() { sinkString = strings.TrimRight(text, "ps") },
		"strings.TrimSpace":                 func() { sinkString = strings.TrimSpace(padded) },
		"strings.TrimSuffix":                func() { sinkString = strings.TrimSuffix(text, "jumps") },
		"net/textproto.TrimString":          func() { sinkString = textproto.TrimString(padded) },
		"(*strings.Reader).*":               func() { sreader.Reset(text); _, _ = sreader.Read(buf); sinkInt = sreader.Len() },
		"(*bytes.Buffer).Cap":               func() { sinkInt = bb.Cap() },
		"(*bytes.Buffer).Len":               func() { sinkInt = bb.Len() },
		"(*bytes.Buffer).Bytes":             func() { sinkBytes = bb.Bytes() },
		"(*bytes.Buffer).Reset":             func() { bb.Reset() },
		"(*regexp.Regexp).String":           func() { sinkString = re.String() },
		"(*regexp.Regexp).NumSubexp":        func() { sinkInt = re.NumSubexp() },
		"sort.Search":                       func() { sinkInt = sort.Search(len(ints), func(i int) bool { return ints[i] > 4 }) },
		"sort.SearchInts":                   func() { sinkInt = sort.SearchInts(ints, 4) },
		"sort.Sort":                         func() { sort.Sort(&sorter) },
		"sort.Stable":                       func() { sort.Stable(&sorter) },
		"sort.Ints":                         func() { sort.Ints(ints) },
		"slices.BinarySearch":               func() { sinkInt, _ = slices.BinarySearch(ints, 4) },
		"slices.BinarySearchFunc":           func() { sinkInt, _ = slices.BinarySearchFunc(ints, 4, cmp.Compare[int]) },
		"slices.Compare":                    func() { sinkInt = slices.Compare(ints, ints) },
		"slices.Contains":                   func() { sinkBool = slices.Contains(ints, 9) },
		"slices.ContainsFunc":               func() { sinkBool = slices.ContainsFunc(ints, func(v int) bool { return v > 8 }) },
		"slices.Equal":                      func() { sinkBool = slices.Equal(ints, ints) },
		"slices.Index":                      func() { sinkInt = slices.Index(ints, 9) },
		"slices.IndexFunc":                  func() { sinkInt = slices.IndexFunc(ints, func(v int) bool { return v > 8 }) },
		"slices.Max":                        func() { sinkInt = slices.Max(ints) },
		"slices.Min":                        func() { sinkInt = slices.Min(ints) },
		"slices.Reverse":                    func() { slices.Reverse(ints) },
		"slices.Compact":                    func() { sinkInt = len(slices.Compact(ints[:len(ints):len(ints)])) },
		"slices.Delete":                     func() { sinkInt = len(slices.Delete(slices.Clip(ints[:3]), 0, 0)) },
		"slices.Sort":                       func() { slices.Sort(ints) },
		"slices.SortFunc":                   func() { slices.SortFunc(ints, cmp.Compare[int]) },
		"slices.SortStableFunc":             func() { slices.SortStableFunc(ints, cmp.Compare[int]) },
		"slices.All": func() {
			for i := range slices.All(ints) {
				sinkInt += i
			}
		},
		"slices.Backward": func() {
			for i := range slices.Backward(ints) {
				sinkInt += i
			}
		},
		"slices.Values": func() {
			for v := range slices.Values(ints) {
				sinkInt += v
			}
		},
		"append: strconv.AppendInt":   func() { sinkBytes = strconv.AppendInt(room[:0], -1234567, 10) },
		"append: strconv.AppendUint":  func() { sinkBytes = strconv.AppendUint(room[:0], 1234567, 10) },
		"append: strconv.AppendBool":  func() { sinkBytes = strconv.AppendBool(room[:0], true) },
		"append: strconv.AppendFloat": func() { sinkBytes = strconv.AppendFloat(room[:0], math.Pi, 'g', -1, 64) },
		"append: (*encoding/base64.Encoding).AppendEncode": func() {
			sinkBytes = base64.StdEncoding.AppendEncode(room[:0], data)
		},
		"append: slices.Grow":                   func() { sinkBytes = slices.Grow(room[:0], 100) },
		"append: (time.Time).AppendFormat":      func() { sinkBytes = now.AppendFormat(room[:0], time.RFC3339Nano) },
		"append: unicode/utf8.AppendRune":       func() { sinkBytes = utf8.AppendRune(room[:0], 'é') },
		"append: encoding/binary.AppendUvarint": func() { sinkBytes = binary.AppendUvarint(room[:0], 1<<40) },
		"append: encoding/binary.AppendVarint":  func() { sinkBytes = binary.AppendVarint(room[:0], -1<<40) },
		"(encoding/binary.littleEndian).*": func() {
			binary.LittleEndian.PutUint32(varint[:], 7)
			sinkInt = int(binary.LittleEndian.Uint32(varint[:]))
		},
		"(encoding/binary.bigEndian).*":          func() { binary.BigEndian.PutUint32(varint[:], 7); sinkInt = int(binary.BigEndian.Uint32(varint[:])) },
		"encoding/binary.Uvarint":                func() { v, _ := binary.Uvarint(varint[:]); sinkInt = int(v) },
		"encoding/binary.Varint":                 func() { v, _ := binary.Varint(varint[:]); sinkInt = int(v) },
		"encoding/binary.PutUvarint":             func() { sinkInt = binary.PutUvarint(varint[:], 1<<40) },
		"encoding/binary.PutVarint":              func() { sinkInt = binary.PutVarint(varint[:], -1<<40) },
		"hash/crc32.Checksum":                    func() { sinkInt = int(crc32.Checksum(data, crcTable)) },
		"crypto/sha256.Sum256":                   func() { s := sha256.Sum256(data); sinkInt = int(s[0]) },
		"crypto/hmac.Equal":                      func() { sinkBool = hmac.Equal(sum[:], sum[:]) },
		"crypto/rand.Read":                       func() { _, _ = rand.Read(buf) },
		"hash/crc32.Update":                      func() { sinkInt = int(crc32.Update(1, crcTable, data)) },
		"hash/maphash.Comparable":                func() { sinkInt = int(maphash.Comparable(seed, text)) },
		"hash/maphash.MakeSeed":                  func() { s := maphash.MakeSeed(); sinkInt = int(maphash.String(s, text)) },
		"(*encoding/base64.Encoding).Encode":     func() { base64.StdEncoding.Encode(enc, data) },
		"(*encoding/base64.Encoding).EncodedLen": func() { sinkInt = base64.StdEncoding.EncodedLen(len(data)) },
		"regexp/syntax.EmptyOpContext":           func() { sinkInt = int(syntax.EmptyOpContext('a', ' ')) },
		"(*regexp/syntax.Inst).MatchRune":        func() { sinkBool = inst.MatchRune('q') },
		"io.ReadFull":                            func() { reader.Reset(data); _, _ = io.ReadFull(reader, buf) },
		"io.ReadAtLeast":                         func() { reader.Reset(data); _, _ = io.ReadAtLeast(reader, buf, 4) },
		"(*os.File).ReadAt":                      func() { _, _ = f.ReadAt(buf, 2) },
		"(*bufio.Reader).Reset":                  func() { reader.Reset(data); buffered.Reset(reader) },
		"(*bufio.Reader).Read":                   func() { reader.Reset(data); buffered.Reset(reader); _, _ = buffered.Read(buf) },
		"(*bufio.Reader).ReadByte":               func() { reader.Reset(data); buffered.Reset(reader); _, _ = buffered.ReadByte() },
		"(*bufio.Reader).Buffered":               func() { sinkInt = buffered.Buffered() },
		"(*bufio.Reader).Discard":                func() { reader.Reset(data); buffered.Reset(reader); _, _ = buffered.Discard(3) },
		"(*bufio.Reader).Peek":                   func() { reader.Reset(data); buffered.Reset(reader); sinkBytes, _ = buffered.Peek(4) },
		"context.Context.Done":                   func() { sinkBool = asCtx.Done() != nil },
		"context.Context.Err":                    func() { sinkBool = asCtx.Err() != nil },
		"(*context.cancelCtx).Done":              func() { sinkBool = cancelCtx.Done() != nil },
		"(*context.cancelCtx).Err":               func() { sinkBool = cancelCtx.Err() != nil },
		"(*context.cancelCtx).Value":             func() { sinkAny = cancelCtx.Value(ctxKey{}) },
		"(*context.timerCtx).Deadline":           func() { _, sinkBool = timerCtx.Deadline() },
		"(*context.valueCtx).Value":              func() { sinkAny = valueCtx.Value(ctxKey{}) },
		"(context.backgroundCtx).*":              func() { sinkBool = bg.Done() == nil && bg.Err() == nil },
		"(context.todoCtx).*":                    func() { sinkBool = context.TODO().Err() == nil },
		"(context.emptyCtx).*":                   func() { _, sinkBool = bg.Deadline() },
		"(context.withoutCancelCtx).Deadline":    func() { _, sinkBool = detached.Deadline() },
		"(context.withoutCancelCtx).Done":        func() { sinkBool = detached.Done() == nil },
		"(context.withoutCancelCtx).Err":         func() { sinkBool = detached.Err() == nil },
		"(*net/http.Request).Context":            func() { sinkBool = req.Context() != nil },
		"errors.Is":                              func() { sinkBool = errors.Is(wrapped, errSentinel) },
		"time.Now":                               func() { sinkBool = time.Now().IsZero() },
		"time.Since":                             func() { sinkInt = int(time.Since(now)) },
		"time.Unix":                              func() { sinkBool = time.Unix(1700000000, 0).IsZero() },
		"(time.Time).In":                         func() { sinkBool = now.In(loc).IsZero() },
		"(time.Time).UTC":                        func() { sinkBool = now.UTC().IsZero() },
		"(time.Time).Sub":                        func() { sinkInt = int(now.Sub(now)) },
		"(time.Time).UnixNano":                   func() { sinkInt = int(now.UnixNano()) },
		"(time.Time).Unix":                       func() { sinkInt = int(now.Unix()) },
		"(time.Time).Before":                     func() { sinkBool = now.Before(now) },
		"(time.Time).After":                      func() { sinkBool = now.After(now) },
		"(time.Time).Equal":                      func() { sinkBool = now.Equal(now) },
		"(time.Time).Date":                       func() { sinkInt, _, _ = now.Date() },
		"(time.Time).Clock":                      func() { sinkInt, _, _ = now.Clock() },
		"(time.Time).Nanosecond":                 func() { sinkInt = now.Nanosecond() },
		"(time.Time).Zone":                       func() { _, sinkInt = now.Zone() },
		"(*time.Timer).Reset":                    func() { sinkBool = timer.Reset(time.Hour) },
		"(*time.Timer).Stop":                     func() { sinkBool = timer.Stop() },
	}
}

// Every allowlist entry is exercised, and none allocates: a release that
// makes one allocate, or an entry added without a case here, fails.
func TestAllowlistedFunctionsAllocateNothing(t *testing.T) {
	cases := allowExercisers(t)
	var entries []string
	for _, l := range strings.Split(Allowlist, "\n") {
		if i := strings.Index(l, "#"); i >= 0 {
			l = l[:i]
		}
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			entries = append(entries, l)
		}
	}
	for _, e := range entries {
		run, ok := cases[e]
		if !ok {
			t.Errorf("%s is allowlisted with nothing that exercises it; add a case to allowExercisers", e)
			continue
		}
		alloctest.Zero(t, e+", which is allowlisted,", run)
	}
	for name := range cases {
		if !slices.Contains(entries, name) {
			t.Errorf("allowExercisers has a case for %s, which allow.txt does not list", name)
		}
	}
}

// boundedModule is this repository's bounded package, which a caller checks
// from source by listing it in SourceModules.
const boundedModule = "github.com/theconsensuslabs/noalloc/bounded"

// TestEscaperModulesAllocateNothing checks html/template and net/url from
// source: each append function they mark allocates only to grow its
// destination, and neither holds an exemption.
func TestEscaperModulesAllocateNothing(t *testing.T) {
	for _, mod := range []struct{ dir, path string }{
		{"html/template", "github.com/theconsensuslabs/noalloc/html/template"},
		{"net/url", "github.com/theconsensuslabs/noalloc/net/url"},
	} {
		dir, err := filepath.Abs(mod.dir)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: mod.path, Root: dir,
			Allow: strings.Split(Allowlist, "\n")})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Roots) == 0 {
			t.Errorf("%s: no root was checked", mod.dir)
		}
		for _, f := range res.Findings {
			t.Errorf("%s allocates: %s", mod.dir, f)
		}
		for _, e := range res.Exemptions {
			t.Errorf("%s holds an exemption: %s", mod.dir, e.Key())
		}
	}
}

// TestBoundedAllocatesNothing checks bounded from source, every root it
// marks: none allocates outside the setup and responses it exempts.
func TestBoundedAllocatesNothing(t *testing.T) {
	dir, err := filepath.Abs("bounded")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(Config{Dir: dir, Patterns: []string{"."}, Module: boundedModule, Root: dir,
		Allow: strings.Split(Allowlist, "\n")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Roots) == 0 {
		t.Fatal("no root of bounded was checked")
	}
	for _, f := range res.Findings {
		t.Errorf("bounded allocates: %s", f)
	}
}
