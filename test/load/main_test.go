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

func TestBodiesAlternate(t *testing.T) {
	a, b := []byte("1"), []byte("2")
	var got string
	for i := 0; i < 4; i++ {
		got += string(bodyFor(i, [][]byte{a, b}))
	}
	if got != "1212" || string(bodyFor(3, [][]byte{a})) != "1" {
		t.Fatalf("%q", got)
	}
}

func TestAnswerLine(t *testing.T) {
	line := answerLine(200, []byte(`{"model":"mock/echo","choices":[{"message":{"content":"pong x-ambit-route=coding x-ambit-config=abc"}}]}`))
	if line != `{"content":"pong x-ambit-route=coding x-ambit-config=abc","model":"mock/echo","status":200}` {
		t.Fatal(line)
	}
	if l := answerLine(500, []byte("upstream error")); l != `{"content":"","model":"","status":500}` {
		t.Fatal(l)
	}
}
