package guard

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// maxExpandExponent is the largest exponent expandExponent writes out. A larger one would make a long text for a
// short number.
const maxExpandExponent = 64

// hasLongNumber reports whether raw may hold a number that a float64 cannot keep exactly: a run of 16 or more digits,
// where "." does not end the run (so 4.111111111111111e15 counts). It also fires on digits inside strings; that only
// sends the result to the exact (slower) decoder.
func hasLongNumber(raw []byte) bool {
	run := 0
	for _, c := range raw {
		switch {
		case c >= '0' && c <= '9':
			if run++; run >= 16 {
				return true
			}
		case c != '.':
			run = 0
		}
	}
	return false
}

// decodeResult decodes a result object. Numbers are float64 unless raw may hold a long number: then every number is a
// json.Number, which keeps the digits the server wrote (issue 360) and is written back as it was. A Decoder holds a
// second copy of raw, which the guard's memory limit (TestMaskResultPeakMemory) does not allow for every result, so
// only the rare result with a long number pays for it.
func decodeResult(raw []byte) (result map[string]any, err error) {
	if !hasLongNumber(raw) {
		return result, json.Unmarshal(raw, &result)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&result); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF { // json.Unmarshal refuses text after the value; so do we
		return nil, json.Unmarshal(raw, &result) // the same error as the other path
	}
	return result, nil
}

// expandExponent writes a number with an exponent (4.111111111111111e15) as plain digits (4111111111111111), as a
// float64 would have been printed, so the masker sees the digits that a reader of the number sees. A number without
// an exponent, or with an exponent beyond maxExpandExponent, is returned as it is.
func expandExponent(lit string) string {
	i := strings.IndexAny(lit, "eE")
	if i < 0 {
		return lit
	}
	exp, err := strconv.Atoi(lit[i+1:])
	if err != nil || exp > maxExpandExponent || exp < -maxExpandExponent {
		return lit
	}
	mant, sign := lit[:i], ""
	if mant[0] == '-' {
		sign, mant = "-", mant[1:]
	}
	whole, frac, _ := strings.Cut(mant, ".")
	digits := whole + frac
	point := len(whole) + exp // where the decimal point stands in digits
	trimmed := strings.TrimLeft(digits, "0")
	point -= len(digits) - len(trimmed)
	digits = trimmed
	switch {
	case digits == "":
		return "0"
	case point <= 0:
		return sign + "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		return sign + digits + strings.Repeat("0", point-len(digits))
	}
	return sign + digits[:point] + "." + digits[point:]
}
