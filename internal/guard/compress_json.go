package guard

import (
	"bytes"
	"encoding/json"
)

// compressJSON removes the whitespace between tokens and, when columnar is set, writes an array of 5 or more
// objects with the same keys in the same order as {"columns":[...],"rows":[[...],...]}. Keys, values and their
// order are copied byte for byte (no float round trip), so the inverse is exact. It works on the bytes
// of valid JSON and never builds a tree. ok is false for text that is not a JSON object or array, or that has
// a duplicate key in one object (a columnar form would hide it).
func compressJSON(in string, columnar bool) (out string, ok bool) {
	t := trimJSON(in)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') || !json.Valid(t) {
		return "", false
	}
	c := &jsonCompactor{in: t, columnar: columnar, buf: bytes.NewBuffer(make([]byte, 0, len(t)))}
	c.value(0)
	if c.dup {
		return "", false
	}
	return c.buf.String(), true
}

// trimJSON trims the JSON whitespace around in.
func trimJSON(in string) []byte { return bytes.Trim([]byte(in), " \t\r\n") }

const minColumnarRows = 5

type jsonCompactor struct {
	in       []byte
	buf      *bytes.Buffer
	columnar bool
	dup      bool
}

func (c *jsonCompactor) ws(i int) int {
	for i < len(c.in) && (c.in[i] == ' ' || c.in[i] == '\t' || c.in[i] == '\n' || c.in[i] == '\r') {
		i++
	}
	return i
}

// str returns the end (after the closing quote) of the string that starts at i.
func (c *jsonCompactor) str(i int) int {
	for i++; c.in[i] != '"'; i++ {
		if c.in[i] == '\\' {
			i++
		}
	}
	return i + 1
}

// end returns the index after the value that starts at i, without writing.
func (c *jsonCompactor) end(i int) int {
	switch c.in[i] {
	case '"':
		return c.str(i)
	case '{', '[':
		for depth := 0; ; i++ {
			switch c.in[i] {
			case '"':
				i = c.str(i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
	}
	for i < len(c.in) && !bytes.ContainsRune([]byte(",]} \t\r\n"), rune(c.in[i])) {
		i++
	}
	return i
}

// members calls f(key, valueStart) for each member of the object at i and returns the index after it.
func (c *jsonCompactor) members(i int, f func(key []byte, v int)) int {
	i = c.ws(i + 1)
	for c.in[i] != '}' {
		k := i
		i = c.str(i)
		key := c.in[k:i]
		i = c.ws(i) + 1 // the colon
		i = c.ws(i)
		f(key, i)
		i = c.ws(c.end(i))
		if c.in[i] == ',' {
			i = c.ws(i + 1)
		}
	}
	return i + 1
}

// value writes the value at i, compacted, and returns the index after it.
func (c *jsonCompactor) value(i int) int {
	switch c.in[i] {
	case '{':
		c.buf.WriteByte('{')
		first := true
		var seen [][]byte
		next := c.members(i, func(key []byte, v int) {
			for _, s := range seen {
				if bytes.Equal(s, key) {
					c.dup = true
				}
			}
			seen = append(seen, key)
			if !first {
				c.buf.WriteByte(',')
			}
			first = false
			c.buf.Write(key)
			c.buf.WriteByte(':')
			c.value(v)
		})
		c.buf.WriteByte('}')
		return next
	case '[':
		if keys, ends := c.table(i); keys != nil {
			c.writeTable(i, keys)
			return ends
		}
		c.buf.WriteByte('[')
		j := c.ws(i + 1)
		for first := true; c.in[j] != ']'; first = false {
			if !first {
				c.buf.WriteByte(',')
			}
			j = c.ws(c.value(j))
			if c.in[j] == ',' {
				j = c.ws(j + 1)
			}
		}
		c.buf.WriteByte(']')
		return j + 1
	}
	e := c.end(i)
	c.buf.Write(c.in[i:e])
	return e
}

// table reports the keys of the array at i when it is a columnar candidate: at least minColumnarRows
// elements, all objects with the same keys in the same order, none duplicated. ends is the index after the array.
func (c *jsonCompactor) table(i int) (keys [][]byte, ends int) {
	if !c.columnar {
		return nil, 0
	}
	rows := 0
	j := c.ws(i + 1)
	for ; c.in[j] != ']'; rows++ {
		if c.in[j] != '{' {
			return nil, 0
		}
		n := 0
		same := true
		var dup bool
		c.members(j, func(key []byte, _ int) {
			if rows == 0 {
				for _, k := range keys {
					dup = dup || bytes.Equal(k, key)
				}
				keys = append(keys, key)
			} else if n >= len(keys) || !bytes.Equal(keys[n], key) {
				same = false
			}
			n++
		})
		if !same || dup || n != len(keys) || n == 0 {
			return nil, 0
		}
		j = c.ws(c.end(j))
		if c.in[j] == ',' {
			j = c.ws(j + 1)
		}
	}
	if rows < minColumnarRows {
		return nil, 0
	}
	return keys, j + 1
}

func (c *jsonCompactor) writeTable(i int, keys [][]byte) {
	c.buf.WriteString(`{"columns":[`)
	for n, k := range keys {
		if n > 0 {
			c.buf.WriteByte(',')
		}
		c.buf.Write(k)
	}
	c.buf.WriteString(`],"rows":[`)
	j := c.ws(i + 1)
	for first := true; c.in[j] != ']'; first = false {
		if !first {
			c.buf.WriteByte(',')
		}
		c.buf.WriteByte('[')
		n := 0
		end := c.members(j, func(_ []byte, v int) {
			if n > 0 {
				c.buf.WriteByte(',')
			}
			n++
			c.value(v)
		})
		c.buf.WriteByte(']')
		j = c.ws(end)
		if c.in[j] == ',' {
			j = c.ws(j + 1)
		}
	}
	c.buf.WriteString(`]}`)
}
