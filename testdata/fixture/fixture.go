// Package fixture holds one case of each thing the checker flags or allows.
// A line the checker must flag ends in a want comment naming each reason.
package fixture

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"unicode/utf8"
)

type Scratch struct {
	buf  []byte
	memo map[int]int
	in   inner
	t    *T
	rows []*row
}

type inner struct{ list []int }

type row struct{ text []byte }

type T struct{ n int }

var (
	sink    any
	global  *T
	keep    func() int
	cached  *T
	errBad  = errors.New("bad")
	once    sync.Once
	byName  map[string]int
	lengths []int
	names   []string
	ptr     *int
)

//noalloc:root
func Root(sc *Scratch, n int, b []byte, s string) (int, error) {
	if n < 0 {
		return 0, fmt.Errorf("negative %d", n)
	}
	if n == 1 {
		return 0, errBad
	}
	if n == 2 {
		return 0, failSlowly(n)
	}
	if n > 1000 {
		panic(fmt.Sprint("too big: ", n))
	}
	if err := check(n); err != nil {
		return 0, fmt.Errorf("checking: %w", err)
	}
	if _, err := withDefer(n); err != nil {
		return 0, err
	}
	if sticky(&rec{}, n) == 0 {
		return 0, nil
	}
	_ = stickyEither(&rec{}, n, n > 5)
	_ = stickyEachTurn(&rec{}, lengths)
	_ = stickyLoop(&rec{}, lengths)
	_ = sumSwallowing(names)
	if _, err := sumPropagating(names); err != nil {
		return 0, err
	}
	_ = countRecording(&rec{}, lengths, n)
	_ = sumRecovering(lengths)
	_ = sumPanicking(lengths)
	_ = recordEach(&rec{}, lengths)
	boxConstants(n)
	if _, err := sumThrough(lengthParser{}, names); err != nil {
		return 0, err
	}
	if _, err := sumChunks(names); err != nil {
		return 0, err
	}
	_ = sort.Search(len(lengths), func(i int) bool { return CompareLengths(lengths[i], n) >= 0 })
	if _, err := firstOver(lengths); err != nil {
		return 0, err
	}
	_, _ = returnsSome(names)
	if _, err := sumSettled(names); err != nil {
		return 0, err
	}
	if _, err := sumUntilPaused(names); err != nil {
		return 0, err
	}
	if _, err := sumTidied(names); err != nil {
		return 0, err
	}
	_ = errorAlways(s)
	_ = errorEachTurn(s, n)
	escapes(n)
	into(sc, n)
	runtimeAllocs(sc, n, b, s)
	calls(n, b)
	allowed(sc, n, b)
	exempt(n)
	dispatch(impl{}, n)
	_ = first(lengths)
	funcValues(n)
	formats(sc, n, b)
	fixedFill(&Fixed{}, &loose{}, n)
	adopt(sc, b)
	guarded(sc, n, s)
	lengths = guardedOutside(lengths, n)
	_ = genAt(&gen[int]{}, n)
	return n, nil
}

// guarded appends to the Scratch where a comparison shows there is room, and
// where it does not show it for the append that follows.
func guarded(sc *Scratch, n int, s string) {
	if len(sc.buf)+1 <= cap(sc.buf) {
		sc.buf = append(sc.buf, 'a')
	}
	if len(sc.buf)+2 > cap(sc.buf) {
		return
	}
	sc.buf = append(sc.buf, 'b', 'c')
	sc.buf = append(sc.buf, 'd') // want: append that may grow reused memory
	if len(s) <= cap(sc.buf)-len(sc.buf) {
		sc.buf = append(sc.buf, s...)
	}
	if len(sc.buf) < cap(sc.buf) {
		sc.buf = append(sc.buf, 'e')
	}
	if len(sc.buf)+1 <= cap(sc.buf) {
		sc.buf = sc.buf[:0]
		sc.buf = append(sc.buf, 'f') // want: append that may grow reused memory
	}
	if n+1 <= cap(sc.buf)-len(sc.buf) {
		sc.buf = append(sc.buf, 'g') // want: append that may grow reused memory
	}
	if len(sc.in.list)+1 <= cap(sc.buf) {
		sc.in.list = append(sc.in.list, n) // want: append that may grow reused memory
	}
	if len(sc.buf)+1 <= cap(sc.buf) {
		sc.buf = append(sc.buf[len(sc.buf):len(sc.buf):cap(sc.buf)], 'h')
	}
}

func escapes(n int) {
	global = &T{n}                 // want: escapes
	sink = n                       // want: escapes
	keep = func() int { return n } // want: escapes
	x := n                         // want: escapes
	ptr = &x
	global = newT()
	sink = double(n)  // want: escapes
	sink = tripled(n) // want: escapes
	boxed := n * 2
	put := func() { sink = boxed }
	put() // want: escapes
	put() // want: escapes
}

// double and tripled are inlined, so the boxing of their results is reported
// where they are called.
func double(n int) int { return n * 2 }

func tripled(n int) (out int) {
	out = n * 3
	return out
}

// guardedOutside appends to a slice the Scratch does not hold, once where a
// comparison shows there is room and once where nothing does.
func guardedOutside(dst []int, n int) []int {
	if len(dst) < cap(dst) {
		dst = append(dst, n)
	}
	return append(dst, n) // want: append to a slice outside the Scratch
}

func newT() *T {
	return new(T) // want: escapes
}

func runtimeAllocs(sc *Scratch, n int, b []byte, s string) {
	buf := make([]byte, n) // want: make with a size
	buf[0] = 1
	var out []int
	out = append(out, n) // want: append to a slice outside the Scratch
	lengths = out
	m := map[int]int{}
	m[n] = 1                        // want: map write
	_ = s + "!" + strconv.Quote("") // want: string concatenation, calls strconv.Quote
	_ = string(b)                   // want: string([]byte)
	_ = []byte(s)                   // want: []byte(string)
	go func() {}()                  // want: go statement, escapes
	for i := range n {
		defer done(i) // want: defer in a loop
	}
	defer done(0)
	_ = m
	for v := range each {
		lengths = append(lengths, v) // want: append to a slice outside the Scratch
	}
}

func done(int) {}

// each is an iterator, so the loop over it runs its body as a function of
// its own.
func each(yield func(int) bool) {
	for i := range 3 {
		if !yield(i) {
			return
		}
	}
}

func calls(n int, b []byte) {
	_ = strconv.Itoa(n)       // want: calls strconv.Itoa
	_ = bytes.IndexByte(b, 1) // on the allowlist
	sort.Sort(byLen(lengths)) // want: escapes
}

type byLen []int

func (s byLen) Len() int           { return len(s) }
func (s byLen) Less(i, j int) bool { return strconv.Itoa(s[i]) < "" } // want: calls strconv.Itoa
func (s byLen) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// allowed holds what reuses the Scratch, what the compiler makes without a
// copy, and setup that runs once.
func allowed(sc *Scratch, n int, b []byte) {
	sc.buf = append(sc.buf, 'x') // want: append that may grow reused memory
	sc.buf = fill(sc.buf[:0], n)
	sc.in.list = append(sc.in.list, n) // want: append that may grow reused memory
	list := sc.in.list[:0]
	add := func(v int) { list = append(list, v) } // want: append that may grow reused memory
	add(n)
	sc.in.list = list
	sc.memo[n] = n // want: map write that may grow reused memory
	_ = byName[string(b)]
	_ = string(b) == "x"
	if sc.t == nil {
		sc.t = &T{n}
	}
	if cached == nil {
		cached = new(T)
	}
	once.Do(setup)
}

func fill(dst []byte, n int) []byte {
	for range n {
		dst = append(dst, 'y') // want: append that may grow reused memory
	}
	return dst
}

func setup() { global = &T{} }

func check(n int) error {
	if n == 3 {
		return &codeErr{n}
	}
	return nil
}

type codeErr struct{ n int }

func (e *codeErr) Error() string { return "code " + strconv.Itoa(e.n) }

// failSlowly only returns errors, so a call to it is an error path.
func failSlowly(n int) error {
	s := strconv.Itoa(n)
	return errors.New(s + " failed")
}

// withDefer returns its error through the deferred-return path.
func withDefer(n int) (int, error) {
	defer done(0)
	if n == 7 {
		return 0, fmt.Errorf("seven %d", n)
	}
	return n, nil
}

// rec keeps the first failure, for a caller to read after.
type rec struct{ err error }

func (r *rec) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// sticky reports its failure to r rather than returning it.
func sticky(r *rec, n int) int {
	if n == 9 {
		r.fail(fmt.Errorf("nine %d", n))
		return 0
	}
	return n
}

// stickyEither reports its failure on either half of a condition.
func stickyEither(r *rec, n int, ok bool) int {
	if !ok || n < 0 {
		r.fail(fmt.Errorf("either %d", n))
		return 0
	}
	return n
}

// stickyEachTurn reports a failure on a turn of its loop and goes on, so
// each turn can make one.
func stickyEachTurn(r *rec, xs []int) int {
	sum := 0
	for _, x := range xs {
		if x < 0 {
			r.fail(fmt.Errorf("negative %d", x)) // want: calls fmt.Errorf, escapes
			continue
		}
		sum += x
	}
	return sum
}

// stickyLoop reports a failure on a turn of its loop and stops.
func stickyLoop(r *rec, xs []int) int {
	for i := range len(xs) {
		if xs[i] < 0 {
			r.fail(fmt.Errorf("negative at %d", i))
			return 0
		}
	}
	return len(xs)
}

// errorAlways makes an error on its normal path, which is no failure's.
func errorAlways(s string) string {
	saved = errors.New("always") // want: calls errors.New
	return s + "?"               // want: string concatenation, escapes
}

// errorEachTurn makes an error on every turn of its loop.
func errorEachTurn(s string, n int) string {
	for range n {
		saved = errors.New("each") // want: calls errors.New
		s = s + "!"                // want: string concatenation, escapes
	}
	return s
}

var saved error

func exempt(n int) {
	_ = strconv.Itoa(n) //noalloc:exempt a trailing exemption
	//noalloc:exempt an exemption standing alone covers the next line
	_ = make([]byte, n)
	allocates(n)                  //noalloc:exempt and stops the walk into what it calls
	answers(len(strconv.Itoa(n))) //noalloc:exempt response: a kind the config checks the calls of
}

func allocates(n int) { global = &T{n} }

func answers(n int) { global = &T{n} } // want: escapes

type writer interface{ write(n int) }

type impl struct{}

func (impl) write(n int) { _ = strconv.Itoa(n) } // want: calls strconv.Itoa

// other implements writer but never reaches dispatch, so VTA leaves it out.
type other struct{}

func (other) write(n int) { _ = strconv.Itoa(n) }

func dispatch(w writer, n int) { w.write(n) }

// first is generic, so the checker reaches its instance, not its source.
func first[T any](xs []T) T {
	_ = strconv.Itoa(len(xs)) // want: calls strconv.Itoa
	return xs[0]
}

// Unreached allocates and is not reached from Root.
func Unreached(n int) []int {
	var w writer = other{}
	w.write(n)
	return make([]int, n)
}

// funcValues calls functions it holds only as values.
func funcValues(n int) {
	f := pick(n)
	f(n) // want: call with no known callee: result of fixture.pick
	_, h := pair(n)
	h(n) // want: call with no known callee: result 1 of fixture.pair
	var k func(int)
	set(&k, n)
	k(n) // want: call with no known callee: *k
	g := func() {
		lengths = append(lengths, n) // want: append to a slice outside the Scratch
	}
	g()
}

var funcs []func(int)

func pick(n int) func(int)              { return funcs[n%len(funcs)] }
func pair(n int) (func(int), func(int)) { return funcs[0], funcs[n%len(funcs)] }
func set(k *func(int), n int)           { *k = funcs[n%len(funcs)] }

// into hands memory to functions that append into it, as a decoder fills the
// buffer it is given: memory from the Scratch is reused, and a fresh
// variable, zero until it grows, grows on every call. A sized array is
// memory of its own size.
func into(sc *Scratch, n int) {
	sc.in = scratchOnly(sc.in, n)
	sc.in = both(sc.in, n)
	var fresh inner
	fresh = both(fresh, n)
	fillAt(&sc.in, n)
	fillAt(&fresh, n)
	if len(sc.rows) > 0 {
		fillRow(sc.rows[0], n)
		fillRowBoth(sc.rows[0], n)
	}
	var own row
	fillRowBoth(&own, n)
	var sized [8]int
	if len(append(sized[:0], n)) == 0 {
		return
	}
}

// formats appends with functions shaped like append: into the Scratch, or
// into what one returned from it, they are append; into a fresh slice they
// are a call.
func formats(sc *Scratch, n int, b []byte) {
	sc.buf = strconv.AppendInt(sc.buf[:0], int64(n), 10)            // want: calls strconv.AppendInt, which may grow reused memory
	sc.buf = strconv.AppendBool(append(sc.buf, ' '), n > 0)         // want: append that may grow reused memory, calls strconv.AppendBool, which may grow reused memory
	sc.buf = append(strconv.AppendUint(sc.buf, uint64(n), 10), '.') // want: calls strconv.AppendUint, which may grow reused memory, append that may grow reused memory
	var fresh []byte
	fresh = strconv.AppendInt(fresh, int64(n), 10)      // want: calls strconv.AppendInt
	fresh = utf8.AppendRune(fresh, 'x')                 // want: calls unicode/utf8.AppendRune
	sc.buf = utf8.AppendRune(sc.buf, 'x')               // want: calls unicode/utf8.AppendRune, which may grow reused memory
	sc.buf = append(AppendDigits(sc.buf, n), '.')       // want: calls fixture.AppendDigits, which may grow reused memory, append that may grow reused memory
	fresh = AppendDigits(fresh, n)                      // want: calls fixture.AppendDigits
	sc.buf = base64.StdEncoding.AppendEncode(sc.buf, b) // want: calls (*encoding/base64.Encoding).AppendEncode, which may grow reused memory
	fresh = base64.StdEncoding.AppendEncode(fresh, b)   // want: calls (*encoding/base64.Encoding).AppendEncode
	sc.buf = AppendTwice(sc.buf, n)                     // want: calls fixture.AppendTwice, which may grow reused memory
	fresh = AppendTwice(fresh, n)                       // want: calls fixture.AppendTwice
	sc.buf = digits{}.AppendTo(sc.buf, n)               // want: calls (fixture.digits).AppendTo, which may grow reused memory
	fresh = digits{}.AppendTo(fresh, n)                 // want: calls (fixture.digits).AppendTo
	if len(append(fresh, '.')) == 0 {                   // want: append to a slice outside the Scratch
		return
	}
}

// AppendDigits is a root shaped like append: it grows dst freely, and each
// call of it is judged as a call of append is.
//
//noalloc:append
func AppendDigits(dst []byte, n int) []byte {
	dst = append(dst, '#')
	return strconv.AppendInt(dst, int64(n), 10)
}

// AppendTwice is a generic root shaped like append: each instance of it is
// judged as the function marked.
//
//noalloc:append
func AppendTwice[T ~int](dst []byte, v T) []byte {
	dst = AppendDigits(dst, int(v))
	return AppendDigits(dst, int(v))
}

type digits struct{}

// AppendTo is a method shaped like append: what it appends to is its first
// argument after the receiver.
//
//noalloc:append
func (digits) AppendTo(dst []byte, n int) []byte {
	return append(dst, byte('0'+n%10))
}

func scratchOnly(in inner, n int) inner {
	in.list = append(in.list[:0], n) // want: append that may grow reused memory
	return in
}

func both(in inner, n int) inner {
	in.list = append(in.list[:0], n) // want: append to a slice outside the Scratch
	return in
}

func fillAt(in *inner, n int) {
	in.list = append(in.list, n) // want: append to a slice outside the Scratch
}

// A struct the Scratch holds by pointer in a slice is the Scratch's memory.
func fillRow(r *row, n int) {
	r.text = append(r.text, byte(n)) // want: append that may grow reused memory
}

func fillRowBoth(r *row, n int) {
	r.text = append(r.text, byte(n)) // want: append to a slice outside the Scratch
}

// Fixed holds memory its methods never grow, so its fields are reused memory
// wherever the pointer to it came from. loose is the same shape unmarked.
//
//noalloc:fixed
type Fixed struct{ mem []byte }

type loose struct{ mem []byte }

// adopt puts the caller's memory into the Scratch, which would then reuse
// memory it does not own.
func adopt(sc *Scratch, b []byte) {
	sc.buf = b // want: stores memory the Scratch does not own
	sc.buf = sc.buf[:0]
}

func fixedFill(f *Fixed, l *loose, n int) {
	f.mem = append(f.mem[:0], byte(n)) // want: append that may grow reused memory
	l.mem = append(l.mem[:0], byte(n)) // want: append to a slice outside the Scratch
}

// gen is a generic list whose method panics with a message past its end.
// The message escapes only on the panic's path.
type gen[T any] struct{ xs []T }

func (g *gen[T]) at(i int) T {
	if i >= len(g.xs) {
		panic("fixture: an index past the end")
	}
	return g.xs[i]
}

// genAt reaches gen's method through a generic function, which inlines it.
func genAt[T any](g *gen[T], i int) T { return g.at(i) }

// GenRoot is a generic root, checked as written rather than as some
// instantiation of it. It reaches gen's method through a wrapper that only
// calls the method's body, and the panic's message is still off the normal
// path.
//
//noalloc:root
func GenRoot[T any](g *gen[T], i int) T {
	g.keep()
	return g.at(i)
}

var genSink any

// keep allocates on its normal path, which a root reaching it through a
// wrapper still finds.
func (g *gen[T]) keep() {
	p := new(T) // want: escapes
	genSink = p
}

// parseName fails on an empty name.
func parseName(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty name") // want: calls fmt.Errorf
	}
	return len(s), nil
}

// sumSwallowing skips each name parseName refuses, so every one can make
// an error.
func sumSwallowing(xs []string) int {
	total := 0
	for _, x := range xs {
		n, err := parseName(x)
		if err != nil {
			continue
		}
		total += n
	}
	return total
}

// parseLength fails on an empty name, as parseName does, for a caller that
// stops at the first failure.
func parseLength(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty length")
	}
	return len(s), nil
}

// sumPropagating returns the first error, which ends the call.
func sumPropagating(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := parseLength(x)
		if err != nil {
			return 0, fmt.Errorf("summing: %w", err)
		}
		total += n
	}
	return total, nil
}

// countRecording records each length over limit and goes on.
func countRecording(r *rec, xs []int, limit int) int {
	count := 0
	for _, x := range xs {
		if x > limit {
			r.fail(fmt.Errorf("%d is over %d", x, limit)) // want: calls fmt.Errorf, escapes
			continue
		}
		count++
	}
	return count
}

// mustPositive panics on a negative length.
func mustPositive(n int) int {
	if n < 0 {
		panic(fmt.Sprintf("negative length %d", n)) // want: calls fmt.Sprintf, escapes
	}
	return n
}

// safeLength recovers mustPositive's panic, so its caller goes on.
func safeLength(n int) (v int) {
	defer func() {
		if recover() != nil {
			v = 0
		}
	}()
	return mustPositive(n)
}

// sumRecovering treats each length that panics as zero, so every one can
// make a panic's message.
func sumRecovering(xs []int) int {
	total := 0
	for _, x := range xs {
		total += safeLength(x)
	}
	return total
}

// mustShort panics on a long length, as mustPositive does, with nothing to
// recover it.
func mustShort(n int) int {
	if n > 1<<20 {
		panic(fmt.Sprintf("length %d is too long", n))
	}
	return n
}

// sumPanicking panics out of the root on the first long length.
func sumPanicking(xs []int) int {
	total := 0
	for _, x := range xs {
		total += mustShort(x)
	}
	return total
}

// recordIfNegative records a negative length and goes on. Called once, its
// failure is made once.
func recordIfNegative(r *rec, n int) {
	if n < 0 {
		r.fail(fmt.Errorf("negative %d", n)) // want: calls fmt.Errorf, escapes
	}
}

// recordEach calls recordIfNegative on every length.
func recordEach(r *rec, xs []int) int {
	recordIfNegative(r, len(xs))
	for _, x := range xs {
		recordIfNegative(r, x)
	}
	return len(xs)
}

// code is a number with a name, boxed as an error is.
type code int

var anySink any

const boxedLimit = 1 << 10

// boxConstants boxes constants, which the compiler keeps as read-only data,
// and a variable, which it moves to the heap.
func boxConstants(n int) {
	anySink = 7
	anySink = code(2)
	anySink = "a constant string"
	anySink = boxedLimit
	anySink = 1.5 * boxedLimit
	anySink = code(boxedLimit) << 2
	anySink = n                                   // want: escapes
	bytesSink = []byte("a constant made a slice") // want: escapes, []byte(string) conversion
}

var bytesSink []byte

// parser is met by lengthParser's value method through a pointer, which
// the compiler wraps.
type parser interface{ parse(s string) (int, error) }

type lengthParser struct{}

func (lengthParser) parse(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty %q", s)
	}
	return len(s), nil
}

// sumThrough returns the first error p gives, which ends the call.
func sumThrough(p lengthParser, xs []string) (int, error) {
	var through parser = &p
	total := 0
	for _, x := range xs {
		n, err := through.parse(x)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// chunk reports whether s is long, failing on an empty one.
func chunk(s string) (bool, error) {
	if s == "" {
		return false, fmt.Errorf("empty chunk")
	}
	return len(s) > 8, nil
}

// sumChunks stops at the first failure or the first long chunk, returning
// the failure itself.
func sumChunks(xs []string) (bool, error) {
	for _, x := range xs {
		if long, err := chunk(x); err != nil || long {
			return long, err
		}
	}
	return false, nil
}

// CompareLengths is a root that a search also calls, once for each length
// it tries. Its panic ends its own call however often the search calls it.
//
//noalloc:root
func CompareLengths(a, b int) int {
	if a < 0 || b < 0 {
		panic(fmt.Sprintf("negative lengths %d and %d", a, b))
	}
	return a - b
}

// pastLimit reports whether n is over the limit, failing on a negative one.
func pastLimit(n int) (bool, error) {
	if n < 0 {
		return false, fmt.Errorf("negative %d", n)
	}
	return n > 9, nil
}

// firstOver stops at the first failure or the first length over the limit,
// telling the two apart with a second test of the error.
func firstOver(xs []int) (int, error) {
	for i, x := range xs {
		if over, err := pastLimit(x); err != nil || over {
			if err != nil {
				return 0, err
			}
			return i, nil
		}
	}
	return -1, nil
}

// parseSome fails on an empty name, for a caller that returns only some of
// its failures.
func parseSome(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty some") // want: calls fmt.Errorf
	}
	return len(s), nil
}

// returnsSome returns a failure only after a long name, and goes on past
// the others.
func returnsSome(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := parseSome(x)
		if total > 100 {
			return 0, err
		}
		total += n
	}
	return total, nil
}

var pending error

// settle is err, or the failure pending before it.
func settle(err error) error {
	if err == nil {
		err = pending
	}
	return err
}

// parseSettled fails on an empty name, for a caller that settles its error.
func parseSettled(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty settled")
	}
	return len(s), nil
}

// sumSettled returns the first error settle carries out of parseSettled.
func sumSettled(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := parseSettled(x)
		if err := settle(err); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// sumTidied runs a loop of its own between a failure and returning it.
func sumTidied(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := parseTidied(x)
		for range n {
			total++
		}
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func parseTidied(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty tidied")
	}
	return len(s), nil
}

var errPaused = errors.New("paused")

// parsePausing pauses on a dash and fails on an empty name.
func parsePausing(s string) (int, error) {
	switch s {
	case "":
		return 0, fmt.Errorf("empty pausing")
	case "-":
		return 0, errPaused
	}
	return len(s), nil
}

// unlessPaused is err, but nil for a pause.
func unlessPaused(err error) error {
	if err == errPaused {
		return nil
	}
	return err
}

// sumUntilPaused returns the first failure but a pause, which it goes on
// past.
func sumUntilPaused(xs []string) (int, error) {
	total := 0
	for _, x := range xs {
		n, err := parsePausing(x)
		if err := unlessPaused(err); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}
