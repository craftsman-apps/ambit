package guard

import (
	"fmt"
	"regexp"
	"strings"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

var (
	levelWords = []string{"INFO", "WARN", "WARNING", "ERROR", "DEBUG", "FATAL", "TRACE"}
	// importantWords mark the lines shorten always keeps (matched as they are written, upper case).
	importantWords = []string{"WARN", "ERROR", "FATAL", "PANIC", "CRITICAL", "level=warn", "level=error", "level=fatal", "panic:", "Traceback", "Exception", "goroutine "}
)

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// logStart reports whether the line begins like a log record: a date and time, a time, a glog prefix (E0102 15:04),
// a level word (also in brackets), level= or time=.
func logStart(l string) bool {
	l = strings.TrimLeft(l, " \t")
	l = strings.TrimPrefix(l, "[")
	switch {
	case len(l) >= 16 && isDigit(l[0]) && isDigit(l[3]) && l[4] == '-' && isDigit(l[5]) && l[7] == '-' && isDigit(l[8]) && isDigit(l[11]) && l[13] == ':': // 2026-03-01T10:00
		return true
	case len(l) >= 8 && isDigit(l[0]) && isDigit(l[1]) && l[2] == ':' && isDigit(l[3]) && l[5] == ':': // 10:00:01
		return true
	case len(l) >= 8 && strings.ContainsRune("IWEF", rune(l[0])) && isDigit(l[1]) && isDigit(l[4]) && l[5] == ' ' && l[8] == ':': // E0102 15:04
		return true
	case strings.HasPrefix(l, "level=") || strings.HasPrefix(l, "time="):
		return true
	}
	for _, w := range levelWords {
		if len(l) >= len(w) && strings.EqualFold(l[:len(w)], w) && (len(l) == len(w) || !isWordByte(l[len(w)])) {
			return true
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || isDigit(c) || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

func isImportant(l string) bool {
	for _, w := range importantWords {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

const (
	logKeepEdge    = 20 // lines kept at the start and at the end
	logKeepContext = 2  // lines kept around an important line
	minLogLines    = 20
)

// compressLogs: lossless removes trailing spaces and ANSI color codes. shorten also drops lines that repeat an
// earlier line (compared without timestamp and numbers), except the first 20 and last 20 lines, every
// WARN/ERROR/FATAL/panic line and the stack that follows it, and 2 lines of context around them. One marker
// stands for each run of dropped lines; omitted counts the markers.
func compressLogs(in string, shorten bool) (out string, omitted int, ok bool) {
	if strings.Contains(in, "\x1b") {
		in = ansiRe.ReplaceAllString(in, "")
	}
	in = strings.TrimRight(in, "\n")
	n, start := strings.Count(in, "\n")+1, 0
	if n < minLogLines {
		return "", 0, false
	}
	for rest := in; rest != ""; { // count without copying the lines: most texts are not logs
		var l string
		l, rest, _ = strings.Cut(rest, "\n")
		if logStart(l) {
			start++
		}
	}
	if start*10 < n*6 {
		return "", 0, false
	}
	lines := strings.Split(in, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	if !shorten {
		return strings.Join(lines, "\n") + "\n", 0, true
	}
	keep := make([]bool, len(lines))
	for i := range lines {
		keep[i] = i < logKeepEdge || i >= len(lines)-logKeepEdge
	}
	important := false
	for i, l := range lines {
		isStart := logStart(l)
		switch {
		case isImportant(l):
			important = true
		case isStart:
			important = false
		}
		if important { // the line itself or its stack continuation
			for j := max(0, i-logKeepContext); j <= min(len(lines)-1, i+logKeepContext); j++ {
				keep[j] = true
			}
		}
	}
	seen := map[string]bool{}
	var key []byte
	var b strings.Builder
	b.Grow(len(in) / 4)
	dropped := 0
	flush := func() {
		if dropped > 0 {
			fmt.Fprintf(&b, "[ambit: %d repeated lines omitted here]\n", dropped)
			omitted++
			dropped = 0
		}
	}
	for i, l := range lines {
		key = logKey(key[:0], l)
		repeat := seen[string(key)] // no allocation for a lookup
		if !repeat {
			seen[string(key)] = true
		}
		if keep[i] || !repeat {
			flush()
			b.WriteString(l + "\n")
			continue
		}
		dropped++
	}
	flush()
	return b.String(), omitted, true
}

// logKey appends the line with every run of digits as one "#" (so timestamps and ids of all lines look alike),
// the form in which lines are compared.
func logKey(key []byte, l string) []byte {
	for i := 0; i < len(l); i++ {
		if l[i] >= '0' && l[i] <= '9' {
			if len(key) == 0 || key[len(key)-1] != '#' {
				key = append(key, '#')
			}
			continue
		}
		key = append(key, l[i])
	}
	return key
}
