package guard

import (
	"encoding/csv"
	"reflect"
	"strings"
	"testing"
)

// readAll parses a whole CSV or TSV text.
func readAll(in string, comma rune) ([][]string, error) {
	r := csv.NewReader(strings.NewReader(in))
	r.Comma = comma
	return r.ReadAll()
}

func TestCompressTableCSVKeepsEveryCell(t *testing.T) {
	in := csvExport(5000)
	out, ok := compressTable(in)
	if !ok {
		t.Fatal("not detected")
	}
	a, _ := readAll(in, ',')
	b, err := readAll(out, ',')
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("cells changed (err %v)", err)
	}
	if !strings.Contains(out, "hello, world\nsecond line") {
		t.Error("quoted comma and newline lost")
	}
}

func TestCompressTableCSVDropsBlankLines(t *testing.T) {
	in := "a,b\n\n1,2\n\n\n3,4\n5,6\n7,8\n"
	out, ok := compressTable(in)
	if !ok || out != "a,b\n1,2\n3,4\n5,6\n7,8\n" {
		t.Fatalf("got %q %v", out, ok)
	}
}

func TestCompressTableMarkdownKeepsCellsDropsPadding(t *testing.T) {
	in := markdownTable(100)
	out, ok := compressTable(in)
	if !ok {
		t.Fatal("not detected")
	}
	if r := float64(len(out)) / float64(len(in)); r > 0.65 {
		t.Errorf("ratio %.2f, want at most 0.65", r)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 101 || lines[0] != "ID|Name|Status" || lines[100] != "99|service-99|healthy" {
		t.Errorf("unexpected rows: %d %q ... %q", len(lines), lines[0], lines[len(lines)-1])
	}
	for _, l := range lines {
		if strings.Contains(l, "---") {
			t.Errorf("separator row kept: %q", l)
		}
	}
}

func TestCompressTableEscapedPipeStaysInItsCell(t *testing.T) {
	in := "| a | b |\n|---|---|\n| x \\| y | 1 |\n| p | 2 |\n| q | 3 |\n| r | 4 |\n"
	out, ok := compressTable(in)
	if !ok || !strings.Contains(out, "x \\| y|1") {
		t.Fatalf("got %q %v", out, ok)
	}
}

func TestCompressTableCSVKeepsSpacesInCells(t *testing.T) {
	in := "a,b\n1,\" x\"\n2,y \n3,\"z\"\n4,\" \"\n5,w\n"
	out, ok := compressTable(in)
	if !ok {
		t.Fatal("not detected")
	}
	a, _ := readAll(in, ',')
	b, err := readAll(out, ',')
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("cells changed: %q -> %q (%v)", a, b, err)
	}
}

func TestCompressTablePipeCellTextIsNeverChanged(t *testing.T) {
	in := "| a | b |\n|---|---|\n| x\\\\|y | 1 |\n| p | 2 |\n| q | 3 |\n| r | 4 |\n"
	out, ok := compressTable(in)
	// Either the row is left as it is (a ragged row makes the text unknown) or the cell text is unchanged.
	if ok && !strings.Contains(out, "x\\\\|y|1") {
		t.Fatalf("got %q", out)
	}
}

func TestCompressTableRaggedIsUnknown(t *testing.T) {
	in := "a,b,c\n1,2,3\n4,5\n6,7,8\n9,10,11\n12,13,14\n"
	if _, ok := compressTable(in); ok {
		t.Error("ragged CSV must not be compressed")
	}
}

func FuzzCompressTable(f *testing.F) {
	f.Add(csvExport(6))
	f.Add(markdownTable(6))
	f.Fuzz(func(t *testing.T, in string) {
		out, ok := compressTable(in)
		if !ok {
			return
		}
		if len(out) > len(in)+len(strings.Split(in, "\n")) { // quoting may add a little; never a lot
			t.Fatalf("grew: %d -> %d", len(in), len(out))
		}
	})
}
