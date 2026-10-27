package bounded_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/theconsensuslabs/noalloc/bounded"
)

func ExampleBuf() {
	var b bounded.Buf
	b.Reset(make([]byte, 32), nil)
	b.WriteString("id=")
	b.WriteInt(42)
	fmt.Println(string(b.Bytes()), b.Full())

	b.WriteString(strings.Repeat("x", 40))
	fmt.Println(string(b.Bytes()), b.Full())
	// Output:
	// id=42 false
	// id=42 true
}

func ExampleBuf_flush() {
	var b bounded.Buf
	b.Reset(make([]byte, 8), os.Stdout)
	b.WriteString("hello, ")
	b.WriteString("world\n")
	b.Flush()
	// Output:
	// hello, world
}

func ExampleVec() {
	var v bounded.Vec[int]
	v.Reset(make([]int, 0, 3))
	for i := range 5 {
		v.Push(i * i)
	}
	fmt.Println(v.Items(), v.Full())
	// Output:
	// [0 1 4] true
}

func ExampleVec_ResetFrom() {
	pool := bounded.NewPool[int](4, 8)
	var r bounded.Region
	r.Reset(2 * pool.ChunkBytes())
	defer r.Release()

	var v bounded.Vec[int]
	v.ResetFrom(&r, pool)
	for i := range 10 {
		v.Push(i)
	}
	fmt.Println(v.Len(), v.Full(), v.At(6))
	for c := range v.Chunks() {
		fmt.Println(c)
	}
	// Output:
	// 8 true 6
	// [0 1 2 3]
	// [4 5 6 7]
}

func ExampleSlice() {
	classes := bounded.NewClasses(bounded.NewPool[int](4, 8), bounded.NewPool[int](16, 8))
	var r bounded.Region
	r.Reset(1 << 10)
	defer r.Release()

	var s bounded.Slice[int]
	s.ResetFrom(&r, classes)
	for i := range 10 {
		s.Push(i)
	}
	fmt.Println(s.Items(), s.Cap())
	// Output:
	// [0 1 2 3 4 5 6 7 8 9] 16
}

func ExampleMap() {
	var m bounded.Map[string, int]
	m.Reset(2)
	m.Set("a", 1)
	m.Set("b", 2)
	fmt.Println(m.Set("c", 3), m.Full())

	v, ok := m.Get("a")
	fmt.Println(v, ok, m.Len())
	// Output:
	// false true
	// 1 true 2
}

func ExampleMap_Grow() {
	maps := bounded.NewMapClasses(bounded.NewMapPool[string, int](4, 8), bounded.NewMapPool[string, int](64, 8))
	var r bounded.Region
	r.Reset(1 << 20)
	defer r.Release()

	var m bounded.Map[string, int]
	m.Grow(&r, maps)
	for i := range 20 {
		m.Set(strconv.Itoa(i), i)
	}
	fmt.Println(m.Len(), m.Limit())
	// Output:
	// 20 64
}

func ExamplePool() {
	pool := bounded.NewPool[byte](1<<10, 2)
	var a, b bounded.Region
	a.Reset(4 << 10)
	b.Reset(4 << 10)

	fmt.Println(bounded.Take(&a, pool) != nil, bounded.Take(&a, pool) != nil, bounded.Take(&b, pool) == nil)
	a.Release()
	fmt.Println(bounded.Take(&b, pool) != nil, pool.Free())
	b.Release()

	st := pool.Stats()
	fmt.Println(st.Lent, st.Empty)
	// Output:
	// true true true
	// true 1
	// 3 1
}
