package guard

import (
	"strings"
	"testing"
)

const pytestRun = `============================= test session starts ==============================
platform linux -- Python 3.12.1, pytest-8.1.1
collected 5 items

tests/test_a.py::test_one PASSED                                         [ 20%]
tests/test_a.py::test_two PASSED                                         [ 40%]
tests/test_a.py::test_three FAILED                                       [ 60%]
tests/test_a.py::test_four PASSED                                        [ 80%]
tests/test_a.py::test_five PASSED                                        [100%]

=================================== FAILURES ===================================
___________________________________ test_three _________________________________

    def test_three():
>       assert add(1, 2) == 4
E       assert 3 == 4

tests/test_a.py:12: AssertionError
=========================== short test summary info ============================
FAILED tests/test_a.py::test_three - assert 3 == 4
========================= 1 failed, 4 passed in 0.12s ==========================
`

const jestRun = `PASS src/a.test.js
  math
    ✓ adds (3 ms)
    ✓ subtracts (1 ms)
FAIL src/b.test.js
  ● strings › trims

    expect(received).toBe(expected)

    Expected: "a"
    Received: "a "

      at Object.<anonymous> (src/b.test.js:5:20)

Tests:       1 failed, 2 passed, 3 total
Test Suites: 1 failed, 1 passed, 2 total
`

func TestCompressTestOutputShortenKeepsFailuresAndSummary(t *testing.T) {
	in := goTestVerbose(400, 2)
	out, omitted, ok := compressTestOutput(in, true)
	if !ok || omitted != 1 {
		t.Fatalf("ok %v omitted %d", ok, omitted)
	}
	for _, want := range []string{"--- FAIL: TestBroken0", "--- FAIL: TestBroken1", "broken_test.go:10: got 3, want 4", "FAIL\texample.com/app/pkg\t0.412s", "[ambit: 400 passing tests omitted here]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--- PASS") {
		t.Error("passing test lines kept")
	}
	if r := float64(len(out)) / float64(len(in)); r > 0.1 {
		t.Errorf("ratio %.3f", r)
	}
}

func TestCompressTestOutputPytestAndJest(t *testing.T) {
	out, _, ok := compressTestOutput(pytestRun, true)
	if !ok || strings.Contains(out, "PASSED") || !strings.Contains(out, "assert 3 == 4") || !strings.Contains(out, "1 failed, 4 passed") || !strings.Contains(out, "[ambit: 4 passing tests omitted here]") {
		t.Errorf("pytest:\n%s", out)
	}
	out, _, ok = compressTestOutput(jestRun, true)
	if !ok || strings.Contains(out, "✓") || !strings.Contains(out, `Received: "a "`) || !strings.Contains(out, "Tests:       1 failed") || !strings.Contains(out, "FAIL src/b.test.js") {
		t.Errorf("jest:\n%s", out)
	}
}

func TestCompressTestOutputLosslessRemovesColorOnly(t *testing.T) {
	in := "\x1b[31m--- FAIL: TestX (0.00s)\x1b[0m\n"
	out, omitted, ok := compressTestOutput(in, false)
	if !ok || omitted != 0 || out != "--- FAIL: TestX (0.00s)\n" {
		t.Fatalf("%q %d %v", out, omitted, ok)
	}
}

func TestCompressTestOutputNotATestRunIsUnknown(t *testing.T) {
	if _, _, ok := compressTestOutput(htmlPage, true); ok {
		t.Error("html")
	}
}

func FuzzCompressTestOutput(f *testing.F) {
	f.Add(goTestVerbose(3, 1))
	f.Fuzz(func(t *testing.T, in string) {
		if out, _, ok := compressTestOutput(in, true); ok && len(out) > len(in)+64 {
			t.Fatal("grew")
		}
	})
}
