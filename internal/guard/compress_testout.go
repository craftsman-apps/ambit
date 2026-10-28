package guard

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// passRe matches the line of a passing test. runRe matches a go test line that only announces a test.
	passRe = regexp.MustCompile(`^\s*--- PASS: |\bPASSED\b|^\s*[✓√✔] `)
	runRe  = regexp.MustCompile(`^=== (RUN|PAUSE|CONT)\s|^\s*--- SKIP: |^PASS( |$)`)
)

// isTestOutput looks for the marks of go test, pytest and jest at the start of a line.
func isTestOutput(in string) bool {
	if strings.Contains(in, " test session starts =") {
		return true
	}
	for _, m := range []string{"--- FAIL", "--- PASS", "=== RUN", "ok  \t", "FAIL\t", "Tests:  ", "Test Suites:"} {
		if strings.HasPrefix(in, m) || strings.Contains(in, "\n"+m) {
			return true
		}
	}
	return false
}

// compressTestOutput: lossless removes ANSI color codes only. shorten also drops the lines of passing tests
// (go test -v, pytest -v, jest), keeps everything else (failures with their messages and stacks, the
// summary), and says how many passing tests it left out in one marker where the first one was.
func compressTestOutput(in string, shorten bool) (out string, omitted int, ok bool) {
	in = ansiRe.ReplaceAllString(in, "")
	if !isTestOutput(in) {
		return "", 0, false
	}
	if !shorten {
		return in, 0, true
	}
	lines := strings.Split(strings.TrimRight(in, "\n"), "\n")
	passing, at := 0, -1
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		switch {
		case passRe.MatchString(l):
			passing++
			if at < 0 {
				at = len(kept)
				kept = append(kept, "")
			}
		case runRe.MatchString(l):
		default:
			kept = append(kept, l)
		}
	}
	if passing == 0 {
		return in, 0, true
	}
	kept[at] = fmt.Sprintf("[ambit: %d passing tests omitted here]", passing)
	return strings.Join(kept, "\n") + "\n", 1, true
}
