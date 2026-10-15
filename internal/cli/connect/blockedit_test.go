package connect

import (
	"strings"
	"testing"
)

const begin = "# >>> ambit: written by `ambit connect codex`; `ambit disconnect codex` removes it >>>"

var body = []string{`model = "m"`, `model_provider = "ambit"`}

// Inserting a block and removing it again returns the original bytes, for every line-ending style.
func TestBlockRoundTripIsByteIdentical(t *testing.T) {
	cases := map[string]string{
		"empty":                "",
		"one line, no newline": "a = 1",
		"final newline":        "a = 1\n",
		"blank last line":      "a = 1\n\n",
		"CRLF":                 "a = 1\r\n[t]\r\nb = 2\r\n",
		"CRLF no final":        "a = 1\r\n[t]\r\nb = 2",
		"BOM":                  "\xef\xbb\xbfa = 1\n",
		"BOM and CRLF":         "\xef\xbb\xbfa = 1\r\n",
		"mixed endings":        "a = 1\r\nb = 2\n[t]\r\n",
		"only blank":           "\n",
	}
	for name, before := range cases {
		for _, at := range []string{"end", "start"} {
			t.Run(name+"/"+at, func(t *testing.T) {
				tx := ParseText([]byte(before))
				pos := len(tx.lines)
				if at == "start" {
					pos = 0
				}
				rec := tx.InsertBlock(pos, begin, body)
				got := string(tx.Bytes())
				if !strings.Contains(got, begin) || !strings.Contains(got, "# <<< ambit <<<") {
					t.Fatalf("markers missing:\n%q", got)
				}
				if strings.HasPrefix(before, "\xef\xbb\xbf") && !strings.HasPrefix(got, "\xef\xbb\xbf") {
					t.Fatal("BOM lost")
				}
				if strings.Contains(before, "\r\n") && strings.Count(got, "\n") != strings.Count(got, "\r\n")+strings.Count(before, "\n")-strings.Count(before, "\r\n") {
					t.Fatalf("inserted lines do not use CRLF: %q", got)
				}
				tx2 := ParseText([]byte(got))
				blocks, err := tx2.Blocks()
				if err != nil || len(blocks) != 1 {
					t.Fatalf("Blocks = %v, %v", blocks, err)
				}
				if tx2.BlockBody(blocks[0]) != strings.Join(body, "\n") {
					t.Fatalf("body = %q", tx2.BlockBody(blocks[0]))
				}
				tx2.RemoveBlock(blocks[0], rec)
				if string(tx2.Bytes()) != before {
					t.Fatalf("after remove:\n got  %q\n want %q", tx2.Bytes(), before)
				}
			})
		}
	}
}

func TestBlocksFindsSeveralAndKeepsTheRest(t *testing.T) {
	tx := ParseText([]byte("a = 1\n" + begin + "\nx\n# <<< ambit <<<\n[t]\n" + begin + "\ny\nz\n# <<< ambit <<<\n"))
	blocks, err := tx.Blocks()
	if err != nil || len(blocks) != 2 {
		t.Fatalf("Blocks = %v, %v", blocks, err)
	}
	if tx.BlockBody(blocks[0]) != "x" || tx.BlockBody(blocks[1]) != "y\nz" {
		t.Fatalf("bodies = %q, %q", tx.BlockBody(blocks[0]), tx.BlockBody(blocks[1]))
	}
}

func TestBlocksRefusesAnUnterminatedBlock(t *testing.T) {
	_, err := ParseText([]byte("a = 1\n" + begin + "\nx\n")).Blocks()
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want it to name line 2", err)
	}
	_, err = ParseText([]byte("# <<< ambit <<<\n")).Blocks()
	if err == nil {
		t.Fatal("an end marker without a begin marker must fail")
	}
}

func TestInsertBlockPadsWithABlankLineOnlyWhenAsked(t *testing.T) {
	tx := ParseText([]byte("[t]\nb = 2\n"))
	rec := tx.InsertBlockPadded(len(tx.lines), begin, body)
	if !strings.Contains(string(tx.Bytes()), "b = 2\n\n"+begin) {
		t.Fatalf("no blank line before the block: %q", tx.Bytes())
	}
	blocks, _ := tx.Blocks()
	tx.RemoveBlock(blocks[0], rec)
	if string(tx.Bytes()) != "[t]\nb = 2\n" {
		t.Fatalf("after remove: %q", tx.Bytes())
	}
}
