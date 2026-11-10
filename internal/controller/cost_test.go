package controller

import (
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// evalCost runs the cost expression the way agentgateway does: llm.* are unsigned token counts.
func evalCost(t *testing.T, prices map[string]v1.ModelPrice, model string, in, out uint64) uint64 {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("llm", cel.MapType(cel.StringType, cel.DynType)), ext.Math())
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(costCEL(prices))
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := prg.Eval(map[string]any{"llm": map[string]any{"inputTokens": in, "outputTokens": out, "requestModel": model}})
	if err != nil {
		t.Fatalf("%s: %v", costCEL(prices), err)
	}
	u, ok := v.Value().(uint64)
	if !ok {
		t.Fatalf("cost is %T, want uint", v.Value())
	}
	return u
}

// One unit is $0.00001. The cost is exact and rounded up: floating point once made 2 x 0.1 + 7 x 0.4 =
// 3.0000000000000004 and charged 4 units.
func TestCostIsExactAndRoundsUp(t *testing.T) {
	p := map[string]v1.ModelPrice{
		"mock/echo": {InputPerMillionTokens: "1.00", OutputPerMillionTokens: "4.00"},       // 0.1 and 0.4 units a token
		"tiny":      {InputPerMillionTokens: "0.000001", OutputPerMillionTokens: "0"},      // 0.0000001 units a token
		"big":       {InputPerMillionTokens: "999999.999999", OutputPerMillionTokens: "0"}, // the CRD's largest price
		"free":      {InputPerMillionTokens: "0", OutputPerMillionTokens: "0"},
	}
	for _, c := range []struct {
		model   string
		in, out uint64
		want    uint64
	}{
		{"mock/echo", 2, 7, 3},     // 0.2 + 2.8 = 3 exactly, not 4
		{"mock/echo", 4, 24, 10},   // 0.4 + 9.6 = 10 exactly, not 11
		{"mock/echo", 7, 1, 2},     // 0.7 + 0.4 = 1.1 -> 2
		{"mock/echo", 20, 5, 4},    // 2 + 2 = 4 exactly
		{"mock/echo", 0, 0, 0},     // nothing used, nothing charged
		{"tiny", 1, 0, 1},          // any positive cost is at least one unit
		{"tiny", 10_000_000, 0, 1}, // exactly 1
		{"tiny", 10_000_001, 0, 2}, // just over 1
		{"free", 1_000_000, 1_000_000, 0},
		{"big", 1_000_000, 0, 100_000_000_000}, // $999,999.999999 = 99,999,999,999.9 units, rounded up
	} {
		if got := evalCost(t, p, c.model, c.in, c.out); got != c.want {
			t.Errorf("%s in=%d out=%d: cost %d units, want %d", c.model, c.in, c.out, got, c.want)
		}
	}
}
