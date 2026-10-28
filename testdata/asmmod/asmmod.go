// Package asmmod calls every kind of callee noalloc judges, from roots.
package asmmod

import (
	"hash"
	_ "hash/crc32"
	_ "hash/fnv"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	_ "unsafe"

	_ "example.com/asmmod/inner"
)

// addAsm and allocAsm are written in assembly; allocAsm calls newInt.
func addAsm(a, b int) int
func allocAsm() *int

//go:noinline
func newInt() *int { return new(int) }

//go:linkname addLinked example.com/asmmod/inner.Add
func addLinked(a, b int) int

//go:linkname allocLinked example.com/asmmod/inner.Alloc
func allocLinked() *int

//go:linkname runLinked example.com/asmmod/inner.Run
func runLinked(f func())

func goAdd(a, b int) int { return a + b }

var sink *int

func goAlloc() { sink = new(int) }

var count int32

//noalloc:root
func ModuleGoAdd(a, b int) int { return goAdd(a, b) }

//noalloc:root
func ModuleGoAlloc() { goAlloc() }

//noalloc:root
func ModuleAsmAdd(a, b int) int { return addAsm(a, b) }

//noalloc:root
func ModuleAsmAlloc() *int { return allocAsm() }

//noalloc:root
func LinkedAdd(a, b int) int { return addLinked(a, b) }

//noalloc:root
func LinkedAlloc() *int { return allocLinked() }

//noalloc:root
func LinkedRun() {
	runLinked(func() {
		sink = new(int)
	})
}

//noalloc:root
func OutsideGoAlloc(n int) string { return strconv.Itoa(n) }

//noalloc:root
func OutsideGoAdd(s string) int { return strings.IndexByte(s, 'x') }

//noalloc:root
func OutsideAsmAdd() int32 { return atomic.AddInt32(&count, 1) }

//noalloc:root
func InterfaceAlloc(w io.Writer, b []byte) { w.Write(b) }

//noalloc:root
func InterfaceAdd(h hash.Hash32) uint32 { return h.Sum32() }

//noalloc:root
func FuncValue(f func(int) int, n int) int { return f(n) }
