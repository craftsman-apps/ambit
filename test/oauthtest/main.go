// oauthtest is TEST-ONLY (test/stepoauth.sh). Two stand-ins:
//
//	oauthtest idp      the company's OpenID Connect provider: a login form (pick a user), the code flow with PKCE,
//	                   signed ID tokens, and users that misbehave on purpose (another issuer, a wrong nonce, ...)
//	oauthtest console  the console's internal OAuth API that ambit-auth calls (servers, agents, active), with a /_state
//	                   page that tells the test what ambit-auth asked for
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: oauthtest idp|console")
	}
	switch os.Args[1] {
	case "idp":
		idp()
	case "console":
		console()
	default:
		log.Fatal("unknown command")
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rnd() string { b := make([]byte, 18); _, _ = rand.Read(b); return b64(b) }

// ---- the identity provider

type user struct {
	Email, Sub string
	Verified   bool
	Groups     []string
	Iss        string // the issuer written into the ID token, when it is not ours
	Nonce      string // a nonce to use instead of the request's
	Aud        string
	Expired    bool
	CbIss      string // an iss parameter on the redirect to the callback
}

var users = map[string]user{
	"alice":         {Email: "alice@acme.test", Sub: "u-alice", Verified: true, Groups: []string{"eng", "ml-research"}},
	"bob":           {Email: "bob@acme.test", Sub: "u-bob", Verified: true, Groups: []string{"eng"}},
	"carol":         {Email: "carol@acme.test", Sub: "u-carol", Verified: false},
	"svcfake":       {Email: "svcfake@svc.ambit.internal", Sub: "u-svc", Verified: true},
	"noemail":       {Sub: "u-noemail", Verified: true},
	"mallory-iss":   {Email: "alice@acme.test", Sub: "u-m1", Verified: true, Iss: "https://evil.example.com"},
	"mallory-nonce": {Email: "alice@acme.test", Sub: "u-m2", Verified: true, Nonce: "forged"},
	"mallory-aud":   {Email: "alice@acme.test", Sub: "u-m3", Verified: true, Aud: "another-client"},
	"mallory-exp":   {Email: "alice@acme.test", Sub: "u-m4", Verified: true, Expired: true},
	"mallory-cbiss": {Email: "alice@acme.test", Sub: "u-m5", Verified: true, CbIss: "https://evil.example.com"},
}

type code struct {
	user, nonce, challenge, redirect string
	exp                              time.Time
}

func idp() {
	issuer, clientID, secret, redirect := os.Getenv("ISSUER"), os.Getenv("CLIENT_ID"), os.Getenv("CLIENT_SECRET"), os.Getenv("REDIRECT_URI")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	kid := rnd()
	var mu sync.Mutex
	codes := map[string]code{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
			"jwks_uri": issuer + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		n := b64(key.N.Bytes())
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid, "n": n, "e": "AQAB"}}})
	})
	// The login form: the test picks a user, as a person would type a name.
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != clientID || q.Get("redirect_uri") != redirect || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
			http.Error(w, "bad authorization request", 400)
			return
		}
		var names []string
		for n := range users {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(w, `<!doctype html><form method="post" action="/authorize">`)
		for _, k := range []string{"state", "nonce", "code_challenge", "redirect_uri"} {
			fmt.Fprintf(w, `<input type="hidden" name="%s" value="%s">`, k, html.EscapeString(q.Get(k)))
		}
		for _, n := range names {
			fmt.Fprintf(w, `<label><input type="radio" name="user" value="%s">%s</label>`, n, n)
		}
		fmt.Fprint(w, `<button name="action" value="login">Sign in</button><button name="action" value="deny">Deny</button></form>`)
	})
	mux.HandleFunc("POST /authorize", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f := r.PostForm
		to, _ := url.Parse(f.Get("redirect_uri"))
		if f.Get("redirect_uri") != redirect {
			http.Error(w, "bad redirect", 400)
			return
		}
		q := to.Query()
		q.Set("state", f.Get("state"))
		u, ok := users[f.Get("user")]
		if f.Get("action") == "deny" || !ok {
			q.Set("error", "access_denied")
		} else {
			c := rnd()
			mu.Lock()
			codes[c] = code{user: f.Get("user"), nonce: f.Get("nonce"), challenge: f.Get("code_challenge"), redirect: redirect, exp: time.Now().Add(time.Minute)}
			mu.Unlock()
			q.Set("code", c)
		}
		if u.CbIss != "" {
			q.Set("iss", u.CbIss)
		} else {
			q.Set("iss", issuer)
		}
		to.RawQuery = q.Encode()
		http.Redirect(w, r, to.String(), 302)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		id, sec, ok := r.BasicAuth()
		if !ok || id != clientID || subtle.ConstantTimeCompare([]byte(sec), []byte(secret)) != 1 {
			http.Error(w, `{"error":"invalid_client"}`, 401)
			return
		}
		_ = r.ParseForm()
		mu.Lock()
		c, ok := codes[r.PostForm.Get("code")]
		delete(codes, r.PostForm.Get("code")) // a code is used once
		mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || time.Now().After(c.exp) || r.PostForm.Get("redirect_uri") != c.redirect || b64(sum[:]) != c.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		u := users[c.user]
		claims := map[string]any{"iss": issuer, "sub": u.Sub, "aud": clientID, "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
			"nonce": c.nonce, "email_verified": u.Verified, "groups": u.Groups}
		if u.Email != "" {
			claims["email"] = u.Email
		}
		if u.Iss != "" {
			claims["iss"] = u.Iss
		}
		if u.Nonce != "" {
			claims["nonce"] = u.Nonce
		}
		if u.Aud != "" {
			claims["aud"] = u.Aud
		}
		if u.Expired {
			claims["exp"] = time.Now().Add(-time.Hour).Unix()
		}
		head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
		body, _ := json.Marshal(claims)
		signing := b64(head) + "." + b64(body)
		h := sha256.Sum256([]byte(signing))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": rnd(), "token_type": "Bearer", "expires_in": 300, "id_token": signing + "." + b64(sig)})
	})
	serveTLS(":8443", mux)
}

func serveTLS(addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServeTLS("/tls/tls.crt", "/tls/tls.key"))
}

// ---- the console's internal OAuth API

func console() {
	var mu sync.Mutex
	servers := map[string][]map[string]string{}
	if v := os.Getenv("SERVERS"); v != "" {
		if err := json.Unmarshal([]byte(v), &servers); err != nil {
			log.Fatal(err)
		}
	}
	inactive := map[string]bool{}
	var created []map[string]any
	var auds []string
	refuse := ""
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if t == "" || t == r.Header.Get("Authorization") {
			http.Error(w, "no token", 401)
			return false
		}
		parts := strings.Split(t, ".")
		if len(parts) == 3 { // the projected token: record its audience (the test checks it is ours alone)
			if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
				var c struct {
					Aud []string `json:"aud"`
				}
				_ = json.Unmarshal(b, &c)
				mu.Lock()
				auds = c.Aud
				mu.Unlock()
			}
		}
		return true
	}
	mux.HandleFunc("POST /internal/oauth/v1/servers", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var in struct {
			Email string `json:"email"`
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(b, &in)
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"servers": servers[in.Email]})
	})
	mux.HandleFunc("POST /internal/oauth/v1/active", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var in struct{ Agent, Grant string }
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(b, &in)
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"active": !inactive[in.Agent]})
	})
	mux.HandleFunc("POST /_inactive", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		inactive[r.URL.Query().Get("agent")] = r.URL.Query().Get("on") == "true"
	})
	mux.HandleFunc("POST /internal/oauth/v1/agents", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var in map[string]any
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(b, &in)
		mu.Lock()
		defer mu.Unlock()
		if refuse != "" {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": refuse})
			return
		}
		created = append(created, in)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": fmt.Sprintf("%v-%d", in["platform"], len(created))})
	})
	// test controls
	mux.HandleFunc("GET /_state", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"created": created, "tokenAudience": auds})
	})
	mux.HandleFunc("POST /_servers", func(w http.ResponseWriter, r *http.Request) {
		var v map[string][]map[string]string
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if json.Unmarshal(b, &v) != nil {
			http.Error(w, "bad", 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		servers = v
	})
	mux.HandleFunc("POST /_refuse", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		refuse = r.URL.Query().Get("msg")
	})
	log.Fatal((&http.Server{Addr: ":8085", Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}
