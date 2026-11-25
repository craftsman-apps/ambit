package guard

import (
	"math/bits"
	"slices"
)

// matcher finds a set of words in a text in one pass (Aho-Corasick, as a table: one load for each byte). It reads ASCII
// letters as lower case, so the words are lower case and the text is not copied.
type matcher struct {
	class    [256]uint8 // the class of a byte (lower-cased): 0 for a byte that is in no word, else one class for each byte of the words
	classes  int
	shift    uint     // a state's row has 1<<shift steps (one for each class, rounded up)
	next     []uint16 // state<<shift | class: the next state
	firstOut uint16   // the states from this one have an output: a word ends here
	// start has a bit for each (hashed) three bytes that begin a word. Almost no position in a text begins a word, so the scan
	// tests the bytes in threes, which needs no result of the step before, and runs the table only where a word may begin.
	// Two bytes would be too few: many words begin with "th" or "in", and prose has them everywhere.
	start  [1 << 10]uint64
	maxLen int
	words  []string
	out    [][]uint16
}

// lowerTab reads ASCII letters as lower case and every white space byte as a space.
var lowerTab = func() (t [256]byte) {
	for i := range t {
		t[i] = byte(i)
		switch {
		case i >= 'A' && i <= 'Z':
			t[i] = byte(i) + 'a' - 'A'
		case i == '\t' || i == '\n' || i == '\v' || i == '\f' || i == '\r':
			t[i] = ' '
		}
	}
	return
}()

// maxHits is the most words that scan keeps for one text. A text with more is made to hit the rules, not written.
const maxHits = 200_000

func newMatcher(words []string) *matcher {
	next := [][256]int32{{}}
	for i := range next[0] {
		next[0][i] = -1
	}
	outs := [][]uint16{nil}
	for id, w := range words {
		st := 0
		for i := 0; i < len(w); i++ {
			c := w[i]
			if next[st][c] < 0 {
				var row [256]int32
				for j := range row {
					row[j] = -1
				}
				next = append(next, row)
				outs = append(outs, nil)
				next[st][c] = int32(len(next) - 1)
			}
			st = int(next[st][c])
		}
		outs[st] = append(outs[st], uint16(id))
	}
	// Breadth first: fail links, outputs through them, and the missing steps (so each state has a step for every byte).
	fail := make([]int, len(next))
	var queue []int
	for c := 0; c < 256; c++ {
		if n := next[0][c]; n < 0 {
			next[0][c] = 0
		} else {
			queue = append(queue, int(n))
		}
	}
	for len(queue) > 0 {
		st := queue[0]
		queue = queue[1:]
		outs[st] = append(outs[st], outs[fail[st]]...)
		for c := 0; c < 256; c++ {
			n := next[st][c]
			if n < 0 {
				next[st][c] = next[fail[st]][c]
				continue
			}
			fail[n] = int(next[fail[st]][c])
			queue = append(queue, int(n))
		}
	}
	// Number the states with an output last, so a byte loop tests "state >= firstOut" and nothing else.
	order := make([]int, 0, len(next))
	for st := range next {
		if len(outs[st]) == 0 {
			order = append(order, st)
		}
	}
	firstOut := len(order)
	for st := range next {
		if len(outs[st]) > 0 {
			order = append(order, st)
		}
	}
	renum := make([]int, len(next))
	for newSt, old := range order {
		renum[old] = newSt
	}
	// A table of 256 steps for each state is large and slow to read (every state is a cache line or more): the bytes that are in
	// no word all behave the same, so each byte of the words gets a class, and every other byte class 0.
	m := &matcher{firstOut: uint16(firstOut), out: make([][]uint16, len(next)), classes: 1, words: words}
	for _, w := range words {
		m.maxLen = max(m.maxLen, len(w))
		if len(w) == 2 { // a word of two bytes: every third byte
			for c := 0; c < 256; c++ {
				m.mark(w[0], w[1], byte(c))
			}
			continue
		}
		m.mark(w[0], w[1], w[2])
	}
	var used [256]bool
	for _, w := range words {
		for i := 0; i < len(w); i++ {
			used[w[i]] = true
		}
	}
	for c := 0; c < 256; c++ {
		if used[c] {
			m.class[c] = uint8(m.classes)
			m.classes++
		}
	}
	m.shift = uint(bits.Len(uint(m.classes - 1)))
	m.next = make([]uint16, len(next)<<m.shift)
	for old, row := range next {
		for c, n := range row {
			if cl := int(m.class[c]); c == 0 || cl != 0 {
				m.next[renum[old]<<m.shift|cl] = uint16(renum[n])
			}
		}
		m.out[renum[old]] = slices.Clone(outs[old])
	}
	return m
}

// startHash is the bit of three bytes in start.
func startHash(a, b, c byte) uint {
	return uint((uint32(a)*0x9E37+uint32(b)*0x85EB+uint32(c)*0xC2B1)>>3) & 0xFFFF
}

func (m *matcher) mark(a, b, c byte) {
	p := startHash(a, b, c)
	m.start[p>>6] |= 1 << (p & 63)
}

// hit is a word found: its number, and where it begins and ends in the text.
type hit struct{ word, start, end int }

// scan returns the words found in s, by end offset. A run of white space of any kind is one space for the words, so
// "from  now\ton" is the word "from now on". nil when there is none: a result without these words costs one pass.
// overflow is true when there are more than maxHits words; the rest are not kept.
func (m *matcher) scan(s string) (hits []hit, overflow bool) {
	for i := 0; i+1 < len(s); i++ {
		var third byte // a word of two bytes at the very end of the text has none
		if i+2 < len(s) {
			third = lowerTab[s[i+2]]
		}
		p := startHash(lowerTab[s[i]], lowerTab[s[i+1]], third)
		if m.start[p>>6]>>(p&63)&1 == 0 {
			continue
		}
		st, consumed, space := uint16(0), 0, false
		for j := i; j < len(s) && consumed < m.maxLen; j++ {
			c := lowerTab[s[j]]
			if c == ' ' {
				if space {
					continue
				}
				space = true
			} else {
				space = false
			}
			consumed++
			st = m.next[int(st)<<m.shift|int(m.class[c])]
			if st == 0 {
				break
			}
			if st >= m.firstOut {
				for _, w := range m.out[st] {
					if len(m.words[w]) == consumed { // a word that begins here; the others begin at their own position
						if len(hits) >= maxHits {
							slices.SortFunc(hits, func(a, b hit) int { return a.end - b.end })
							return hits, true
						}
						hits = append(hits, hit{int(w), i, j + 1})
					}
				}
			}
		}
	}
	slices.SortFunc(hits, func(a, b hit) int { return a.end - b.end })
	return hits, false
}
