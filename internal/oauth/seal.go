package oauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Sealer encrypts and authenticates the small records that live in the person's browser (AES-256-GCM), and makes the
// consent form tokens (HMAC-SHA-256). Both keys come from one secret of at least 32 bytes, by separate labels, so a
// replica that restarts or a second replica reads what the first one wrote.
type Sealer struct {
	aead cipher.AEAD
	mac  []byte
}

// MinSecret is the shortest cookie secret that is accepted.
const MinSecret = 32

func derive(secret []byte, label string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("ambit-auth/" + label))
	return m.Sum(nil)
}

func NewSealer(secret []byte) (*Sealer, error) {
	if len(secret) < MinSecret {
		return nil, errors.New("the cookie secret must be at least 32 bytes")
	}
	block, err := aes.NewCipher(derive(secret, "seal"))
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: a, mac: derive(secret, "mac")}, nil
}

// Seal encrypts v as JSON; aad binds the record to its use (a record made for one cookie name never opens as another).
func (s *Sealer) Seal(v any, aad string) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	n := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(s.aead.Seal(n, n, b, []byte(aad))), nil
}

var errSealed = errors.New("the record is not valid")

func (s *Sealer) Open(sealed, aad string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || len(b) < s.aead.NonceSize() {
		return errSealed
	}
	p, err := s.aead.Open(nil, b[:s.aead.NonceSize()], b[s.aead.NonceSize():], []byte(aad))
	if err != nil || json.Unmarshal(p, v) != nil {
		return errSealed
	}
	return nil
}

// Token is an HMAC over the parts, for the consent form: bound to the browser's cookie and to the challenge.
func (s *Sealer) Token(parts ...string) string {
	m := hmac.New(sha256.New, s.mac)
	for _, p := range parts {
		m.Write([]byte{byte(len(p) >> 8), byte(len(p))})
		m.Write([]byte(p))
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Sealer) Equal(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// random returns n random bytes, URL-safe base64.
func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("no randomness: " + err.Error()) // the process cannot make a safe record; a restart is right
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Transaction cookies. One sign-in has its own cookie, named by a random handle that is also the start of the `state`
// sent to the company IdP. Two sign-ins in one browser (two platform tabs) therefore never overwrite each other.
const (
	txPrefix  = "__Host-ambit_tx_"
	maxTxLive = 6 // the browser keeps at most this many pending sign-ins; the oldest is dropped
	txMaxAge  = 600
)

// Tx is one pending sign-in.
type Tx struct {
	Login    string `json:"l"` // Hydra's login challenge
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"` // PKCE verifier toward the company IdP
	Exp      int64  `json:"e"`
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// startTx makes a transaction and its cookie; the returned transaction holds the state to send to the IdP. Cookies of sign-ins that
// expired are removed, and when more than maxTxLive remain the ones that expire first are removed too.
func (s *Sealer) startTx(w http.ResponseWriter, r *http.Request, t Tx, now time.Time) (Tx, error) {
	handle := random(12)
	t.State = handle + "." + random(24)
	t.Exp = now.Add(txMaxAge * time.Second).Unix()
	v, err := s.Seal(t, txPrefix+handle)
	if err != nil {
		return Tx{}, err
	}
	type live struct {
		name string
		exp  int64
	}
	var all []live
	for _, c := range r.Cookies() {
		if !strings.HasPrefix(c.Name, txPrefix) {
			continue
		}
		var old Tx
		if s.Open(c.Value, c.Name, &old) != nil || old.Exp <= now.Unix() {
			setCookie(w, c.Name, "", -1)
			continue
		}
		all = append(all, live{c.Name, old.Exp})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].exp < all[j].exp })
	for len(all) >= maxTxLive {
		setCookie(w, all[0].name, "", -1)
		all = all[1:]
	}
	setCookie(w, txPrefix+handle, v, txMaxAge)
	return t, nil
}

// takeTx finds the transaction of a state, removes its cookie (a transaction is used once, whatever happens next) and
// checks it. The error text is for the log; the person gets one sentence.
func (s *Sealer) takeTx(w http.ResponseWriter, r *http.Request, state string, now time.Time) (Tx, error) {
	handle, _, ok := strings.Cut(state, ".")
	if !ok || handle == "" || len(handle) > 64 {
		return Tx{}, errors.New("state has no transaction handle")
	}
	name := txPrefix + handle
	c, err := r.Cookie(name)
	if err != nil {
		return Tx{}, errors.New("no transaction cookie: expired, or started in another browser")
	}
	setCookie(w, name, "", -1)
	var t Tx
	if err := s.Open(c.Value, name, &t); err != nil {
		return Tx{}, errors.New("transaction cookie not valid")
	}
	if now.Unix() > t.Exp {
		return Tx{}, errors.New("transaction expired")
	}
	if !s.Equal(state, t.State) {
		return Tx{}, errors.New("state does not match")
	}
	return t, nil
}
