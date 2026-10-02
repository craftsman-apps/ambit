package pricing

import (
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

func TestByModel(t *testing.T) {
	price := func(in string) *v1.ModelPrice {
		return &v1.ModelPrice{InputPerMillionTokens: in, OutputPerMillionTokens: "1"}
	}
	model := func(name string, p *v1.ModelPrice) v1.Model {
		return v1.Model{Spec: v1.ModelSpec{Model: name, Price: p}}
	}
	got := ByModel([]v1.Model{
		model("gpt-small", price("0.15")),
		model("llama", price("0.10")), model("llama", price("0.20")),
		model("qwen", price("0.30")), model("qwen", nil),
		model("mistral", price("0.40")), model("mistral", price("0.40")),
		model("local", nil),
		model("free", price("0")),
	})
	if len(got) != 3 || got["gpt-small"].InputPerMillionTokens != "0.15" || got["mistral"].InputPerMillionTokens != "0.40" || got["free"].InputPerMillionTokens != "0" {
		t.Fatalf("%+v", got)
	}
}

func TestUnits(t *testing.T) {
	for in, want := range map[string]uint32{"50": 5_000_000, "0.00001": 1, "0.00005": 5, "42949.67": 4_294_967_000, "1.5": 150_000} {
		if got, err := Units(in); err != nil || got != want {
			t.Errorf("%s: %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"0", "0.000001", "42949.68", "42949.67001", "100000", "-1", "1e3", ".5", "abc", ""} {
		if _, err := Units(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPerTokenUnits(t *testing.T) {
	for in, want := range map[string]string{"1.00": "0.1", "4": "0.4", "0.15": "0.015", "0": "0", "123456.654321": "12345.6654321"} {
		if got, err := PerTokenUnits(in); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := PerTokenUnits("x"); err == nil || !strings.Contains(err.Error(), "x") {
		t.Error("bad price accepted")
	}
}

// The error says what is wrong, so the console can tell the person.
func TestUnitsErrorsNameTheProblem(t *testing.T) {
	for in, want := range map[string]string{"1.000001": "decimals", "42949.67001": "largest", "1.0000001": "decimals", "100000": "largest", "42949.68": "largest", "0": "positive", "abc": "positive"} {
		if _, err := Units(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error about %q", in, err, want)
		}
	}
}

// A wildcard model name ("gpt-*") matches requests for other names, which the cost map cannot price: it
// counts as unpriced, so capped groups are refused rather than served for free.
func TestWildcardModelNamesAreUnpriced(t *testing.T) {
	p := &v1.ModelPrice{InputPerMillionTokens: "1", OutputPerMillionTokens: "1"}
	got := ByModel([]v1.Model{{Spec: v1.ModelSpec{Model: "gpt-*", Price: p}}, {Spec: v1.ModelSpec{Model: "gpt-4.1", Price: p}}})
	if _, ok := got["gpt-*"]; ok || len(got) != 1 {
		t.Fatalf("%v", got)
	}
}
