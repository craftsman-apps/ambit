// Package pricing is the one place that decides a model's price and converts dollars to the spend units
// that envoy ratelimit counts ($0.00001; its limits are uint32, so a cap is at most $42,949.67).
package pricing

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

const UnitsPerDollar = 100000

// ByModel maps the model name clients send to its price. A name that several Models share with different
// prices, or where one of them has no price, has no price: it is never charged from the wrong Model.
func ByModel(models []v1.Model) map[string]v1.ModelPrice {
	byName := map[string][]*v1.ModelPrice{}
	for _, m := range models {
		byName[m.Spec.Model] = append(byName[m.Spec.Model], m.Spec.Price)
	}
	out := map[string]v1.ModelPrice{}
	for name, ps := range byName {
		same := ps[0] != nil
		for _, p := range ps[1:] {
			same = same && p != nil && *p == *ps[0]
		}
		if same {
			out[name] = *ps[0]
		}
	}
	return out
}

// Units converts a dollar amount ("50", "0.00005") to whole units, exactly.
var dollarRE = regexp.MustCompile(`^[0-9]{1,5}(\.[0-9]{1,5})?$`) // the CRD's pattern

const maxUnits = 4_294_967_000 // $42,949.67

func Units(dollars string) (uint32, error) {
	r, ok := new(big.Rat).SetString(dollars)
	if !dollarRE.MatchString(dollars) || !ok || r.Sign() <= 0 {
		return 0, fmt.Errorf("%q is not a positive dollar amount", dollars)
	}
	r.Mul(r, big.NewRat(UnitsPerDollar, 1))
	if !r.IsInt() {
		return 0, fmt.Errorf("%q has more than 5 decimals", dollars)
	}
	if r.Num().Cmp(big.NewInt(maxUnits)) > 0 {
		return 0, fmt.Errorf("%q is above the largest cap, 42949.67", dollars)
	}
	return uint32(r.Num().Uint64()), nil
}

// PerTokenUnits converts a price in dollars per million tokens to units per token, as a decimal string for
// a CEL double literal: "1.00" -> "0.1".
func PerTokenUnits(perMillion string) (string, error) {
	r, ok := new(big.Rat).SetString(perMillion)
	if !ok || strings.ContainsAny(perMillion, "eE/") || r.Sign() < 0 {
		return "", fmt.Errorf("price %q is not a non-negative decimal", perMillion)
	}
	r.Mul(r, big.NewRat(UnitsPerDollar, 1_000_000))
	s := r.FloatString(8)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" {
		s = "0"
	}
	return s, nil
}
