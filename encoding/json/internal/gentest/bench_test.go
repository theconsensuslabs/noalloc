package gentest_test

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/theconsensuslabs/noalloc/encoding/json"
	"github.com/theconsensuslabs/noalloc/encoding/json/internal/gentest"
)

func benchCookie() *gentest.Cookie {
	return &gentest.Cookie{UserID: gentest.ID{0x6f, 0x1c, 0x2a, 0x9e}, Exp: 1793059200, Kind: "session",
		Flags: []string{"member", "beta"}}
}

// benchFeed is 500 items of words, about 130 KB of JSON.
func benchFeed() *gentest.Feed {
	random := rand.New(rand.NewPCG(1, 2))
	words := strings.Fields("lock free ring buffer postgres vacuum btree page split wal fsync io_uring kernel scheduler arena allocator latency tail p99 shard replica raft log")
	sentence := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = words[random.IntN(len(words))]
		}
		return strings.Join(parts, " ")
	}
	feed := make(gentest.Feed, 500)
	for i := range feed {
		feed[i] = gentest.Item{
			Title:  sentence(8),
			URL:    fmt.Sprintf("https://example.com/blog/%d/%s", i, strings.ReplaceAll(sentence(4), " ", "-")),
			Score:  random.Float64() * 100,
			Posted: 1790000000 + int64(random.IntN(1e7)),
			Tags:   strings.Fields(sentence(3)),
			Author: gentest.Author{Name: sentence(2), Domain: "example.com"},
		}
	}
	return &feed
}

// pooledFeed is a Feed with room for n items of up to 8 tags each, as a
// pooled reply would hold.
func pooledFeed(n int) *gentest.Feed {
	feed := make(gentest.Feed, n)
	for i := range feed {
		feed[i].Tags = make([]string, 0, 8)
	}
	feed = feed[:0]
	return &feed
}

type payload struct {
	name    string
	v       writable
	pooled  func() any
	readGen func(*json.Reader, any)
}

func payloads() []payload {
	return []payload{
		{"cookie", benchCookie(), func() any { return &gentest.Cookie{Flags: make([]string, 0, 8)} },
			func(r *json.Reader, v any) { v.(*gentest.Cookie).ReadJSON(r) }},
		{"feed", benchFeed(), func() any { return pooledFeed(500) },
			func(r *json.Reader, v any) { v.(*gentest.Feed).ReadJSON(r) }},
	}
}

func BenchmarkEncode(b *testing.B) {
	for _, p := range payloads() {
		data, _ := stdjson.Marshal(p.v)
		b.Run(p.name+"/encoding-json", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := stdjson.Marshal(p.v); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(p.name+"/generated", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			var w json.Writer
			mem := make([]byte, 2*len(data))
			for b.Loop() {
				w.Reset(mem, nil)
				p.v.WriteJSON(&w)
				if err := w.Err(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	for _, p := range payloads() {
		data, _ := stdjson.Marshal(p.v)
		b.Run(p.name+"/encoding-json", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			dst := p.pooled()
			for b.Loop() {
				if err := stdjson.Unmarshal(data, dst); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(p.name+"/generated", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			dst := p.pooled()
			var r json.Reader
			mem := make([]byte, 0, len(data))
			for b.Loop() {
				r.Arena.Reset(mem)
				r.ResetBytes(data)
				p.readGen(&r, dst)
				if err := r.Finish(); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(p.name+"/generated-streamed", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			dst := p.pooled()
			var r json.Reader
			var src bytes.Reader
			mem, buf := make([]byte, 0, len(data)), make([]byte, 4096)
			for b.Loop() {
				r.Arena.Reset(mem)
				src.Reset(data)
				r.Reset(&src, buf)
				p.readGen(&r, dst)
				if err := r.Finish(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
