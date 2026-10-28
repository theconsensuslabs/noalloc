// Package inlcaller calls inldep's functions from roots.
package inlcaller

import "example.com/inldep"

var sink any

//noalloc:root
func CallBoxed(n int) {
	sink = inldep.Boxed(n)
}

//noalloc:root
func CallUnboxed(n int) int {
	return inldep.Unboxed(n)
}

//noalloc:root
func CallInstall(n int) {
	inldep.Install(n)
}

//noalloc:root
func CallHook(n int) int {
	return inldep.Hook(n)
}

//noalloc:root
func RangeValues(xs []int) int {
	t := 0
	for x := range inldep.Values(xs) {
		t += x
	}
	return t
}

//noalloc:root
func CallTotal(xs []int) int {
	return inldep.Total(xs)
}

//noalloc:root
func CallCount(b []byte) int {
	return inldep.Count(b)
}
