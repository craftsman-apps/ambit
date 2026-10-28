package guard

import (
	"fmt"
	"testing"
)

// sized returns a fixture of about size bytes (the generators grow linearly with n).
func sized(gen func(n int) string, size int) string {
	n := 1000
	for range 3 {
		n = max(10, n*size/len(gen(n)))
	}
	return gen(n)
}

var benchTypes = []struct {
	name string
	gen  func(n int) string
}{
	{"json", queryRows},
	{"table", csvExport},
	{"markdown", markdownTable},
	{"yaml", podYAML},
	{"logs", func(n int) string { return serviceLog(n, n/250) }},
	{"test", func(n int) string { return goTestVerbose(n, 2) }},
}

// BenchmarkCompress reports time and memory (-benchmem) of each compressor at 100 KB, 1 MB and 4 MB.
func BenchmarkCompress(b *testing.B) {
	for _, tc := range benchTypes {
		for _, size := range []int{100 << 10, 1 << 20, 4 << 20} {
			text := sized(tc.gen, size)
			for _, mode := range []string{ModeLossless, ModeShorten} {
				b.Run(fmt.Sprintf("%s/%s/%dKB", tc.name, mode, size>>10), func(b *testing.B) {
					b.SetBytes(int64(len(text)))
					for range b.N {
						detect(text, mode == ModeShorten, true)
					}
					out, _, _ := detect(text, mode == ModeShorten, true)
					b.ReportMetric(100*float64(len(out))/float64(len(text)), "%size")
				})
			}
		}
	}
}

// BenchmarkCheckResponse is the whole path (parse, compress, mask, encode) for a result of one text.
func BenchmarkCheckResponse(b *testing.B) {
	for _, size := range []int{100 << 10, 1 << 20, 4 << 20} {
		in := textResult(sized(func(n int) string { return serviceLog(n, n/250) }, size))
		for _, mode := range []string{ModeOff, ModeShorten} {
			b.Run(fmt.Sprintf("%s/%dKB", mode, size>>10), func(b *testing.B) {
				g := &Guard{Masker: Builtin{}, Compression: on(mode)}
				for range b.N {
					call(b, g, append([]byte(nil), in...))
				}
			})
		}
	}
}
