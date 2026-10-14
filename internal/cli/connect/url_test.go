package connect

import "testing"

func TestNormalizeURL(t *testing.T) {
	ok := map[string]string{
		"https://llm.example.com":         "https://llm.example.com",
		"HTTPS://LLM.Example.COM/":        "https://llm.example.com",
		"https://llm.example.com/v1":      "https://llm.example.com",
		"https://llm.example.com/v1/":     "https://llm.example.com",
		"http://llm.localtest.me:8080":    "http://llm.localtest.me:8080",
		"http://llm.localtest.me:8080/v1": "http://llm.localtest.me:8080",
		"https://example.com/gateway/v1":  "https://example.com/gateway",
		"https://example.com/Gateway":     "https://example.com/Gateway", // the path keeps its case
		"  https://llm.example.com  ":     "https://llm.example.com",
		"https://llm.example.com:443":     "https://llm.example.com:443",
	}
	for in, want := range ok {
		if got, err := NormalizeURL(in); err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "llm.example.com", "ftp://llm.example.com", "https://", "https://llm.example.com?x=1", "https://llm.example.com#f", "https://user:pw@llm.example.com", "ht tp://x"} {
		if got, err := NormalizeURL(in); err == nil {
			t.Errorf("NormalizeURL(%q) = %q, want an error", in, got)
		}
	}
}
