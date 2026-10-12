package routerconfig

import "testing"

func TestHashChangesWithAnySetting(t *testing.T) {
	a := []Router{{Name: "a", Wire: "auto", Type: "LLM", TimeoutMillis: 1500}}
	b := []Router{{Name: "a", Wire: "auto", Type: "LLM", TimeoutMillis: 1600}}
	if HashOf(a) == HashOf(b) || HashOf(a) != HashOf(append([]Router(nil), a...)) || len(HashOf(a)) != 64 {
		t.Fatal("hash: stable for equal input, different for any change, hex sha256")
	}
	if _, ok := (Config{Routers: a}).Find("auto"); !ok {
		t.Fatal("Find by wire name")
	}
}
