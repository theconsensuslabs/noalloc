package noalloc

import (
	"slices"
	"strings"
	"testing"
)

const interfaceSrc = `
type shape interface{ area() int }

type smallShape struct{ s int }

func (q smallShape) area() int { return q.s * q.s }

type bigShape struct{ n int }

func (b *bigShape) area() int { return len(make([]int, b.n)) }

//noalloc:root
func OpenShape(s shape) int {
	return s.area()
}

//noalloc:root
func NarrowedShape(n int) int {
	var s shape = smallShape{n}
	return s.area()
}

//noalloc:root
func GenericShape[T shape](t T) int {
	return t.area()
}

//noalloc:root
func GenericWriter[W io.Writer](w W, p []byte) (int, error) {
	return w.Write(p)
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

//noalloc:root
func OpenWriter(w io.Writer, p []byte) (int, error) {
	return w.Write(p)
}

//noalloc:root
func OpenContext(ctx context.Context) error {
	return ctx.Err()
}

type wrappedContext struct{ context.Context }

//noalloc:root
func WrappedContext(ctx context.Context) error {
	var c context.Context = wrappedContext{ctx}
	return c.Err()
}

type wrappedWriter struct{ io.Writer }

//noalloc:root
func MixedWriter(w io.Writer, p []byte, n int) (int, error) {
	var x io.Writer = wrappedWriter{w}
	if n > 0 {
		x = nopWriter{}
	}
	return x.Write(p)
}

//noalloc:root
func WriterMethodExpr(w io.Writer, p []byte) (int, error) {
	write := wrappedWriter.Write
	return write(wrappedWriter{w}, p)
}

//noalloc:root
func WriterMethodValue(w io.Writer, p []byte) (int, error) {
	write := w.Write
	return write(p)
}

type unimplemented interface{ Unimplemented(int) int }

//noalloc:root
func OpenUnimplemented(u unimplemented, n int) int {
	return u.Unimplemented(n)
}
`

// TestEachInterfaceCallIsJudgedByEveryMethodItCanReach checks interface
// method calls VTA cannot resolve against every implementation that could
// answer them: the module's, which are checked where they are defined, and
// those outside it, which are judged by their code, together. So is one that
// SSA makes in a wrapper: a promoted method of an embedded interface, a
// method value, or a method expression. A call with no implementation
// anywhere is reported, as a call through a function value with no known
// callee is.
func TestEachInterfaceCallIsJudgedByEveryMethodItCanReach(t *testing.T) {
	res := checkFrom(t, []string{"context", "io"}, interfaceSrc, nil)
	reasons := func(fn string) []string {
		var out []string
		for _, f := range res.Findings {
			if f.Func == fn {
				out = append(out, f.Reasons...)
			}
		}
		return out
	}
	has := func(fn, want string) bool {
		return slices.ContainsFunc(reasons(fn), func(r string) bool { return strings.Contains(r, want) })
	}
	if !has("(*fixture.bigShape).area", "make") {
		t.Errorf("a module implementation an open call can reach is not checked: %q", reasons("(*fixture.bigShape).area"))
	}
	if !slices.ContainsFunc(res.Findings, func(f Finding) bool {
		return f.Func == "(*fixture.bigShape).area" && slices.Contains(f.Path, "fixture.OpenShape")
	}) {
		t.Error("(*fixture.bigShape).area is not reached from fixture.OpenShape")
	}
	if slices.ContainsFunc(res.Findings, func(f Finding) bool {
		return f.Func == "(*fixture.bigShape).area" && slices.Contains(f.Path, "fixture.NarrowedShape")
	}) {
		t.Error("a call VTA narrows to smallShape reaches (*fixture.bigShape).area")
	}
	if got := reasons("fixture.OpenShape"); len(got) != 0 {
		t.Errorf("an unexported method outside code cannot implement is judged outside the module: %q", got)
	}
	if got := reasons("fixture.GenericShape"); len(got) != 0 {
		t.Errorf("a method call on a type parameter is not judged by its constraint: %q", got)
	}
	if !has("fixture.GenericWriter", "calls") {
		t.Errorf("a type parameter constrained by io.Writer is not judged by the implementations outside the module: %q", reasons("fixture.GenericWriter"))
	}
	if !has("fixture.OpenWriter", "calls io.Writer.Write") {
		t.Errorf("a module implementation of io.Writer hides the ones outside it: fixture.OpenWriter flagged for %q", reasons("fixture.OpenWriter"))
	}
	if got := reasons("fixture.OpenContext"); len(got) != 0 {
		t.Errorf("context.Context.Err, which no implementation allocates in, is flagged for %q", got)
	}
	if got := reasons("fixture.WrappedContext"); len(got) != 0 {
		t.Errorf("a call VTA resolves only to a promoted method of an embedded context.Context is flagged for %q", got)
	}
	if !has("fixture.MixedWriter", "calls io.Writer.Write") {
		t.Errorf("a call VTA resolves to a module method and a promoted method of an embedded io.Writer is not judged by the implementations outside the module: %q", reasons("fixture.MixedWriter"))
	}
	if got := reasons("fixture.WriterMethodExpr"); !has("fixture.WriterMethodExpr", "calls io.Writer.Write") || has("fixture.WriterMethodExpr", "no known callee") {
		t.Errorf("a method expression of a promoted io.Writer method is flagged for %q, want calls io.Writer.Write", got)
	}
	if got := reasons("fixture.WriterMethodValue"); !has("fixture.WriterMethodValue", "calls io.Writer.Write") || has("fixture.WriterMethodValue", "no known callee") {
		t.Errorf("a method value of an io.Writer is flagged for %q, want calls io.Writer.Write", got)
	}
	if !has("fixture.OpenUnimplemented", "call with no known callee") {
		t.Errorf("a call of a method nothing implements is flagged for %q, want a call with no known callee", reasons("fixture.OpenUnimplemented"))
	}
}
