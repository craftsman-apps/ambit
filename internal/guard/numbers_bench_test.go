package guard

import "testing"

// BenchmarkHasLongNumber is the cost of the scan that every result pays (issue 360), for a 10 MiB result.
func BenchmarkHasLongNumber(b *testing.B) {
	in := bigResult(10 << 20)
	b.SetBytes(int64(len(in)))
	for range b.N {
		hasLongNumber(in)
	}
}
