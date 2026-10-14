package connect

import "testing"

// The same cases as web/scripts/tool-setup.test.mjs ("isPublicHttps"), so the console and the CLI cannot drift.
func TestIsPublicHTTPS(t *testing.T) {
	for _, u := range []string{"https://llm.example.com", "https://llm.example.com:8443/x"} {
		if !IsPublicHTTPS(u) {
			t.Errorf("IsPublicHTTPS(%q) = false, want true", u)
		}
	}
	for _, u := range []string{"http://llm.example.com", "https://localhost", "https://127.0.0.1", "https://192.168.1.4", "https://172.20.0.1",
		"https://foo.localtest.me", "https://[::1]", "https://nodot", "", "x"} {
		if IsPublicHTTPS(u) {
			t.Errorf("IsPublicHTTPS(%q) = true, want false", u)
		}
	}
	// Edges of the private ranges and the suffix rule that the console test does not list.
	for u, want := range map[string]bool{
		"https://10.0.0.1": false, "https://0.1.2.3": false, "https://169.254.1.1": false,
		"https://172.15.0.1": true, "https://172.32.0.1": true, "https://8.8.8.8": true,
		"https://printer.local": false, "https://a.internal": false, "https://a.lan": false, "https://a.home.arpa": false,
		"https://notlocal.example.com": true, "https://LLM.Example.COM": true,
	} {
		if got := IsPublicHTTPS(u); got != want {
			t.Errorf("IsPublicHTTPS(%q) = %v, want %v", u, got, want)
		}
	}
}
