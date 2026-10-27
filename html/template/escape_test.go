package template

import (
	"encoding/json"
	htmltemplate "html/template"
	"math"
	"net/url"
	"slices"
	"strings"
	"testing"
	"text/template/parse"
)

// A context is a place in a page where html/template escapes an action, with
// the escapers it picks there and the same escapers composed from this
// package. Each is checked against html/template's output for the same value.
type context struct {
	name           string
	src            string // the template, with one action
	prefix, suffix string // the text around the action's output
	escapers       []string
	ours           func(dst []byte, s string) []byte
}

var contexts = []context{
	{
		name:     "text",
		src:      `{{.}}`,
		escapers: []string{"_html_template_htmlescaper"},
		ours:     AppendHTML,
	},
	{
		name:     "quoted attribute",
		src:      `<a title="{{.}}">`,
		prefix:   `<a title="`,
		suffix:   `">`,
		escapers: []string{"_html_template_attrescaper"},
		ours:     AppendHTML,
	},
	{
		name:     "URL",
		src:      `<a href="{{.}}">`,
		prefix:   `<a href="`,
		suffix:   `">`,
		escapers: []string{"_html_template_urlfilter", "_html_template_urlnormalizer", "_html_template_attrescaper"},
		ours: func(dst []byte, s string) []byte {
			return AppendHTML(dst, string(AppendURLNormalized(nil, string(AppendURLFiltered(nil, s)))))
		},
	},
	{
		name:     "URL path",
		src:      `<a href="/{{.}}">`,
		prefix:   `<a href="/`,
		suffix:   `">`,
		escapers: []string{"_html_template_urlnormalizer", "_html_template_attrescaper"},
		ours: func(dst []byte, s string) []byte {
			return AppendHTML(dst, string(AppendURLNormalized(nil, s)))
		},
	},
	{
		name:     "URL query",
		src:      `<a href="/?q={{.}}">`,
		prefix:   `<a href="/?q=`,
		suffix:   `">`,
		escapers: []string{"_html_template_urlescaper", "_html_template_attrescaper"},
		ours: func(dst []byte, s string) []byte {
			return AppendHTML(dst, string(AppendURLEscaped(nil, s)))
		},
	},
	{
		name:     "URL query through urlquery",
		src:      `<a href="/?q={{. | urlquery}}">`,
		prefix:   `<a href="/?q=`,
		suffix:   `">`,
		escapers: []string{"urlquery", "_html_template_attrescaper"},
		ours: func(dst []byte, s string) []byte {
			// net/url's AppendQueryEscaped writes what url.QueryEscape returns;
			// its own tests hold it to that.
			return AppendHTML(dst, url.QueryEscape(s))
		},
	},
	{
		name:     "script",
		src:      `<script>var x = {{.}};</script>`,
		prefix:   `<script>var x = `,
		suffix:   `;</script>`,
		escapers: []string{"_html_template_jsvalescaper"},
		ours: func(dst []byte, s string) []byte {
			marshaled, _ := json.Marshal(s)
			return AppendJSValue(dst, marshaled)
		},
	},
}

var scriptContext = contexts[len(contexts)-1]

func (c context) template(t testing.TB) *htmltemplate.Template {
	t.Helper()
	tmpl, err := htmltemplate.New(c.name).Parse(c.src)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// render is html/template's output for the action in c over data.
func (c context) render(t testing.TB, tmpl *htmltemplate.Template, data any) string {
	t.Helper()
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	out, ok := strings.CutPrefix(b.String(), c.prefix)
	if ok {
		out, ok = strings.CutSuffix(out, c.suffix)
	}
	if !ok {
		t.Fatalf("%s: output %q is not wrapped in %q and %q", c.name, b.String(), c.prefix, c.suffix)
	}
	return out
}

// TestEscaperChains checks that html/template still picks the escapers each
// context composes. A Go release that changes them fails here rather than as
// a mismatch in the fuzz tests.
func TestEscaperChains(t *testing.T) {
	for _, c := range contexts {
		tmpl := c.template(t)
		c.render(t, tmpl, "x")
		var got []string
		for _, n := range tmpl.Tree.Root.Nodes {
			action, ok := n.(*parse.ActionNode)
			if !ok {
				continue
			}
			for _, cmd := range action.Pipe.Cmds[1:] {
				got = append(got, cmd.Args[0].String())
			}
		}
		if !slices.Equal(got, c.escapers) {
			t.Errorf("%s: html/template escapes with %v, want %v", c.name, got, c.escapers)
		}
	}
}

var stringSeeds = []string{
	"",
	"plain",
	`<script>alert('x' + "y")</script>`,
	"a&b&amp;c",
	"\x00",
	"nul\x00inside",
	"\xff",
	"bad\xffbyte\xc3",
	"\u2028\u2029",
	"\ufdd0\uffff\ufffd",
	"日本語",
	"javascript:alert(1)",
	"JaVaScRiPt:alert(1)",
	" javascript:x",
	"data:text/html,<b>",
	"http://example.com/a b?c=d&e=f#g",
	"HTTPS://example.com",
	"mailto:a@example.com",
	"/relative:path",
	"a/b:c",
	"%41%zz%4%",
	"%",
	"a b+c~d-e_f.g",
	"!#$&*+,/:;=?@[]",
	"\t\n\r\b\f\x01\x1f\x7f",
	`back\slash"quote`,
	"$x", "x$", "_", "0",
}

func FuzzContexts(f *testing.F) {
	for _, s := range stringSeeds {
		f.Add(s)
	}
	templates := make([]*htmltemplate.Template, len(contexts))
	for i, c := range contexts {
		templates[i] = c.template(f)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// The escapers look at no more than a scheme's worth of input at once,
		// and a long input makes each minimization step run seven templates.
		if len(s) > 256 {
			return
		}
		for i, c := range contexts {
			want := c.render(t, templates[i], s)
			if got := string(c.ours([]byte("keep"), s)); got != "keep"+want {
				t.Errorf("%s of %q:\n got %q\nwant %q", c.name, s, got, "keep"+want)
			}
		}
	})
}

var floatSeeds = []float64{
	0, math.Copysign(0, -1), 1, -1, 0.1, 123.456, 1e-6, 9.999999e-7, 1e-7, 1e20, 1e21, 1.5e300,
	math.MaxFloat64, math.SmallestNonzeroFloat64, math.MaxFloat32, math.SmallestNonzeroFloat32,
	math.Inf(1), math.Inf(-1), math.NaN(), 16777217, 1e-9, 5e-324,
}

// FuzzJSValueFloat checks AppendJSValue against jsValEscaper over every
// float json.Marshal writes.
func FuzzJSValueFloat(f *testing.F) {
	for _, v := range floatSeeds {
		f.Add(math.Float64bits(v))
		f.Add(uint64(math.Float32bits(float32(v))))
	}
	script := scriptContext.template(f)
	f.Fuzz(func(t *testing.T, bits uint64) {
		check := func(v any) {
			marshaled, err := json.Marshal(v)
			if err != nil {
				return
			}
			want := scriptContext.render(t, script, v)
			if js := AppendJSValue(nil, marshaled); string(js) != want {
				t.Fatalf("script value of %v = %q, html/template = %q", v, js, want)
			}
		}
		check(math.Float64frombits(bits))
		check(math.Float32frombits(uint32(bits)))
	})
}

// FuzzJSValue checks AppendJSValue against jsValEscaper over any JSON that
// json.Marshal writes, which reaches the padding and line separator handling
// with values no string or float encodes to.
func FuzzJSValue(f *testing.F) {
	for _, s := range []string{
		`null`, `true`, `0`, `-1`, `"s"`, `[]`, `{}`, `[1,"a"]`, `{"a":1}`, `1e5`,
		"\"\u2028\u2029\"", `"\u2028"`, `{"k":"<\/script>"}`,
	} {
		f.Add([]byte(s))
	}
	script := scriptContext.template(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if !json.Valid(b) {
			return
		}
		raw := json.RawMessage(b)
		marshaled, err := json.Marshal(raw)
		if err != nil {
			return
		}
		want := scriptContext.render(t, script, raw)
		if got := string(AppendJSValue([]byte("keep"), marshaled)); got != "keep"+want {
			t.Fatalf("AppendJSValue(%q) = %q, html/template = %q", marshaled, got[4:], want)
		}
	})
}

// TestJSValueEscapesLineSeparators covers what no json.Marshal output reaches,
// since it escapes U+2028 and U+2029 itself: raw separators in the input.
func TestJSValueEscapesLineSeparators(t *testing.T) {
	for in, want := range map[string]string{
		"\"a\u2028b\u2029\"": `"a\u2028b\u2029"`,
		"":                   " null ",
		"x\u2028":            ` x\u2028 `,
	} {
		if got := string(AppendJSValue(nil, []byte(in))); got != want {
			t.Errorf("AppendJSValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAppendsAllocateNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates for its own bookkeeping")
	}
	// Each input reaches every branch of the functions it is given to.
	text := "plain <a href=\"x\">&'+</a>\x00 日本\xff\u2028 %41%zz !#$*,/:;=?@[] -._~\t\n\r\b\f\x01\\"
	json, _ := json.Marshal(text)
	dst := make([]byte, 0, 4096)
	for name, run := range map[string]func(){
		"AppendHTML":          func() { dst = AppendHTML(dst[:0], text) },
		"AppendURLFiltered":   func() { dst = AppendURLFiltered(AppendURLFiltered(dst[:0], text), "https://x") },
		"AppendURLNormalized": func() { dst = AppendURLNormalized(dst[:0], text) },
		"AppendURLEscaped":    func() { dst = AppendURLEscaped(dst[:0], text) },
		"AppendJSValue":       func() { dst = AppendJSValue(AppendJSValue(dst[:0], json), []byte("\"\u2028\"")) },
	} {
		if allocs := testing.AllocsPerRun(100, run); allocs != 0 {
			t.Errorf("%s allocates %v times per call into a buffer with room", name, allocs)
		}
	}
}
