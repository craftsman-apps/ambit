package guard

import "testing"

// Each input must come out exactly as want: what must be masked is masked, and the text around it is kept.
func checkMasks(t *testing.T, cases map[string]string) {
	t.Helper()
	for in, want := range cases {
		if got, _, _ := (Builtin{}).Mask(t.Context(), in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

// Issue 295: a short number next to a card number (a date or time in a log line, an id column, an expiry date,
// a CVV) made the card mask check the longer number, which fails the Luhn check, and the card was left unmasked.
func TestBuiltinMasksCardNextToShortNumbers(t *testing.T) {
	checkMasks(t, map[string]string{
		"2026-11-17 10:00:01 4111 1111 1111 1111 declined": "2026-11-17 10:00:01 <CARD_NUMBER> declined",
		"2026-11-17 4111111111111111":                      "2026-11-17 <CARD_NUMBER>",
		"since 2026 4111 1111 1111 1111":                   "since 2026 <CARD_NUMBER>",
		"4111 1111 1111 1111 12/27":                        "<CARD_NUMBER> 12/27",
		"card 4111 1111 1111 1111 123":                     "card <CARD_NUMBER> 123",
		"1 4111111111111111 Kim\n2 5500000000000004 Lee":   "1 <CARD_NUMBER> Kim\n2 <CARD_NUMBER> Lee",
		"4111111111111111 2 items":                         "<CARD_NUMBER> 2 items",
		"2026-11-17-4111-1111-1111-1111":                   "2026-11-17-<CARD_NUMBER>",
		"1,4111111111111111,Kim":                           "1,<CARD_NUMBER>,Kim", // worked before; must still work
	})
}

// Issue 296: other common written forms of the same identifiers.
func TestBuiltinMasksOtherWrittenForms(t *testing.T) {
	checkMasks(t, map[string]string{
		// Korean resident registration numbers: foreign residents (7th digit 5 to 8); without the dash or with a
		// space when a word such as RRN or 주민등록번호 stands just before.
		"외국인등록번호 900101-5234567":                       "외국인등록번호 <NATIONAL_ID>",
		"RRN 9001011234567":                            "RRN <NATIONAL_ID>",
		"RRN 900101 1234567":                           "RRN <NATIONAL_ID>",
		`{"주민등록번호": "9001011234567"}`:                  `{"주민등록번호": "<NATIONAL_ID>"}`,
		`{"resident_registration_no":"9001011234567"}`: `{"resident_registration_no":"<NATIONAL_ID>"}`,
		// US Social Security numbers without dashes or with spaces, with a word such as SSN just before.
		"SSN 078051120":                     "SSN <NATIONAL_ID>",
		"SSN 078 05 1120":                   "SSN <NATIONAL_ID>",
		`{"ssn":"078051120"}`:               `{"ssn":"<NATIONAL_ID>"}`,
		"Social Security number: 078051120": "Social Security number: <NATIONAL_ID>",
		// Card numbers with dots, with runs of spaces, in full-width digits, and Amex groups.
		"card 4111.1111.1111.1111":    "card <CARD_NUMBER>",
		"card 4111  1111  1111  1111": "card <CARD_NUMBER>",
		"カード ４１１１１１１１１１１１１１１１":        "カード <CARD_NUMBER>",
		"カード ４１１１ １１１１ １１１１ １１１１":     "カード <CARD_NUMBER>",
		"amex 3782 822463 10005":      "amex <CARD_NUMBER>",
		// Email addresses URL-encoded in a link.
		"https://crm.example/search?q=kim%40acme.com": "https://crm.example/search?q=<EMAIL>",
		"mailto:KIM%40ACME.COM":                       "mailto:<EMAIL>",
	})
}

// The wider masks must leave ordinary data alone. The numbers marked Luhn pass the card check digit, so only the
// layout rules keep them unmasked.
func TestBuiltinMasksLeaveOrdinaryData(t *testing.T) {
	luhnValid := []string{"1700000000.123456", "1.2.3.4.5.6.7.8.9.10.11.16", "3.141592653587", "2026-11-17-1234567891", "1763372401127"}
	for _, s := range luhnValid {
		if !luhn(digitsOf(s)) {
			t.Fatalf("test data: %q should pass the Luhn check", s)
		}
	}
	same := []string{
		"2026-11-17 10:00:01 INFO request 42 took 1700 ms", // date, time
		"2026.11.17 23:59:59.999999",                       // dotted date, fraction of a second
		"created 1700000000.123456",                        // Unix time with microseconds (Luhn)
		`{"createdAt": "1763372401127"}`,                   // Unix time in milliseconds (Luhn)
		"pi is 3.141592653587",                             // a decimal number (Luhn)
		"version 1.2.3.4.5.6.7.8.9.10.11.16",               // a long version string (Luhn)
		"build 2026-11-17-1234567891",                      // a date glued to a build number (Luhn)
		"hosts 10.0.0.1 192.168.100.207 172.16.254.1",      // IP addresses
		"order 1234567890123 shipped",                      // an order number
		"order 123456789, invoice 078051120",               // 9-digit numbers without an SSN label
		"id 9001011234567 and 900101 1234567",              // RRN-like numbers without a label
		"RRN 9013011234567",                                // labelled, but month 13 is no date
		"SSN 000051120 and ssn 666 05 1120",                // labelled, but not a valid SSN
		"call 010-1234-5678 or 010-9876-5432",              // phone numbers
		"at 37.5665350 126.9779690",                        // coordinates
		"go1.27.2 darwin/arm64",                            // a version
		"price 1,234,567.89 KRW",                           // an amount
	}
	cases := map[string]string{}
	for _, s := range same {
		cases[s] = s
	}
	checkMasks(t, cases)
}

// Masks keep counting one finding per value, also when a card number has a short number on each side.
func TestBuiltinMaskCountsOncePerValue(t *testing.T) {
	_, n, _ := Builtin{}.Mask(t.Context(), "2026-11-17 4111 1111 1111 1111 123, RRN 9001011234567, kim%40acme.com")
	if n != 3 {
		t.Fatalf("found %d, want 3", n)
	}
}

func digitsOf(s string) (d []int) {
	for _, c := range s {
		if c >= '0' && c <= '9' {
			d = append(d, int(c-'0'))
		}
	}
	return d
}
