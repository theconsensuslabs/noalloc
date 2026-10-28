# noalloc

*Disclaimer: This repo is entirely written by AI. The README is
largely written by AI. It is guided by a human and used in production
at The Consensus in both the web service and our code search engine. It
is experimental and may change vastly.*

It's one thing to be able to write some code that doesn't allocate on
the heap. It's another thing to be able to keep code from not
allocating as the code changes. With noalloc you can assert Golang
functions should not allocate and the noalloc CLI will check the
entire call tree from that function for heap allocations.

noalloc will tell you every line that allocates, allowing you to
remediate or exempt lines. noalloc also provides zero-allocating,
fixed-sized data structures and algorithms (including a JSON encoder
and decoder) to make it easier and cleaner to write code that does not allocate.

```
go install github.com/theconsensuslabs/noalloc/cmd/noalloc@latest
```

You'll need Go 1.27 or later.

For example, mark a function with `//noalloc:root` and run noalloc in its
module.

```go
//noalloc:root
func (c *Counter) Add(name string, n int) string {
	c.counts[name] += n
	return name + "=" + strconv.Itoa(c.counts[name])
}
```

```
$ noalloc
counter.go:11: (*example.com/counter.Counter).Add: map write
counter.go:12: (*example.com/counter.Counter).Add: calls strconv.Itoa; string concatenation; escapes: name + "=" + ~r0 escapes to heap
```

noalloc exits 1 (it found an allocation) or 2 (it couldn't run at all). It
allows allocations on an error or panic that ends the root's call and in
first-use setup, which the caveats below cover.

## Directives

- `//noalloc:root` checks a function and everything it calls.
- `//noalloc:exempt <reason>` exempts the line it's on and everything that
  line calls. On a line by itself, it applies to the next line.
- `//noalloc:fixed` on a type says its methods never grow its slices, so
  noalloc treats them as reused memory.
- `//noalloc:append` marks a function that works like `append`, so a call to
  it is judged like a call to `append`.

Reused memory is memory kept between requests, such as a buffer in a
long-lived struct. An append to it is only allowed after a comparison proves
there's room. You can also mark a type's fields as reused memory with
`-scratch pkg/path.Type`. A call to a function outside your module is
allowed when that function's own code allocates nothing, which noalloc works
out from its source, or from its compiled code when it's written in assembly
or is part of the runtime. Packages you check from source with `-source`,
which takes an import path prefix, are checked as part of your module
instead. Check noalloc's own libraries that way:
`-source github.com/theconsensuslabs/noalloc/bounded`, and the same for
`encoding/json`, so their `//noalloc:fixed` types and other directives count,
which they don't in code outside your module. Run `noalloc -h` for every flag.

## Designing with bounded

[bounded](bounded) has four containers with a fixed maximum capacity: `Buf`,
`Vec`, `Slice`, and `Map`. When a write exceeds it, the write does nothing and
marks the container full, so you can check once at the end instead of
after every write. A container's memory can be your own, or borrowed from a
`Pool` through a `Region`. These examples are also in
[example_test.go](bounded/example_test.go), where `go test` checks their
output.

### Buf

A `Buf` is a byte buffer. Give it memory with `Reset`, and once a write doesn't
fit it stays full:

```go
var b bounded.Buf
b.Reset(make([]byte, 32), nil)
b.WriteString("id=")
b.WriteInt(42)
fmt.Println(string(b.Bytes()), b.Full()) // id=42 false

b.WriteString(strings.Repeat("x", 40))
fmt.Println(string(b.Bytes()), b.Full()) // id=42 true
```

Give it a writer too and it flushes to the writer instead of filling up:

```go
var b bounded.Buf
b.Reset(make([]byte, 8), os.Stdout)
b.WriteString("hello, ")
b.WriteString("world\n") // flushes "hello, " first
b.Flush()                // prints hello, world
```

### Vec

A `Vec` is a list of any type. With memory you give it, it holds as many items
as that memory's capacity:

```go
var v bounded.Vec[int]
v.Reset(make([]int, 0, 3))
for i := range 5 {
	v.Push(i * i)
}
fmt.Println(v.Items(), v.Full()) // [0 1 4] true
```

With `ResetFrom` it can borrow chunks from a pool as it fills, up to the
region's budget. Its items aren't contiguous, so read it with `At`, or
a chunk at a time with `Chunks`:

```go
pool := bounded.NewPool[int](4, 8)
var r bounded.Region
r.Reset(2 * pool.ChunkBytes())
defer r.Release()

var v bounded.Vec[int]
v.ResetFrom(&r, pool)
for i := range 10 {
	v.Push(i)
}
fmt.Println(v.Len(), v.Full(), v.At(6)) // 8 true 6
for c := range v.Chunks() {
	fmt.Println(c) // [0 1 2 3], then [4 5 6 7]
}
```

### Slice

A `Slice` is contiguous. When it fills a chunk, it can move to a chunk
from the next larger pool, the way `append` moves a slice to a bigger array:

```go
classes := bounded.NewClasses(bounded.NewPool[int](4, 8), bounded.NewPool[int](16, 8))
var r bounded.Region
r.Reset(1 << 10)
defer r.Release()

var s bounded.Slice[int]
s.ResetFrom(&r, classes)
for i := range 10 {
	s.Push(i)
}
fmt.Println(s.Items(), s.Cap()) // [0 1 2 3 4 5 6 7 8 9] 16
```

### Map

A `Map` holds up to the number of entries you reset it for. A new key past
that isn't added, but you can still update the keys it has:

```go
var m bounded.Map[string, int]
m.Reset(2)
m.Set("a", 1)
m.Set("b", 2)
fmt.Println(m.Set("c", 3), m.Full()) // false true

v, ok := m.Get("a")
fmt.Println(v, ok, m.Len()) // 1 true 2
```

With `Grow` it starts small and can move to larger pools of slots as it fills:

```go
maps := bounded.NewMapClasses(bounded.NewMapPool[string, int](4, 8), bounded.NewMapPool[string, int](64, 8))
var r bounded.Region
r.Reset(1 << 20)
defer r.Release()

var m bounded.Map[string, int]
m.Grow(&r, maps)
for i := range 20 {
	m.Set(strconv.Itoa(i), i)
}
fmt.Println(m.Len(), m.Limit()) // 20 64
```

### Pool and Region

A `Pool` lends a fixed number of chunks. When they're all out, `Take` returns
nil until a region gives one back:

```go
pool := bounded.NewPool[byte](1<<10, 2)
var a, b bounded.Region
a.Reset(4 << 10)
b.Reset(4 << 10)

bounded.Take(&a, pool) // a gets the first chunk
bounded.Take(&a, pool) // and the second
bounded.Take(&b, pool) // nil: the pool is empty
a.Release()
bounded.Take(&b, pool) // b gets one of a's
b.Release()

st := pool.Stats()
fmt.Println(st.Lent, st.Empty) // 3 1
```

Use `Wait` instead of `Take` to wait for a chunk until a channel closes, and
`Reserve` to take several at once or none.

### Handling dynamic input

A request's input size isn't known until the input arrives, so give each
request a `Region` with a fixed budget when it starts. Within that budget it
borrows chunks from a shared `Pool` as its input calls for them, and
releasing the region when the request ends gives them all back:

```go
var rows = bounded.NewPool[byte](16<<10, 64)

// nowOnly starts closed, so Reserve takes a free chunk and never waits.
var nowOnly = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func (p *page) serve(w io.Writer) error {
	p.region.Reset(4 * rows.ChunkBytes())
	defer p.region.Release()
	if !bounded.Reserve(nowOnly, &p.region, rows, 1) {
		return errBusy
	}
	p.out.ResetFrom(&p.region, rows, w)
	for p.next() {
		p.writeRow(&p.out)
	}
	p.out.Flush()
	return p.out.Err()
}
```

Here the page streams through one chunk that flushes as it fills, so a long
page needs no more memory than a short one. Output that must be held whole
can call `Buf.Borrow` instead, which moves the buffer to a larger chunk each
time it fills, until the region's budget runs out.

Every site that can exhaust its memory budget needs a plan for when it does. For
example, you might:

- End the page early and let the client resume from a cursor.
- Skip an optimization when the slower path still gives the right answer.
- Forget an entry in a memo.
- Refuse input that's too large to parse before you start parsing it.

Stream output through a `Buf` that flushes to the response, so its memory
doesn't grow with the response. Reserve what a request needs to start when
admission control admits it, and size the pool to hold every request admitted
at once.

## Encoding and decoding JSON

[encoding/json](encoding/json) encodes and decodes JSON without allocating,
the first call included, through methods its `jsongen` command writes for
your types:

```
//go:generate go run github.com/theconsensuslabs/noalloc/cmd/jsongen -write Reply -read Request
```

`WriteJSON` writes through a `Writer`, into memory you give it and keep
across calls, flushing to an `io.Writer` as that fills:

```go
var w json.Writer
w.Reset(mem, out)
reply.WriteJSON(&w)
if w.Flush() != nil {
	return errReply
}
```

`ReadJSON` reads through a `Reader`, from memory or from an `io.Reader`
through a buffer you give it. Strings decode into the Reader's `Arena`, over
your memory, which calls its `More` for another chunk when it fills, and
stay valid until you reuse that memory. A slice decodes into the capacity it
has and a pointer into what it points at; past either, the decode fails
with `Full` rather than allocating:

```go
req := Request{Tags: make([]string, 0, 16)}
var r json.Reader
r.Arena.Reset(stringMemory)
r.ResetBytes(body)
req.ReadJSON(&r)
if r.Finish() != nil {
	return errRequest
}
```

A `Writer` or `Reader` can live in a local, and allocates nothing there,
but the memory you give it can't: `Reset` keeps the slice, which moves an
array behind it to the heap. The error `Err`, `Flush`, and `Finish` return
points into the `Writer` or `Reader`, so compare it with nil, as above,
rather than return or log it, which moves the whole value to the heap.

What `WriteJSON` writes decodes, with the standard library's
`encoding/json`, to what `json.Marshal`'s output decodes to, and `ReadJSON`
decodes what `json.Unmarshal` does, except that a time keeps its instant but
decodes in UTC. `jsongen` refuses types it can't encode or decode without
allocating, such as maps and interfaces, and says why. A type can write its
own `WriteJSON` or `ReadJSON` instead, and
[encoding/json/jsontest](encoding/json/jsontest) checks either kind against
the standard library's by the round trip.

### Performance

Here is the code `jsongen` writes next to the standard library and four
other libraries, each on its own fastest documented path: encoding and
decoding a feed of 500 items, about 130 KB, and a small object the size of
a session cookie, decoding each into a value it reuses. Every library
decodes both to the value `encoding/json` does.

Each figure is CPU seconds, user and system, for 3,000 feed or 3,000,000
cookie operations in a process of the library's own, so it includes garbage
collection and a fixed setup cost. These are medians of four alternating
rounds on an Apple M4 with 10 cores and Go 1.27.0, at a load average between
18 and 37.

| Library | Feed encode (s) | Feed decode (s) | Cookie encode (s) | Cookie decode (s) | Allocations per feed decode |
|---|---|---|---|---|---|
| goccy/go-json v0.11.2 | 0.64 | 0.74 | 0.68 | 0.67 | 4 |
| noalloc's `jsongen` | 0.87 | 1.16 | 0.75 | 1.08 | 0 |
| go-faster/jx v1.2.0 | 0.98 | 1.54 | 0.80 | 1.13 | 3,500 |
| mailru/easyjson v0.9.0 | 0.97 | 1.77 | 0.87 | 1.88 | 4,001 |
| bytedance/sonic v1.15.5 | 1.52 | 1.04 | 1.89 | 1.82 | 3 |
| `encoding/json/v2` | 1.58 | 2.63 | 1.82 | 2.11 | 3,196 |
| `encoding/json` | 1.52 | 2.94 | 1.77 | 2.65 | 1,696 |

sonic's fastest path is built for amd64, so on the M4, an arm64 machine,
these numbers likely understate it.

## Escaping HTML and URLs

[html/template](html/template) has append versions of six of the sixteen
escapers `html/template` picks among: the two for HTML text and attribute
values, the URL filter, normalizer, and escaper, and the step that finishes
a value in a script, given that value already encoded as JSON. It doesn't
parse or execute templates, or escape CSS, JavaScript strings, comments, or
`srcset`. [net/url](net/url) has an append version of `url.QueryEscape`.
Each writes the bytes its standard library counterpart returns, and
allocates only when the slice you pass must grow.

## Common mistakes

These examples share these declarations:

```go
var (
	pool    = bounded.NewPool[byte](4<<10, 8)
	classes = bounded.NewClasses(bounded.NewPool[int](16, 8), bounded.NewPool[int](256, 8))
	r       bounded.Region
	b, next bounded.Buf
	s       bounded.Slice[int]
)
```

### Using memory after `Region.Release`

A `Vec` or `Slice` panics if you use it after its region is released. A `Buf`
or `Map` can't tell, and reads whatever the next borrower wrote:

```go
r.Reset(1 << 20)
b.ResetFrom(&r, pool, nil)
b.WriteString("alice's page")
r.Release()
r.Reset(1 << 20)
next.ResetFrom(&r, pool, nil)
next.WriteString("bob's page..")
fmt.Printf("%q\n", b.Bytes()) // "bob's page.."
```

Release a region only once the request is done with everything it borrowed,
which a `defer` right after `Reset` does. `-race` can catch a request reading
memory another request has borrowed.

### Keeping `Items` or `Bytes` across a write

When a `Slice`, or a `Buf` that can borrow, reaches its capacity, it can move
to a larger chunk and return the old one to its pool. A slice you got from `Items` or `Bytes`
before the move still points at the old chunk:

```go
s.ResetFrom(&r, classes)
s.Push(1)
first := s.Items()
s.Extend(make([]int, 100)) // moves to a larger chunk
first[0] = 2               // writes to a chunk s gave back
```

Call `Items` or `Bytes` again after each write.

### Reserving outside admission

A region holds on to the chunks it has while it waits for the rest. So two
regions that each need both chunks of a two-chunk pool can each take one and
deadlock:

```go
two := bounded.NewPool[byte](64, 2)
for range 2 {
	go func() {
		var r bounded.Region
		r.Reset(1 << 20)
		bounded.Reserve(nil, &r, two, 2) // can deadlock
		r.Release()
	}()
}
```

Reserve only once a request is admitted, and size the pool to hold every
reservation the admitted requests can hold at once.

### Borrowing per item

Borrowing and giving back each take a lock, so borrow once per request rather
than once per item:

```go
for _, line := range lines {
	r.Reset(1 << 20)
	b.ResetFrom(&r, pool, w)
	b.WriteString(line)
	b.Flush()
	r.Release()
}
```

### Sharing a `Region` between goroutines

A `Pool` is safe to share between goroutines but a `Region` isn't. `-race` can
catch this:

```go
r.Reset(1 << 20)
for range 4 {
	go bounded.Take(&r, pool) // races on r
}
```

### Passing `ctx.Done()` to `Wait` or `Reserve`

The first call to `Done` on a cancelable context allocates its channel:

```go
bounded.Reserve(ctx.Done(), &r, pool, 1) // one allocation
```

noalloc counts this as [first-use setup](#first-use-setup) and doesn't
report it, but every new context allocates its own. Pass a channel you make
once instead, such as one your server closes on shutdown.

### Expecting zeroed chunks

A pool only zeroes a chunk that comes back if the chunk's type holds pointers.
Otherwise you get whatever the last borrower left in it:

```go
first := bounded.Take(&r, pool)
first[0] = 'x'
r.Release()
r.Reset(1 << 20)
again := bounded.Take(&r, pool)
fmt.Printf("%q\n", again[0]) // 'x'
```

Clear a chunk yourself if you need zeros.

### Exempting a line to quiet the check

An exemption covers everything the line calls. Here noalloc never looks inside
`format`, so nothing it allocates gets reported:

```go
//noalloc:root
func (p *page) render(w io.Writer) {
	p.format(w) //noalloc:exempt response: the page
}
```

To exempt only the line itself, pass the reason's kind (here `response`) to
`-line-only`.

An exemption still shows what it lets through. `-report` lists, under each
one, the allocations on its line and in what only its line calls, and the
list `-write-exemptions` writes holds them too, indented under the exemption,
so a change in what an exemption covers fails the check until you review it
and write the list again. An exemption whose line is reached but lets
nothing through fails as well: delete it.

## Caveats

### Functions written in assembly

noalloc judges a function with no Go body by its compiled code: it follows the
calls in it to the runtime's allocator. A call through a register can't be
followed, so a function that makes one counts as allocating:

```go
func sum(xs []int) int // written in sum_arm64.s

//noalloc:root
func Total(xs []int) int { return sum(xs) } // reported only if sum's code can reach the allocator
```

When noalloc can't follow a function you know allocates nothing, list it as
`measured` and test it with `alloctest.Zero`. It runs the function through
`testing.AllocsPerRun`, fails when the count isn't zero, and skips the test
under `-race`, which allocates for its own bookkeeping:

```go
func TestSumAllocatesNothing(t *testing.T) {
	xs := []int{1, 2, 3}
	alloctest.Zero(t, "sum", func() { sum(xs) })
}
```

### Calls out of your module

noalloc works out what each function outside your module allocates from the
code your build compiles, and caches it in a `noalloc` directory in your user
cache, one file for each build. The first run on a new toolchain, platform,
set of build tags, or set of dependency versions works it out again, and
takes longer than later runs: on The Consensus's server, 39 seconds against
4. A function it can't follow, such as one that calls through a function
value, counts as allocating, and `-report` gives the chain of calls that led
to the allocation under each finding:

```
counter.go:12: (*example.com/counter.Counter).Add: calls strconv.Itoa; string concatenation; escapes: name + "=" + ~r0 escapes to heap
    because strconv.Itoa calls internal/strconv.Itoa at number.go:215 > internal/strconv.Itoa calls internal/strconv.FormatInt at itoa.go:53 > internal/strconv.FormatInt calls internal/strconv.formatBits at itoa.go:47 > internal/strconv.formatBits: string([]byte) conversion at itoa.go:146
```

### The compiler's output

noalloc parses the output of `go build -gcflags=-m`, and that output can change
between Go releases. Pin your toolchain in `go.mod`, and rerun noalloc when you
upgrade:

```
go 1.27.0

toolchain go1.27.0
```

### What noalloc trusts

noalloc trusts its allowlist, `//noalloc:fixed`, and every exemption without
verifying them. The allowlist is a built-in [list](allow.txt) plus the files
you pass with `-allow`, and it only overrides what noalloc works out: each line
names a function and why it can be taken to allocate nothing even though its
code doesn't show it. The reason is one of these:

- `amortized`: it allocates only to grow memory the runtime keeps and reuses,
  such as the cache a blocked `sync.Mutex` takes its waiter from.
- `measured`: its code can't be followed, and a test that calls it counts no
  allocation.
- `debug` or `profiling`: it allocates only on a path that runs to debug or
  profile.

```
measured: example.com/fmtx.Format
```

The built-in list takes `sync`'s locks and wait groups as `amortized`.
`-report` lists every line and says which ones no check relied on. For a
`measured` line, write the test:

```go
func TestFormatAllocatesNothing(t *testing.T) {
	buf := make([]byte, 0, 64)
	alloctest.Zero(t, "fmtx.Format", func() { fmtx.Format(buf[:0], 42) })
}
```

### Calls it cannot resolve

When noalloc can't tell which function a function value refers to, it reports
the call. For an interface call, it checks every method the call could reach:

```go
//noalloc:root
func Run(f func(int) int, n int) int {
	return f(n)
}
```

```
$ noalloc
run.go:5: example.com/app.Run: call with no known callee: f
```

To fix this, call a concrete function or type instead, or exempt the call.

### Code no root reaches

noalloc only checks code that a root reaches. Nothing calls this function from
a root, so noalloc never looks at it:

```go
func Handle(w io.Writer) {
	fmt.Fprintf(w, "%d", 1)
}
```

So mark every entry point with `//noalloc:root`.

### Error and panic paths

noalloc allows allocations on a failure that ends the root's call: a path
that panics, or returns a non-nil error that reaches the root's return.

```go
//noalloc:root
func Parse(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty input") // noalloc allows this
	}
	return len(s), nil
}
```

A failure that ends the call costs it a fixed number of allocations, however
much input the call reads. One that doesn't end the call is checked like any
other path, because it can happen again on the next item:

```go
for _, s := range items {
	n, err := parse(s) // parse's fmt.Errorf is reported
	if err != nil {
		continue
	}
	total += n
}
```

The same holds for a panic recovered by a function called in a loop, and for
a branch that records an error and goes on, as code that keeps its first
failure does, when it can run more than once in the root's call. Once per
call is allowed:

```go
if n > limit {
	r.fail(fmt.Errorf("%d is over the limit", n)) // noalloc allows this
	return 0
}
```

Code outside the module can call a function you hand it any number of times,
drop its errors, and recover its panics, so a callback's failures are
checked. A root's own failures stay allowed however else it is reached.

If a client can send invalid input on every request, every request still
allocates once. On a path requests can reach frequently, return a sentinel
error made once:

```go
var errEmpty = errors.New("empty input")
```

noalloc follows an error along every path from the call that made it: past
tests against nil and against a package-level sentinel, through
reassignments, and into a helper of the module that returns it. An error
stored in a field, or turned into a result the caller stops on, such as a
flag or a message, looks swallowed, and its failure is reported. A function
that calls itself directly counts as called again, but mutual recursion does
not.

### First-use setup

noalloc allows a branch that finds a field or package variable nil and sets
it, directly or through `sync/atomic`:

```go
if p.index == nil {
	p.index = make(map[string]int, 64) // noalloc allows this
}
```

If something resets the field to nil, say between requests, every request
allocates. Set it once, when you create the value.

### The check against compiled code

noalloc compares its findings with the allocating runtime calls in your
compiled code. That comparison has a few gaps:

- A baseline can't list its failures and exemptions don't cover them, so a run
  keeps failing until you change the code.
- It depends on the compiler's assembly and symbol names, which can change
  between Go releases.
- It counts but doesn't compare calls inlined from other packages, calls on a
  line that also holds code noalloc doesn't check, and calls to runtime
  functions it can't classify:

```
cross-check: 9 allocating calls inlined from other packages and 0 on lines that also hold unchecked code were not compared; calls to runtime functions not known either way: map[typeAssert:3]
```

- It only sees one build: your platform, with the inlining your compiler chose.

## Code that already allocates

If you're adding noalloc to code that already allocates, start a baseline and
check against it:

```
noalloc -baseline noalloc.txt -start
noalloc -baseline noalloc.txt
```

The second command fails on any new allocation, and on a listed allocation
you've since removed until you run `-prune`. To review new exemptions in code
review, keep a list of them: `-exemptions file -write-exemptions` writes it,
and `-exemptions file` fails when the code and the list disagree.

## Performance

bounded's `Pool` and `Map` are slower than `sync.Pool` and Go's `map`, in
exchange for a bounded memory footprint. These are medians of six runs, at a
load average between 3.5 and 11:

| Operation | Go | bounded |
|---|---|---|
| Borrow and return a 16 KB buffer | 8.0 ns (`sync.Pool`) | 15.0 ns |
| The same on 10 cores at once, per borrow | 1.5 ns (`sync.Pool`) | 3.1 ns |
| 1,024 sets and gets | 12.0 µs (`map`) | 17.3 µs (`Map`) |
| Read 4,096 ints | 4.8 µs (slice) | 4.0 µs (`Vec.At`), 4.8 µs (`Vec.Chunks`) |

These numbers come from an Apple M4 with 10 cores and Go 1.27.0 while other
builds were running on it, so compare the rows with each other rather than
with your own machine.

To repeat these comparisons, run `go test -bench .` in `bounded`.

## Prior art

- [OxCaml's zero_alloc checker](https://oxcaml.org/documentation/miscellaneous-extensions/zero_alloc_check/)
  has the compiler prove that a function marked `[@zero_alloc]`, and
  everything it calls, never allocates on the heap.
- [gcassert](https://github.com/jordanlewis/gcassert) checks the specific
  lines you annotate.
- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) counts the
  allocations for the inputs a test happens to run.
- [TigerBeetle](https://github.com/tigerbeetle/tigerbeetle/blob/main/docs/TIGER_STYLE.md)
  allocates all of its memory at startup.
- [PostgreSQL's memory contexts](https://github.com/postgres/postgres/blob/master/src/backend/utils/mmgr/README)
  free all of a query's memory at once.
- [Zig](https://ziglang.org/documentation/master/#Choosing-an-Allocator)
  passes an allocator to every function that allocates.

## License

MIT. See [LICENSE](LICENSE). Code adapted from Go's source keeps Go's BSD
license, in the `GO_LICENSE` or `LICENSE` beside it:
[cmd/jsongen](cmd/jsongen), [html/template](html/template),
[net/url](net/url), and [encoding/json](encoding/json), whose
[internal/strconv](encoding/json/internal/strconv) is a copy of Go's own.
[encoding/json/testdata/JSONTestSuite](encoding/json/testdata/JSONTestSuite)
is Nicolas Seriot's JSONTestSuite, under the MIT license in its directory.
