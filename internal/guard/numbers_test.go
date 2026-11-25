package guard

import (
	"strconv"
	"strings"
	"testing"
)

// luhnCard returns prefix plus the one check digit that makes it pass the Luhn check.
func luhnCard(t *testing.T, prefix string) string {
	t.Helper()
	for d := range 10 {
		s := prefix + strconv.Itoa(d)
		digits := make([]int, len(s))
		for i, c := range s {
			digits[i] = int(c - '0')
		}
		if luhn(digits) {
			return s
		}
	}
	t.Fatal("no check digit")
	return ""
}

// Issue 360: a card number written as a JSON number is checked from the digits the server wrote, not from a rounded
// float. Without that, a 19-digit card number (more than 2^53) was rounded, failed the Luhn check, and passed.
func TestLargeNumbersAreMaskedFromTheirExactText(t *testing.T) {
	card19 := luhnCard(t, "411111111111111111") // 19 digits
	card17 := luhnCard(t, "4111111111111111")   // 17 digits
	for _, c := range []string{card19, card17} {
		f, _ := strconv.ParseFloat(c, 64)
		if strconv.FormatFloat(f, 'f', -1, 64) == c {
			t.Fatalf("%s is exact as a float, so the test proves nothing", c)
		}
	}
	g := &Guard{Pins: pins, Masker: Builtin{}}
	for name, number := range map[string]string{
		"19 digits":                card19,
		"17 digits":                card17,
		"negative":                 "-" + card19,
		"decimal point":            card19 + ".0",
		"decimal point and zeros":  card19 + ".000",
		"decimals after the point": "0." + card19,
		"exponent, lower case":     card19[:1] + "." + card19[1:] + "e18",
		"exponent, upper case":     card19[:1] + "." + card19[1:] + "E+18",
		"exponent, no point":       card19 + "e0",
		"exponent, negative":       card19 + "e-0",
		"exponent with a point":    card19[:5] + "." + card19[5:] + "e14",
	} {
		for _, body := range []string{
			`{"content":[],"structuredContent":{"card":` + number + `}}`,
			`{"content":[],"structuredContent":{"rows":[{"a":1},{"card":` + number + `,"b":2.5}]}}`,
		} {
			got, withheld := respond(t, g, "tools/call", body)
			if withheld || !strings.Contains(got, "<CARD_NUMBER>") || strings.Contains(got, number) {
				t.Errorf("%s: card number not masked: %s", name, got)
			}
			if strings.Contains(got, "<CARD_NUMBER>.") { // the zeros of a fraction do not stay next to the placeholder
				t.Errorf("%s: fraction zeros kept after the placeholder: %s", name, got)
			}
		}
	}
}

// A result with a long number that the masker does not change keeps every number exactly as the server wrote it,
// also when another part of the result is masked and the result is written again.
func TestNumbersKeepTheirSpellingWhenAResultIsRewritten(t *testing.T) {
	g := &Guard{Pins: pins, Masker: Builtin{}}
	got, withheld := respond(t, g, "tools/call",
		`{"content":[{"type":"text","text":"kim@acme.example"}],"structuredContent":{"id":9007199254740993,"big":12345678901234567890,`+
			`"price":1.50,"tiny":0.10000000000000000001,"exp":1.5e3,"huge":1e400}}`)
	if withheld {
		t.Fatalf("withheld: %s", got)
	}
	for _, want := range []string{`"<EMAIL>"`, `"id":9007199254740993`, `"big":12345678901234567890`, `"price":1.50`,
		`"tiny":0.10000000000000000001`, `"exp":1.5e3`, `"huge":1e400`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in %s", want, got)
		}
	}
}

// Numbers of up to 15 digits take the fast path (floats); the card numbers of 16 digits and the written forms around
// them are masked as before.
func TestShortNumbersAreStillMasked(t *testing.T) {
	g := &Guard{Pins: pins, Masker: Builtin{}}
	for _, number := range []string{"4111111111111111", "4111111111111111.0", "4.111111111111111e15"} {
		got, _ := respond(t, g, "tools/call", `{"content":[],"structuredContent":{"card":`+number+`}}`)
		if !strings.Contains(got, "<CARD_NUMBER>") {
			t.Errorf("%s: %s", number, got)
		}
	}
}

// Text after the result object is refused on both paths: the one for ordinary numbers and the one for long numbers.
func TestResultWithTextAfterTheObjectIsWithheld(t *testing.T) {
	g := &Guard{Pins: pins, Masker: Builtin{}}
	for _, body := range []string{
		`{"content":[]} {"a":"kim@acme.example"}`,
		`{"content":[],"n":4111111111111111111} {"a":"kim@acme.example"}`,
		`{"content":[],"n":4111111111111111111}x`,
		`{"content":[],"n":4111111111111111111,}`,
		`{"content":[],"n":41111111111111111.11.1}`,
	} {
		if got, withheld := respond(t, g, "tools/call", body); !withheld || strings.Contains(got, "kim@") {
			t.Errorf("%s: want the result withheld, got %s", body, got)
		}
	}
}

func TestExpandExponent(t *testing.T) {
	for in, want := range map[string]string{
		"12.5":                 "12.5",
		"4111111111111111e0":   "4111111111111111",
		"4.111111111111111e15": "4111111111111111",
		"4.111E+3":             "4111",
		"4.1111e3":             "4111.1",
		"1.5e3":                "1500",
		"-1.5e3":               "-1500",
		"15e-1":                "1.5",
		"1.5e-3":               "0.0015",
		"0.5e1":                "5",
		"0e5":                  "0",
		"1e400":                "1e400", // too far to write out: kept as it is
		"1e-400":               "1e-400",
	} {
		if got := expandExponent(in); got != want {
			t.Errorf("expandExponent(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestNumberText(t *testing.T) {
	for in, want := range map[string]string{
		"4111111111111111.0":   "4111111111111111",
		"4111111111111111.000": "4111111111111111",
		"12.50":                "12.50", // other digits in the fraction: the zero stays
		"100":                  "100",   // no point: the zeros are digits
		"100.0":                "100",
		"-1.0":                 "-1",
		"0.0":                  "0",
		"0.10":                 "0.10",
		"1.50e400":             "1.50e400", // not written out: kept as it is
		"4.0e15":               "4000000000000000",
	} {
		if got := numberText(in); got != want {
			t.Errorf("numberText(%s) = %s, want %s", in, got, want)
		}
	}
}
