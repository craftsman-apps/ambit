package guard

import "slices"

// matcher finds a set of words in a text in one pass (Aho-Corasick, as a table: one load for each byte). It reads ASCII
// letters as lower case, so the words are lower case and the text is not copied.
type matcher struct {
	next     []uint16 // state*256+byte: the next state
	firstOut uint16   // the states from this one have an output: a word ends here
	out      [][]uint16
}

var lowerTab = func() (t [256]byte) {
	for i := range t {
		t[i] = byte(i)
		if i >= 'A' && i <= 'Z' {
			t[i] = byte(i) + 'a' - 'A'
		}
	}
	return
}()

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
	m := &matcher{next: make([]uint16, len(next)*256), firstOut: uint16(firstOut), out: make([][]uint16, len(next))}
	for old, row := range next {
		for c, n := range row {
			m.next[renum[old]*256+c] = uint16(renum[n])
		}
		m.out[renum[old]] = slices.Clone(outs[old])
	}
	return m
}

// hit is a word found: its number and the offset of its last byte plus one.
type hit struct{ word, end int }

// scan returns where the words end in s. nil when there is none: a result without these words costs one pass.
func (m *matcher) scan(s string) (hits []hit) {
	st := uint16(0)
	for i := 0; i < len(s); i++ {
		st = m.next[int(st)<<8|int(lowerTab[s[i]])]
		if st >= m.firstOut {
			for _, w := range m.out[st] {
				hits = append(hits, hit{int(w), i + 1})
			}
		}
	}
	return hits
}
