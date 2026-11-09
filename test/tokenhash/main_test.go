package main

import (
	"strings"
	"testing"
)

func TestDescribeHashesAndNamesTheUser(t *testing.T) {
	// header.payload.signature with payload {"preferred_username":"alice"}; the signature is not checked here.
	jwtLike := "eyJhbGciOiJSUzI1NiJ9.eyJwcmVmZXJyZWRfdXNlcm5hbWUiOiJhbGljZSJ9.c2ln"
	a, b := describe(jwtLike), describe("opaque-token")
	if !strings.HasSuffix(a, " user=alice") || !strings.HasSuffix(b, " user=-") {
		t.Fatalf("a=%q b=%q", a, b)
	}
	if strings.Split(a, " ")[0] == strings.Split(b, " ")[0] || strings.Split(a, " ")[0] != strings.Split(describe(jwtLike), " ")[0] {
		t.Fatalf("hashes must differ between tokens and stay the same for one token: %q %q", a, b)
	}
	if strings.Contains(a, jwtLike) {
		t.Fatal("the token itself must never be returned")
	}
}

func TestConfigReportsHashOfSecretNotValue(t *testing.T) {
	t.Setenv("TOKENHASH_LABEL", "lbl")
	t.Setenv("TOKENHASH_SECRET", "hunter2-test-value")
	got := config("sfx")
	if !strings.HasPrefix(got, " label=lbl secret=sha256") && !strings.Contains(got, " label=lbl secret=") || !strings.HasSuffix(got, " suffix=sfx") {
		t.Fatal(got)
	}
	if strings.Contains(got, "hunter2") {
		t.Fatal("the secret value must never be returned")
	}
}
