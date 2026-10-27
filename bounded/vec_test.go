package bounded

import (
	"slices"
	"testing"

	"github.com/theconsensuslabs/noalloc/alloctest"
)

func TestVecFillsToItsCapacityAndStaysFull(t *testing.T) {
	var v Vec[int]
	v.Reset(make([]int, 1, 3))
	for i := range 5 {
		if ok := v.Push(i); ok != (i < 3) {
			t.Errorf("Push(%d) = %v, want %v", i, ok, i < 3)
		}
	}
	if !slices.Equal(v.Items(), []int{0, 1, 2}) || !v.Full() || v.Cap() != 3 {
		t.Fatalf("items %v, full %v, cap %d; want [0 1 2], full, cap 3", v.Items(), v.Full(), v.Cap())
	}
	v.Truncate(1)
	if !v.Push(7) || !slices.Equal(v.Items(), []int{0, 7}) || v.Full() {
		t.Errorf("after Truncate(1) and Push(7): %v, full %v", v.Items(), v.Full())
	}
	v.Clear()
	if v.Len() != 0 || v.Full() {
		t.Errorf("after Clear: len %d, full %v", v.Len(), v.Full())
	}
}

func TestVecTruncatePastLenPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Truncate past Len did not panic")
		}
	}()
	var v Vec[int]
	v.Reset(make([]int, 0, 4))
	v.Push(1)
	v.Truncate(2)
}

func TestVecAllocatesNothing(t *testing.T) {
	var v Vec[string]
	v.Reset(make([]string, 0, 8))
	alloctest.Zero(t, "a fill and clear", func() {
		for range 10 {
			v.Push("x")
		}
		_ = v.Items()
		v.Clear()
	})
}
