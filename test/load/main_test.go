package main

import "testing"

func TestPercentiles(t *testing.T) {
	var d []float64
	for i := 1; i <= 100; i++ {
		d = append(d, float64(i))
	}
	p := percentiles(d)
	if p[50] != 50 || p[95] != 95 || p[99] != 99 {
		t.Fatalf("%v", p)
	}
	if len(percentiles(nil)) != 0 {
		t.Fatal("empty input")
	}
}
