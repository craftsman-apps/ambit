package main

import "testing"

func TestEnvInt(t *testing.T) {
	for val, want := range map[string]int{"": 30, "90": 90, "abc": 30, "0": 0} {
		t.Setenv("AMBIT_TEST_INT", val)
		if got := envInt("AMBIT_TEST_INT", 30); got != want {
			t.Errorf("%q: got %d, want %d", val, got, want)
		}
	}
}
