package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// mint signs the claims on standard input (a JSON object) with the private key in the file named by SIGNING_JWK and prints the token. TEST-ONLY:
// test/oauthtest/gateway.py uses it to make a token that has a valid signature and a claim that Ambit's own sign-in never writes (for example two
// audiences), so that a refusal is proven to come from the rule and not from a bad signature.
func mint() {
	b, err := os.ReadFile(os.Getenv("SIGNING_JWK"))
	if err != nil {
		log.Fatal("SIGNING_JWK: ", err)
	}
	var jwk jose.JSONWebKey
	if err := jwk.UnmarshalJSON(b); err != nil || jwk.IsPublic() {
		log.Fatal("SIGNING_JWK is not a private key")
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jwk}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", jwk.KeyID))
	if err != nil {
		log.Fatal(err)
	}
	in, _ := io.ReadAll(os.Stdin)
	var claims map[string]any
	if err := json.Unmarshal(in, &claims); err != nil {
		log.Fatal("claims: ", err)
	}
	tok, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(tok)
}
