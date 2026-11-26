package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/craftsman-apps/ambit/internal/identity"
	"golang.org/x/oauth2"
)

// claim marks a Hydra login or consent challenge as decided, once. Hydra v26.2.0 lets a consent request that was rejected
// be read and even accepted again until its verifier is used, so "single use" is kept here, in the store the replicas
// share. false: it was decided before.
func (s *Server) claim(ctx context.Context, kind, challenge string) (bool, error) {
	h := sha256.Sum256([]byte(challenge))
	return s.Clients.Once(ctx, "challenge:"+kind+":"+hex.EncodeToString(h[:16]), 20*time.Minute)
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusFound)
}

// plain answers a person with one fixed sentence. Nothing of the request is repeated in it.
func plain(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, msg)
}

// login is Hydra's login step: hand the person to the company's identity provider.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ch := r.URL.Query().Get("login_challenge")
	lr, err := s.Hydra.GetLogin(r.Context(), ch)
	if err != nil {
		if errors.Is(err, ErrUnknown) {
			plain(w, 400, "This sign-in link is not valid any more. Start again from the platform you are connecting.")
		} else {
			s.log().Error("login: Hydra", "err", err)
			plain(w, 503, "Sign-in is not available now. Try again in a minute.")
		}
		return
	}
	if !s.resourceOK(lr.RequestURL) {
		mSignins.WithLabelValues("bad_resource").Inc()
		if first, err := s.claim(r.Context(), "login", ch); err != nil || !first {
			plain(w, 400, "This sign-in link is not valid any more. Start again from the platform you are connecting.")
			return
		}
		s.rejectLogin(w, r, ch, "invalid_target", "the resource parameter must be given once and be this server's MCP URL")
		return
	}
	t, err := s.Sealer.startTx(w, r, Tx{Login: ch, Nonce: random(24), Verifier: oauth2.GenerateVerifier()}, s.now())
	if err != nil {
		plain(w, 500, "Sign-in could not be started.")
		return
	}
	redirect(w, r, s.IdP.AuthURL(t.State, t.Nonce, t.Verifier))
}

func (s *Server) rejectLogin(w http.ResponseWriter, r *http.Request, ch, code, desc string) {
	to, err := s.Hydra.RejectLogin(r.Context(), ch, code, desc)
	if err != nil {
		plain(w, 400, "This sign-in link is not valid any more. Start again from the platform you are connecting.")
		return
	}
	redirect(w, r, to)
}

// callback is where the company's identity provider sends the person back.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for _, k := range []string{"state", "code", "error", "iss"} { // a parameter twice is read differently by different parts
		if len(q[k]) > 1 {
			plain(w, 400, "This sign-in could not be completed. Start again from the platform you are connecting.")
			return
		}
	}
	t, err := s.Sealer.takeTx(w, r, q.Get("state"), s.now())
	if err != nil {
		mSignins.WithLabelValues("no_transaction").Inc()
		s.log().Info("callback refused", "reason", err.Error())
		plain(w, 400, "This sign-in has expired, or was started in another browser. Start again from the platform you are connecting.")
		return
	}
	if first, err := s.claim(r.Context(), "login", t.Login); err != nil {
		plain(w, 503, "Sign-in is not available now. Try again in a minute.")
		return
	} else if !first {
		mSignins.WithLabelValues("challenge_used").Inc()
		plain(w, 400, "This sign-in link is not valid any more. Start again from the platform you are connecting.")
		return
	}
	deny := func(result, why string) {
		mSignins.WithLabelValues(result).Inc()
		s.log().Info("sign-in refused", "reason", result)
		s.rejectLogin(w, r, t.Login, "access_denied", why)
	}
	// RFC 9207 at the identity provider: when it says which server answered, it must be the one we sent the person to.
	if (q.Has("iss") && q.Get("iss") != s.Cfg.IdPIssuer) || (!q.Has("iss") && s.IdP.IssRequired()) {
		deny("idp_iss", "the identity provider's answer came from another issuer")
		return
	}
	if q.Has("error") {
		deny("idp_error", "the identity provider refused the sign-in")
		return
	}
	if q.Get("code") == "" {
		deny("idp_no_code", "the identity provider sent no code")
		return
	}
	claims, err := s.IdP.Exchange(r.Context(), q.Get("code"), t.Verifier)
	if err != nil {
		s.log().Info("sign-in: the identity provider's answer was not accepted", "err", err.Error())
		deny("idp_exchange", "the identity provider's answer was not accepted")
		return
	}
	if claims.Issuer != s.Cfg.IdPIssuer || !s.Sealer.Equal(claims.Nonce, t.Nonce) {
		deny("nonce_or_issuer", "the identity provider's answer was not accepted")
		return
	}
	// The same rule as everywhere else in Ambit: only A-Z are lower-cased (Unicode lower-casing could make two addresses one).
	email, usable := identity.CanonicalEmail(claims.Email)
	switch {
	case !usable || strings.Count(email, "@") != 1 || strings.ContainsAny(email, " \t\r\n,;<>\""):
		deny("no_email", "the identity provider sent no usable email address")
		return
	case !claims.EmailVerified:
		deny("email_unverified", "the email address is not verified")
		return
	case strings.HasSuffix(email, "@svc.ambit.internal"):
		deny("service_domain", "this account cannot sign in")
		return
	}
	off, err := s.Deny.Offboarded(email)
	if err != nil {
		s.log().Error("sign-in: the offboarding list is not readable", "err", err)
		mSignins.WithLabelValues("deny_unreadable").Inc()
		plain(w, 503, "Sign-in is not available now. Try again in a minute.")
		return
	}
	if off {
		deny("offboarded", "this account cannot sign in")
		return
	}
	groups := claims.Groups
	if len(groups) > 200 {
		groups = groups[:200]
	}
	// The consent step belongs to this browser: Hydra hands the context back with the consent request, and a cookie that
	// is sealed for this id is asked for there. (Hydra's own challenge strings change from step to step.)
	bind := random(18)
	to, err := s.Hydra.AcceptLogin(r.Context(), t.Login, email, map[string]any{"groups": groups, "idp_iss": claims.Issuer, "idp_sub": claims.Subject, "bind": bind})
	if err != nil {
		mSignins.WithLabelValues("hydra").Inc()
		plain(w, 400, "This sign-in link is not valid any more. Start again from the platform you are connecting.")
		return
	}
	if v, err := s.Sealer.Seal(Tx{Login: bind, Exp: s.now().Add(txMaxAge * time.Second).Unix()}, bindName(bind)); err == nil {
		setCookie(w, bindName(bind), v, txMaxAge)
	}
	mSignins.WithLabelValues("ok").Inc()
	redirect(w, r, to)
}

const bindPrefix = "__Host-ambit_bind_"

func bindName(id string) string {
	h := sha256.Sum256([]byte(id))
	return bindPrefix + hex.EncodeToString(h[:6])
}

// bound says whether this browser is the one that signed in for the consent request: the consent link alone (in a
// message, a log, a screenshot) neither shows the person's tools nor lets anyone create an agent.
func (s *Server) bound(r *http.Request, cr ConsentRequest) bool {
	id, _ := cr.Context["bind"].(string)
	if id == "" {
		return false
	}
	c, err := r.Cookie(bindName(id))
	if err != nil {
		return false
	}
	var t Tx
	return s.Sealer.Open(c.Value, bindName(id), &t) == nil && t.Login == id && s.now().Unix() <= t.Exp
}

// ---- consent

const csrfPrefix = "__Host-ambit_csrf_"

func csrfName(challenge string) string {
	h := sha256.Sum256([]byte(challenge))
	return csrfPrefix + hex.EncodeToString(h[:6]) // one cookie per consent: two consents in one browser do not collide
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Connect to Ambit</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:34rem;margin:2rem auto;padding:0 1rem}label{display:block;margin:.4rem 0}button{font:inherit;padding:.5rem 1rem;margin-right:.5rem}.note{color:#555;font-size:.9rem}</style>
<h1>Connect a platform to Ambit</h1>
<p><b>{{.Client}}</b> <span class="note">(this name was chosen by the platform; Ambit has not checked it)</span> wants to use Ambit's tools <b>as you</b>, {{.Subject}}.</p>
<p class="note">It will return to <b>{{.Host}}</b> after you decide.</p>
<form method="post" action="/consent">
<input type="hidden" name="challenge" value="{{.Challenge}}"><input type="hidden" name="csrf" value="{{.CSRF}}">
{{if .Servers}}<p>Tools it may use (only those you may use yourself; none is ticked):</p>
{{range .Servers}}<label><input type="checkbox" name="server" value="{{.Name}}"> {{.Display}}</label>{{end}}
{{else}}<p>No tool is open to platforms for you now. You can still connect it; it will have no tools until an admin changes that.</p>{{end}}
{{if .Connections}}<p>Apps where you signed in yourself:</p>
{{range .Connections}}<label><input type="checkbox" name="connection" value="{{.}}"> May use my sign-in to {{.}}</label>{{end}}{{end}}
<p><button name="action" value="allow">Allow</button><button name="action" value="deny">Deny</button></p>
</form>
<p class="note">You can end this connection later in Ambit.</p></html>`))

func printable(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// requestRedirect is the address this authorization request returns to: the redirect_uri of the request (Hydra has
// compared it with the client's registered addresses), not the client's first one.
func requestRedirect(cr ConsentRequest) *url.URL {
	if u, err := url.Parse(cr.RequestURL); err == nil {
		if ru, err := url.Parse(u.Query().Get("redirect_uri")); err == nil && ru.Host != "" {
			return ru
		}
	}
	return &url.URL{}
}

func (s *Server) rejectConsent(w http.ResponseWriter, r *http.Request, ch, code, desc string) {
	to, err := s.Hydra.RejectConsent(r.Context(), ch, code, desc)
	if err != nil {
		plain(w, 400, "This consent link is not valid any more. Start again from the platform you are connecting.")
		return
	}
	redirect(w, r, to)
}

func (s *Server) consentPage(w http.ResponseWriter, r *http.Request) {
	ch := r.URL.Query().Get("consent_challenge")
	cr, err := s.Hydra.GetConsent(r.Context(), ch)
	if err != nil {
		if errors.Is(err, ErrUnknown) {
			plain(w, 400, "This consent link is not valid any more. Start again from the platform you are connecting.")
		} else {
			plain(w, 503, "Consent is not available now. Try again in a minute.")
		}
		return
	}
	if !s.bound(r, cr) {
		mConsent.WithLabelValues("not_bound").Inc()
		plain(w, 403, "This page can be opened only in the browser where you signed in. Start again from the platform you are connecting.")
		return
	}
	if !s.resourceOK(cr.RequestURL) {
		mConsent.WithLabelValues("bad_resource").Inc()
		s.rejectConsent(w, r, ch, "invalid_target", "the resource parameter must be given once and be this server's MCP URL")
		return
	}
	servers, err := s.Backend.Servers(r.Context(), cr.Subject, consentGroups(cr))
	if err != nil {
		s.log().Error("consent: the console did not answer", "err", err)
		plain(w, 503, "Consent is not available now. Try again in a minute.")
		return
	}
	var conns []string
	for _, sv := range servers {
		if sv.Connection != "" && !slices.Contains(conns, sv.Connection) {
			conns = append(conns, sv.Connection)
		}
	}
	cookie := random(24)
	setCookie(w, csrfName(ch), cookie, 600)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentPage.Execute(w, map[string]any{"Client": printable(cr.Client.Name, 100), "Subject": cr.Subject, "Host": requestRedirect(cr).Host,
		"Challenge": ch, "CSRF": s.Sealer.Token("csrf", cookie, ch), "Servers": servers, "Connections": conns})
}

func (s *Server) consentSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.Cfg.PublicURL {
		mConsent.WithLabelValues("origin").Inc()
		plain(w, 403, "This request did not come from the consent page.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		plain(w, 400, "The form could not be read.")
		return
	}
	ch := r.PostForm.Get("challenge")
	ck, err := r.Cookie(csrfName(ch))
	if err != nil || !s.Sealer.Equal(s.Sealer.Token("csrf", ck.Value, ch), r.PostForm.Get("csrf")) {
		mConsent.WithLabelValues("csrf").Inc()
		plain(w, 403, "This request did not come from the consent page.")
		return
	}
	cr, err := s.Hydra.GetConsent(r.Context(), ch)
	if err != nil {
		setCookie(w, csrfName(ch), "", -1)
		plain(w, 400, "This consent link is not valid any more. Start again from the platform you are connecting.")
		return
	}
	if !s.bound(r, cr) {
		mConsent.WithLabelValues("not_bound").Inc()
		plain(w, 403, "This page can be opened only in the browser where you signed in. Start again from the platform you are connecting.")
		return
	}
	// The form's cookie ends with the decision: a form that is refused (a tool the person may not offer) can be sent again.
	decide := func() bool { // exactly one decision per challenge, even if two requests race
		first, err := s.claim(r.Context(), "consent", ch)
		if err != nil {
			plain(w, 503, "Consent is not available now. Try again in a minute.")
			return false
		}
		if !first {
			mConsent.WithLabelValues("challenge_used").Inc()
			plain(w, 400, "This consent link is not valid any more. Start again from the platform you are connecting.")
			return false
		}
		return true
	}
	if !s.resourceOK(cr.RequestURL) {
		if !decide() {
			return
		}
		setCookie(w, csrfName(ch), "", -1)
		mConsent.WithLabelValues("bad_resource").Inc()
		s.rejectConsent(w, r, ch, "invalid_target", "the resource parameter must be given once and be this server's MCP URL")
		return
	}
	if r.PostForm.Get("action") != "allow" {
		if !decide() {
			return
		}
		setCookie(w, csrfName(ch), "", -1)
		mConsent.WithLabelValues("denied").Inc()
		s.rejectConsent(w, r, ch, "access_denied", "the person did not allow the connection")
		return
	}
	// What was ticked must be what the person may offer now (AccessRules ∩ the front door's servers): the console
	// answers again; a name that is not in that answer is refused, and nothing is created.
	allowed, err := s.Backend.Servers(r.Context(), cr.Subject, consentGroups(cr))
	if err != nil {
		plain(w, 503, "Consent is not available now. Try again in a minute.")
		return
	}
	byName := map[string]ServerChoice{}
	for _, a := range allowed {
		byName[a.Name] = a
	}
	var scope, conns []string
	for _, n := range r.PostForm["server"] {
		sv, ok := byName[n]
		if !ok {
			mConsent.WithLabelValues("server_not_allowed").Inc()
			plain(w, 400, "A tool you ticked is not open to you. Go back and choose again.")
			return
		}
		if !slices.Contains(scope, n) {
			scope = append(scope, n)
		}
		_ = sv
	}
	if len(scope) > 50 {
		plain(w, 400, "Too many tools were chosen.")
		return
	}
	for _, c := range r.PostForm["connection"] {
		ok := false
		for _, n := range scope {
			ok = ok || byName[n].Connection == c
		}
		if !ok {
			mConsent.WithLabelValues("connection_not_allowed").Inc()
			plain(w, 400, "A connection you ticked does not belong to a tool you chose.")
			return
		}
		if !slices.Contains(conns, c) {
			conns = append(conns, c)
		}
	}
	if scope == nil {
		scope, conns = []string{}, []string{}
	}
	if !decide() {
		return
	}
	setCookie(w, csrfName(ch), "", -1)
	if id, _ := cr.Context["bind"].(string); id != "" {
		setCookie(w, bindName(id), "", -1)
	}
	grant := NewGrantID(s.now())
	platform := Platform(requestRedirect(cr).Hostname())
	agent, err := s.Backend.CreateAgent(r.Context(), AgentInput{Owner: cr.Subject, Platform: platform,
		DisplayName: printable(cr.Client.Name, 60), Scope: scope, Connections: conns, GrantID: grant, Groups: groupNames(cr.Context["groups"])})
	if err != nil {
		var refused ErrRefused
		if errors.As(err, &refused) {
			mConsent.WithLabelValues("agent_refused").Inc()
			s.log().Info("consent: the console refused the agent", "reason", refused.Msg)
			s.rejectConsent(w, r, ch, "access_denied", "the connection could not be created")
			return
		}
		s.log().Error("consent: the console did not create the agent", "err", err)
		plain(w, 503, "Consent is not available now. Try again in a minute.")
		return
	}
	groups, _ := cr.Context["groups"].([]any)
	to, err := s.Hydra.AcceptConsent(r.Context(), ch, AcceptConsentBody{GrantScope: cr.RequestedScope, GrantAudience: []string{s.Cfg.Resource},
		AccessToken: map[string]any{"email": cr.Subject, "email_verified": true, "groups": groups, "ambit_agent": agent, "ambit_agent_mode": "person",
			"ambit_agent_owner_at_issue": cr.Subject, "ambit_grant": grant, "ambit_scope": scope, "ambit_platform_declared": platform}})
	if err != nil {
		s.log().Error("consent: Hydra did not accept", "err", err)
		plain(w, 400, "This consent link is not valid any more. Start again from the platform you are connecting.")
		return
	}
	s.touchClient(r, cr.Client.ID)
	mConsent.WithLabelValues("allowed").Inc()
	s.log().Info("connection allowed", "client_id", cr.Client.ID, "agent", agent, "grant", grant, "servers", len(scope))
	redirect(w, r, to)
}

// touchClient writes "last used" into a dynamic client's metadata, at most once a day: the janitor deletes clients that
// nobody used for Config.UnusedClientDays. The first call also marks the client as used: one that never was is deleted much sooner.
func (s *Server) touchClient(r *http.Request, id string) {
	if id == "" || s.Clients == nil {
		return
	}
	first, err := s.Clients.Once(r.Context(), "touch:"+id, 24*time.Hour)
	if err != nil || !first {
		return
	}
	c, err := s.Hydra.GetClient(r.Context(), id)
	if err != nil || c.Metadata["registered_via"] != "dcr" {
		return
	}
	c.Metadata["last_used"] = s.now().UTC().Format(time.RFC3339)
	c.Metadata["used"] = true // a consent or a refresh: the client is not one that only registered (security review N2)
	if err := s.Hydra.SetClientMetadata(r.Context(), id, c.Metadata); err != nil {
		s.log().Warn("could not write the client's last use", "err", err)
	}
}

// consentGroups are the groups the identity provider gave at sign-in (ambit-auth put them into the login context).
func consentGroups(cr ConsentRequest) []string {
	var out []string
	if l, ok := cr.Context["groups"].([]any); ok {
		for _, g := range l {
			if s, ok := g.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// NewGrantID is "g-<seconds since 1970 in base 36>-<random>": the age of a grant can be read from its name, which the token
// hook uses to end a grant after oauth.maxGrantDays (the groups in a token are those of the sign-in, so a long grant would
// keep a permission that was taken away at the identity provider).
func NewGrantID(now time.Time) string {
	return "g-" + strconv.FormatInt(now.Unix(), 36) + "-" + random(9)
}

// grantTime reads the time at which a grant was made from its name; ok is false for a name that was not made by NewGrantID.
var grantForm = regexp.MustCompile(`^g-[0-9a-z]{1,13}-[A-Za-z0-9_-]{6,24}$`)

func grantTime(id string) (time.Time, bool) {
	if !grantForm.MatchString(id) {
		return time.Time{}, false
	}
	parts := strings.Split(id, "-")
	if len(parts) < 3 || parts[0] != "g" {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(parts[1], 36, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// GrantTime is the time at which a grant was made (see NewGrantID).
func GrantTime(id string) (time.Time, bool) { return grantTime(id) }

// GrantAge reads the age of a grant from its name; ok is false for a name that was not made by NewGrantID.
func GrantAge(id string, now time.Time) (time.Duration, bool) {
	t, ok := grantTime(id)
	if !ok {
		return 0, false
	}
	return now.Sub(t), true
}

// groupNames reads the groups that the sign-in put into the consent request's context.
func groupNames(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
