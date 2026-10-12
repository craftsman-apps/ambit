package main

import (
	"testing"
	"time"
)

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

// A stop (SIGTERM from a test script whose event is over) ends sending at once; requests in flight still finish.
func TestDispatchStopsOnSignal(t *testing.T) {
	ticks, stop := make(chan time.Time), make(chan struct{})
	start := time.Now()
	sent := 0
	done := make(chan int)
	go func() { done <- dispatch(ticks, stop, start, time.Hour, func(time.Time) { sent++ }) }()
	for i := 0; i < 3; i++ {
		ticks <- start.Add(time.Duration(i) * time.Second)
	}
	close(stop)
	if n := <-done; n != 3 || sent != 3 {
		t.Fatalf("sent %d (returned %d), want 3", sent, n)
	}
}

// Without a stop, sending ends after the duration.
func TestDispatchEndsAfterDuration(t *testing.T) {
	ticks := make(chan time.Time, 5)
	start := time.Now()
	for _, s := range []int{0, 1, 2, 11, 12} {
		ticks <- start.Add(time.Duration(s) * time.Second)
	}
	if n := dispatch(ticks, make(chan struct{}), start, 10*time.Second, func(time.Time) {}); n != 3 {
		t.Fatalf("sent %d, want 3", n)
	}
}
