package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Guard 2: a sign-in whose email is in the service account domain gets 403 and no session cookie.
func TestSignInRefusesTheServiceDomain(t *testing.T) {
	for _, c := range []struct {
		email string
		want  int
	}{
		{"user01@example.com", http.StatusFound},
		{"ci-deploy@svc.ambit.internal", http.StatusForbidden},
		{" CI-Deploy@SVC.Ambit.Internal ", http.StatusForbidden},
	} {
		f := newFakeIssuer(t)
		f.connector, f.email, f.groups = "okta", c.email, []string{"ambit-admins"}
		ts, cl := newTestServer(t, f)
		r := signIn(t, ts, cl, f, false)
		session := false
		for _, ck := range r.Cookies() {
			session = session || (ck.Name == SessionCookie && ck.Value != "")
		}
		if r.StatusCode != c.want || session != (c.want == http.StatusFound) {
			t.Errorf("%q: %d, session cookie %v; want %d", c.email, r.StatusCode, session, c.want)
		}
	}
	// a session cookie made before the upgrade for such an address is not accepted either
	newTestServer(t, newFakeIssuer(t))
	s := Session{ID: "x", Email: "ci@svc.ambit.internal", Exp: time.Now().Add(time.Hour).Unix()}
	testAuth.Sessions.Add(s.ID, s.Email, s.Exp)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: testAuth.codec.Encode(s)})
	if _, ok := testAuth.Current(r); ok {
		t.Error("a session for a reserved address was accepted")
	}
}
