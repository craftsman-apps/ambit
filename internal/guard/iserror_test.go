package guard

import (
	"strings"
	"testing"
)

// Issue 333: a tool failure that a server reports as a result (isError true, as the MCP specification asks) is a result,
// so it is masked like any other. Only a JSON-RPC error never reaches the guard (docs/concepts.md, Masking).
func TestToolErrorResultIsMasked(t *testing.T) {
	g := &Guard{Pins: pins, Masker: Builtin{}}
	got, withheld := respond(t, g, "tools/call",
		`{"isError":true,"content":[{"type":"text","text":"lookup 42 failed for customer RRN 900101-1234567, email minsu.kim@example.com, card 4111 1111 1111 1111"}],`+
			`"structuredContent":{"error":"no customer with email minsu.kim@example.com"}}`)
	if withheld {
		t.Fatalf("withheld: %s", got)
	}
	for _, leak := range []string{"900101-1234567", "minsu.kim", "4111 1111 1111 1111"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q reached the client: %s", leak, got)
		}
	}
	for _, want := range []string{`"isError":true`, "<NATIONAL_ID>", "<EMAIL>", "<CARD_NUMBER>"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in %s", want, got)
		}
	}
}
