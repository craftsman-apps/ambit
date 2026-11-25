#!/usr/bin/env python3
"""End-to-end checks of the OAuth sign-in (test/stepoauth.sh runs this). Standard library only.

It plays two parts: a hosted platform (registers a client, starts the authorization, exchanges the code, refreshes) and
the person's browser (cookies, redirects, the company identity provider's form). Nothing leaves the machine: every
connection goes to a kubectl port-forward on 127.0.0.1, with the real host names for the certificate check.

Environment: GW_PORT (the public Gateway, https), IDP_HOST (the identity provider's name in the cluster), IDP_PORT
(a port-forward to it), CONSOLE_URL (a port-forward to the fake console), CA_FILE, AUTH_HOST, MCP_HOST, KUBECTL
(a command that runs kubectl in the right namespace; used for the stop list and restarts)."""
import base64, hashlib, http.client, http.cookiejar, json, os, re, secrets, socket, ssl, subprocess, sys, time
import urllib.parse, urllib.request

GW = int(os.environ["GW_PORT"]); IDP_PORT = int(os.environ["IDP_PORT"]); CA = os.environ["CA_FILE"]
AUTH = os.environ["AUTH_HOST"]; MCP = os.environ["MCP_HOST"]; IDP = os.environ["IDP_HOST"]; CONSOLE = os.environ["CONSOLE_URL"]
KUBECTL = os.environ["KUBECTL"].split()
# The stop list (ConfigMap ambit-agent-stops) may be written by the console only (admission policy ambit-keys-writer): the test acts as the
# console's service account, as a pod of it.
KUBECTL_CONSOLE = os.environ.get("KUBECTL_CONSOLE", os.environ["KUBECTL"]).split()
IDP_PORT_URL = int(os.environ.get("IDP_URL_PORT", "443"))
IDP_BASE = f"https://{IDP}" + ("" if IDP_PORT_URL == 443 else f":{IDP_PORT_URL}")
RES = f"https://{MCP}/mcp"; ISS = f"https://{AUTH}"
REDIRECT = "https://chatgpt.com/connector_platform_oauth_redirect"
fails = []


def check(name, cond, detail=""):
    print(("PASS " if cond else "FAIL ") + name + ("" if cond else f": {detail}"), flush=True)
    if not cond:
        fails.append(name)


def b64(b): return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def pkce():
    v = b64(secrets.token_bytes(32)); return v, b64(hashlib.sha256(v.encode()).digest())


# ---- connections: the real name for the certificate, a local port for the socket
ctx = ssl.create_default_context(cafile=CA)
ROUTES = {(AUTH, 443): GW, (IDP, int(os.environ.get("IDP_URL_PORT", "443"))): IDP_PORT, (MCP, 443): GW}


class Conn(http.client.HTTPSConnection):
    def connect(self):
        port = ROUTES.get((self.host, self.port))
        if port is None:
            raise OSError(f"no route to {self.host}:{self.port}")
        sock = socket.create_connection(("127.0.0.1", port), timeout=20)
        self.sock = ctx.wrap_socket(sock, server_hostname=self.host)


class Handler(urllib.request.HTTPSHandler):
    def https_open(self, req): return self.do_open(Conn, req)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k): return None


class Browser:
    """Cookies, and redirects followed by hand: the platform's own address (chatgpt.com) is never fetched."""
    def __init__(self, addr=None):
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(Handler(), urllib.request.HTTPCookieProcessor(self.jar), NoRedirect())
        self.extra = {}

    def req(self, method, url, data=None, headers=None):
        h = dict(self.extra); h.update(headers or {})
        body = urllib.parse.urlencode(data).encode() if isinstance(data, dict) else data
        if isinstance(data, dict): h.setdefault("Content-Type", "application/x-www-form-urlencoded")
        r = urllib.request.Request(url, data=body, method=method, headers=h)
        try:
            resp = self.op.open(r, timeout=int(os.environ.get('HTTP_TIMEOUT', '30')))
        except urllib.error.HTTPError as e:
            resp = e
        return resp.status if hasattr(resp, "status") else resp.code, dict(resp.headers), resp.read().decode("utf-8", "replace")

    def cookie_names(self): return sorted(c.name for c in self.jar)

    def follow(self, url, stop=("chatgpt.com", "claude.ai"), max_hops=15, form=None):
        """GET url and follow redirects until one leaves for a platform. Returns (status, headers, body, last_url)."""
        method, data = "GET", None
        for _ in range(max_hops):
            st, h, body = self.req(method, url, data)
            loc = h.get("Location")
            if st in (301, 302, 303, 307) and loc:
                url = urllib.parse.urljoin(url, loc)
                if urllib.parse.urlparse(url).hostname in stop:
                    return st, h, body, url
                method, data = "GET", None
                continue
            return st, h, body, url
        raise RuntimeError("too many redirects")


def base_url(path): return f"https://{AUTH}{path}"


def api(method, path, body=None, headers=None, browser=None):
    b = browser or Browser()
    data = json.dumps(body).encode() if body is not None else None
    st, h, t = b.req(method, base_url(path), data, {"Content-Type": "application/json", **(headers or {})})
    try: j = json.loads(t)
    except Exception: j = {}
    return st, h, j


def register(redirect=REDIRECT, name="ChatGPT", **extra):
    return api("POST", "/register", {"client_name": name, "redirect_uris": [redirect], "application_type": "web", **extra})


def authorize_url(client_id, challenge, state, redirect=REDIRECT, resource=(RES,), extra=None):
    q = [("client_id", client_id), ("redirect_uri", redirect), ("response_type", "code"), ("code_challenge", challenge),
         ("code_challenge_method", "S256"), ("state", state)] + [("resource", r) for r in resource] + list((extra or {}).items())
    return base_url("/oauth2/auth?" + urllib.parse.urlencode(q))


def idp_pick(b, form_html, url, user, action="login"):
    """The company's sign-in page: read the hidden fields, post the choice."""
    f = dict(re.findall(r'name="([a-z_]+)" value="([^"]*)"', form_html))
    f = {k: v.replace("&amp;", "&") for k, v in f.items() if k in ("state", "nonce", "code_challenge", "redirect_uri")}
    f.update({"user": user, "action": action})
    return b.req_follow_post(f"{IDP_BASE}/authorize", f)


def _post_follow(self, url, data):
    st, h, body = self.req("POST", url, data)
    loc = h.get("Location")
    if st in (301, 302, 303) and loc:
        return self.follow(urllib.parse.urljoin(url, loc))
    return st, h, body, url


Browser.req_follow_post = _post_follow


def sign_in(b, client_id, user, challenge=None, state=None, resource=(RES,), tick=("time",), connections=(), consent=True, redirect=REDIRECT, extra=None):
    """The whole browser part. Returns (kind, detail): ("code", {code,state,iss}) or ("error", {error,...}) or ("page", (status, body))."""
    v, ch = (None, challenge) if challenge else pkce()
    state = state or secrets.token_urlsafe(8)
    st, h, body, url = b.follow(authorize_url(client_id, ch, state, redirect, resource, extra))
    host = urllib.parse.urlparse(url).hostname
    if host == IDP:  # at the company's sign-in page
        st, h, body, url = idp_pick(b, body, url, user)
        host = urllib.parse.urlparse(url).hostname
    if host == AUTH and urllib.parse.urlparse(url).path == "/consent":
        if not consent:
            return "consent", (st, body, url, v, state)
        form = dict(re.findall(r'name="(challenge|csrf)" value="([^"]*)"', body))
        form["action"] = "allow"
        data = list(form.items()) + [("server", t) for t in tick] + [("connection", c) for c in connections]
        st, h, body = b.req("POST", base_url("/consent"), urllib.parse.urlencode(data).encode(),
                            {"Origin": ISS, "Content-Type": "application/x-www-form-urlencoded"})
        loc = h.get("Location")
        if st == 302 and loc:
            st, h, body, url = b.follow(urllib.parse.urljoin(base_url("/consent"), loc))
        host = urllib.parse.urlparse(url).hostname
    if host in ("chatgpt.com", "claude.ai"):
        q = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
        flat = {k: x[0] for k, x in q.items()}
        flat["_url"] = url; flat["_verifier"] = v
        return ("code" if "code" in flat else "error"), flat
    return "page", (st, body, url)


def token(client_id, **form):
    b = Browser()
    try:
        st, h, t = b.req("POST", base_url("/oauth2/token"), {"client_id": client_id, **form})
    except Exception as e:  # a request that gets no answer in time is a refusal for the test
        return 0, {"error": "no answer: " + type(e).__name__}
    try: j = json.loads(t)
    except Exception: j = {}
    return st, j


def jwt_claims(tok):
    p = tok.split(".")[1]; p += "=" * (-len(p) % 4)
    return json.loads(base64.urlsafe_b64decode(p))


def exchange(client_id, res):
    return token(client_id, grant_type="authorization_code", code=res["code"], redirect_uri=REDIRECT, code_verifier=res["_verifier"], resource=RES)


def kube(*args, input=None):
    r = subprocess.run(KUBECTL + list(args), input=input, capture_output=True, text=True)
    return r.stdout.strip()


def console_state():
    with urllib.request.urlopen(CONSOLE + "/_state", timeout=10) as r: return json.load(r)


def console_post(path, data=None):
    req = urllib.request.Request(CONSOLE + path, data=data, method="POST")
    urllib.request.urlopen(req, timeout=10).read()


def stop_list(*entries):
    now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    cm = {"gen": int(time.time()), "entries": [{"kind": k, "name": n, "gen": int(time.time()), "slot": i, "requestedAt": now} for i, (k, n) in enumerate(entries)]}
    y = subprocess.run(KUBECTL + ["create", "configmap", "ambit-agent-stops", "--from-literal=list.json=" + json.dumps(cm), "--dry-run=client", "-o", "yaml"], capture_output=True, text=True).stdout
    r = subprocess.run(KUBECTL_CONSOLE + ["apply", "-f", "-"], input=y, capture_output=True, text=True)
    if r.returncode != 0:
        print("FAIL the stop list could not be written:", r.stderr.strip()[:300]); sys.exit(1)


def wait(fn, secs=30, every=1.0):
    end = time.time() + secs
    while time.time() < end:
        try:
            if fn(): return True
        except Exception: pass
        time.sleep(every)
    return False


def limits_only():
    """The registration limit, run on its own and first, with the limit at 10 per hour per address (the step lowers it)."""
    codes = []
    for i in range(14):
        st, _, j = api("POST", "/register", {"client_name": f"c{i}", "redirect_uris": [REDIRECT]},
                       {"X-Forwarded-For": f"198.51.100.{i}", "X-Real-IP": f"198.51.100.{i}", "Forwarded": f"for=198.51.100.{i}", "X-Ambit-Client-Address": f"198.51.100.{i}"})
        codes.append(st)
    check("8 registration limit: 10 per hour per address, counted in Valkey by both replicas; X-Forwarded-For, X-Real-IP, Forwarded and X-Ambit-Client-Address from the caller change nothing",
          codes[:10].count(201) >= 9 and 429 in codes[9:] and codes[-1] == 429 and codes.count(201) <= 10, codes)
    print("FAILED: " + ", ".join(fails) if fails else "all checks passed")
    sys.exit(1 if fails else 0)


if len(sys.argv) > 1 and sys.argv[1] == "limits":
    limits_only()

# ======================================================================================================================
# 1 · discovery and registration
st, h, md = api("GET", "/.well-known/oauth-authorization-server")
check("1 discovery: Ambit's own document", st == 200 and md.get("issuer") == ISS and md.get("authorization_response_iss_parameter_supported") is True
      and md.get("code_challenge_methods_supported") == ["S256"] and md.get("response_modes_supported") == ["query"]
      and md.get("registration_endpoint") == ISS + "/register" and md.get("token_endpoint") == ISS + "/oauth2/token", (st, md))
check("1 discovery: security headers", h.get("X-Frame-Options") == "DENY" and "frame-ancestors 'none'" in h.get("Content-Security-Policy", "")
      and h.get("Cache-Control") == "no-store", h)

st, _, reg = register()
check("1 registration: a platform registers itself", st == 201 and reg.get("token_endpoint_auth_method") == "none" and reg.get("client_id"), (st, reg))
CID = reg.get("client_id", "")

# ======================================================================================================================
# 2 · the whole sign-in, token, refresh
b = Browser()
kind, res = sign_in(b, CID, "alice")
check("2 sign-in: code, state and iss come back to the platform's address", kind == "code" and res.get("iss") == ISS and res["_url"].startswith(REDIRECT) and res.get("state"), (kind, res))
check("2 sign-in: the transaction cookie is gone", not any(n.startswith("__Host-ambit_tx_") for n in b.cookie_names()), b.cookie_names())
st, tok = exchange(CID, res) if kind == "code" else (0, {})
check("2 token: a JWT access token and a refresh token", st == 200 and tok.get("access_token", "").count(".") == 2 and tok.get("refresh_token"), (st, tok))
claims = jwt_claims(tok["access_token"]) if tok.get("access_token") else {}
check("2 token: issuer, audience (our one MCP URL), person and a life of 15 minutes", claims.get("iss") == ISS and claims.get("aud") == [RES]
      and claims.get("sub") == "alice@acme.test" and abs(claims.get("exp", 0) - claims.get("iat", 0) - 900) <= 1, claims)
state = console_state()
agent = claims.get("ambit_agent")
check("2 token: the agent and the grant of the consent are in it, the scope is what was ticked", agent and claims.get("ambit_agent_mode") == "person"
      and claims.get("ambit_grant", "").startswith("g-") and claims.get("ambit_scope") == ["time"] and claims.get("ambit_platform_declared") == "chatgpt"
      and claims.get("ambit_agent_owner_at_issue") == "alice@acme.test" and sorted(claims.get("groups", [])) == ["eng", "ml-research"]
      and re.match(r"^g-[0-9a-z]+-", claims.get("ambit_grant", "")), claims)
check("2 console: the agent was created for the person, and the console saw a token for its audience only",
      any(c.get("owner") == "alice@acme.test" and c.get("grantID") == claims.get("ambit_grant") and c.get("scope") == ["time"] for c in state["created"])
      and state["tokenAudience"] == ["ambit-console"], state)
# refresh: new tokens; the groups stay those of the sign-in; the agent and the grant stay
st, tok2 = token(CID, grant_type="refresh_token", refresh_token=tok.get("refresh_token", ""))
c2 = jwt_claims(tok2["access_token"]) if tok2.get("access_token") else {}
check("2 refresh: a new access token with the same agent, grant, scope and groups", st == 200 and sorted(c2.get("groups", [])) == ["eng", "ml-research"] and c2.get("ambit_grant") == claims.get("ambit_grant")
      and c2.get("ambit_agent") == agent and c2.get("ambit_scope") == ["time"], (st, c2))
st, again = token(CID, grant_type="refresh_token", refresh_token=tok.get("refresh_token", ""))
check("2 refresh: the old refresh token is used up (rotation with reuse detection)", st in (400, 401) and "access_token" not in again, (st, again))

# ======================================================================================================================
# 3 · stop and offboarding refuse a refresh; an unrelated grant keeps working
b2 = Browser(); k2, r2 = sign_in(b2, CID, "alice")
s2, t2 = exchange(CID, r2)
cl2 = jwt_claims(t2["access_token"]); other_grant = cl2["ambit_grant"]
b3 = Browser(); k3, r3 = sign_in(b3, CID, "alice")
s3, t3 = exchange(CID, r3)
stop_list(("grant", cl2["ambit_grant"]))
time.sleep(5)  # ambit-auth reads the list every 3 seconds
st, j = token(CID, grant_type="refresh_token", refresh_token=t2.get("refresh_token", ""))
check("3 stop: the refresh of a stopped grant is refused (access_denied)", st in (400, 401, 403) and "access_token" not in j and "denied" in json.dumps(j).lower(), (st, j))
st, j3 = token(CID, grant_type="refresh_token", refresh_token=t3.get("refresh_token", ""))
check("3 stop: another grant of the same person refreshes", st == 200 and j3.get("access_token"), (st, j3))
stop_list()  # an empty list: the grant is free again; a refused refresh did not use the refresh token up
time.sleep(5)
st, j = token(CID, grant_type="refresh_token", refresh_token=t2.get("refresh_token", ""))
check("3 start: after the entry is gone the same refresh token works (a refused refresh did not consume it)", st == 200 and j.get("access_token"), (st, j))
stop_list(("agent", jwt_claims(j["access_token"])["ambit_agent"]) if j.get("access_token") else ("agent", "x"))
time.sleep(5)
st, j4 = token(CID, grant_type="refresh_token", refresh_token=j.get("refresh_token", ""))
check("3 stop by agent: the refresh is refused", st in (400, 401, 403) and "access_token" not in j4, (st, j4))
stop_list(); time.sleep(4)
# the console says the agent is not active (deleted, stopped, or the connection ended): the refresh is refused, whatever the lists say
bi = Browser(); ki, ri = sign_in(bi, CID, "alice"); si, ti = exchange(CID, ri)
agent_i = jwt_claims(ti["access_token"])["ambit_agent"]
console_post("/_inactive?agent=%s&on=true" % agent_i)
st, ji = token(CID, grant_type="refresh_token", refresh_token=ti.get("refresh_token", ""))
check("3 an agent that the console reports as not active: the refresh is refused", st in (400, 401, 403) and "access_token" not in ji, (st, ji))
console_post("/_inactive?agent=%s&on=false" % agent_i)
st, ji = token(CID, grant_type="refresh_token", refresh_token=ti.get("refresh_token", ""))
check("3 ... and it works again when the agent is active (a refused refresh did not consume the token)", st == 200 and ji.get("access_token"), (st, ji))
# offboarded person
bo = Browser(); ko, ro = sign_in(bo, CID, "bob")
so, to = exchange(CID, ro) if ko == "code" else (0, {})
check("3 offboarding: before the person is on the list, sign-in works", ko == "code" and so == 200, (ko, ro))
kube("create", "configmap", "ambit-offboarded", "--from-literal=emails=[\"bob@acme.test\"]")
time.sleep(5)
st, j = token(CID, grant_type="refresh_token", refresh_token=to.get("refresh_token", ""))
check("3 offboarding: the refresh of an offboarded person is refused", st in (400, 401, 403) and "access_token" not in j, (st, j))
kind, det = sign_in(Browser(), CID, "bob")
check("3 offboarding: a new sign-in of the person is refused with access_denied and iss", kind == "error" and det.get("error") == "access_denied" and det.get("iss") == ISS, (kind, det))
kube("delete", "configmap", "ambit-offboarded", "--ignore-not-found")

# ======================================================================================================================
# 4 · two sign-ins in one browser (two platform tabs), finished in reverse order
REDIRECT2 = "https://claude.ai/api/mcp/auth_callback"
st, _, reg2 = register(REDIRECT2, "Claude")
CID2 = reg2.get("client_id", "")
bb = Browser()
v1, c1 = pkce(); v2, c2_ = pkce()
def begin(bro, cid, ch, st, redirect):
    s, h, body, url = bro.follow(authorize_url(cid, ch, st, redirect))
    return body, url
body1, url1 = begin(bb, CID, c1, "state-one", REDIRECT)
body2, url2 = begin(bb, CID2, c2_, "state-two", REDIRECT2)
check("4 two pending sign-ins have two cookies", len([n for n in bb.cookie_names() if n.startswith("__Host-ambit_tx_")]) == 2, bb.cookie_names())
def finish(bro, body, url, user, tick):
    st, h, body, url = idp_pick(bro, body, url, user)
    form = dict(re.findall(r'name="(challenge|csrf)" value="([^"]*)"', body)); form["action"] = "allow"
    data = list(form.items()) + [("server", t) for t in tick]
    st, h, body = bro.req("POST", base_url("/consent"), urllib.parse.urlencode(data).encode(), {"Origin": ISS, "Content-Type": "application/x-www-form-urlencoded"})
    if "Location" not in h:
        print("   consent POST answered", st, body[:200]); return {}
    st, h, body, url = bro.follow(urllib.parse.urljoin(base_url("/consent"), h["Location"]))
    return urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
q2 = finish(bb, body2, url2, "alice", ())
q1 = finish(bb, body1, url1, "alice", ("time",))
check("4 reverse order: both finish with their own state, to their own platform", q2.get("state") == ["state-two"] and q1.get("state") == ["state-one"] and "code" in q1 and "code" in q2, (q1, q2))
sa, ta = token(CID, grant_type="authorization_code", code=q1["code"][0], redirect_uri=REDIRECT, code_verifier=v1, resource=RES)
sb, tb = token(CID2, grant_type="authorization_code", code=q2["code"][0], redirect_uri=REDIRECT2, code_verifier=v2, resource=RES)
check("4 reverse order: each code works with its own client and verifier, and the scopes and platforms stay apart", sa == 200 and sb == 200 and jwt_claims(ta["access_token"])["ambit_scope"] == ["time"]
      and jwt_claims(tb["access_token"])["ambit_scope"] == [] and jwt_claims(tb["access_token"])["ambit_platform_declared"] == "claude-ai", (sa, sb))
sc, _ = token(CID, grant_type="authorization_code", code=q1["code"][0], redirect_uri=REDIRECT, code_verifier=v1, resource=RES)
check("4 a code is used once", sc in (400, 401), sc)
sd, _ = token(CID2, grant_type="authorization_code", code=q2["code"][0], redirect_uri=REDIRECT2, code_verifier=v1, resource=RES)
check("4 a code with another verifier is refused", sd in (400, 401), sd)

# ======================================================================================================================
# 5 · the sign-in at the identity provider: what is refused
for user in ("mallory-iss", "mallory-nonce", "mallory-aud", "mallory-exp", "mallory-cbiss", "carol", "svcfake", "noemail"):
    kind, det = sign_in(Browser(), CID, user)
    check(f"5 {user}: the platform gets access_denied (with iss), no code", kind == "error" and det.get("error") == "access_denied" and det.get("iss") == ISS and "code" not in det, (kind, det))
bd = Browser(); st, h, body, url = bd.follow(authorize_url(CID, pkce()[1], "state-deny"))
st, h, body, url = idp_pick(bd, body, url, "alice", action="deny")
q = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
check("5 the person denies at the identity provider: access_denied with iss", q.get("error") == ["access_denied"] and q.get("iss") == [ISS] and q.get("state") == ["state-deny"], url)

# callback attacks, by hand
def pending():
    bro = Browser(); s, h, body, url = bro.follow(authorize_url(CID, pkce()[1], "state-attack"))
    return bro, body, url
bro, body, url = pending()
f = dict(re.findall(r'name="([a-z_]+)" value="([^"]*)"', body)); st_ = f["state"].replace("&amp;", "&")
cb = lambda extra, br: br.req("GET", base_url("/callback?" + urllib.parse.urlencode(extra)))
s, _, t = cb({"code": "x", "state": st_ + "x"}, bro)
check("5 callback with a wrong state: 400, no sign-in", s == 400, (s, t))
s, _, t = cb({"code": "x", "state": st_}, Browser())
check("5 callback in another browser (no transaction cookie): 400", s == 400, (s, t))
bro2, body2_, url2_ = pending(); f2 = dict(re.findall(r'name="([a-z_]+)" value="([^"]*)"', body2_)); st2 = f2["state"].replace("&amp;", "&")
for c in bro2.jar: c.value = c.value[:-4] + "AAAA"
s, _, t = cb({"code": "x", "state": st2}, bro2)
check("5 callback with a changed cookie: 400", s == 400, (s, t))
bro3, body3_, _ = pending(); f3 = dict(re.findall(r'name="([a-z_]+)" value="([^"]*)"', body3_)); st3 = f3["state"].replace("&amp;", "&")
bro3b = Browser()  # the cookie of one sign-in presented under the state of another
bro4, body4_, _ = pending(); f4 = dict(re.findall(r'name="([a-z_]+)" value="([^"]*)"', body4_)); st4 = f4["state"].replace("&amp;", "&")
for c in list(bro3.jar): bro4.jar.set_cookie(c)
s, _, t = cb({"code": "x", "state": st4}, bro4)
check("5 callback: a cookie of another sign-in does not complete this one", s in (302, 400), s)  # the real cookie of st4 is also there: this one runs and the IdP code 'x' fails
bro5, body5_, _ = pending(); f5 = dict(re.findall(r'name="([a-z_]+)" value="([^"]*)"', body5_)); st5 = f5["state"].replace("&amp;", "&")
s, h, t = cb({"code": "x", "state": st5, "iss": "https://evil.example.com"}, bro5)
_, _, _, url = bro5.follow(urllib.parse.urljoin(base_url("/"), h.get("Location", "/")))
q = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
check("5 callback with an iss that is not the identity provider's: refused (access_denied)", s == 302 and q.get("error") == ["access_denied"] and "code" not in q, (s, url[:200]))
s, _, t = cb({"code": "x", "state": st5}, bro5)
check("5 callback replay: 400", s == 400, s)

# ======================================================================================================================
# 6 · the authorization request: the resource, PKCE, modes, parameters
ch = pkce()[1]
def front(**kw):
    b_ = Browser(); st, h, body = b_.req("GET", authorize_url(CID, ch, "state-six", **kw)); return st
for name, kw in {"no resource": dict(resource=()), "resource twice": dict(resource=(RES, RES)), "other resource": dict(resource=("https://other.example.com/mcp",)),
                 "two different": dict(resource=(RES, "https://other.example.com/mcp")), "plain pkce": dict(extra={"code_challenge_method": "plain"}),
                 "fragment mode": dict(extra={"response_mode": "fragment"}), "form_post mode": dict(extra={"response_mode": "form_post"}),
                 "unknown scope": dict(extra={"scope": "mcp admin"}), "request object": dict(extra={"request": "eyJ"})}.items():
    if "pkce" in name:
        u = authorize_url(CID, ch, "state-six").replace("code_challenge_method=S256", "code_challenge_method=plain")
        st = Browser().req("GET", u)[0]
    else:
        st = front(**kw)
    check(f"6 authorize: {name} is refused with 400", st == 400, st)
u = authorize_url(CID, ch, "state-six").replace("&code_challenge=" + ch, "")
check("6 authorize: no code_challenge is refused", Browser().req("GET", u)[0] == 400)
u = authorize_url(CID, ch, "state-six") + "&state=state-again"
check("6 authorize: a parameter twice is refused", Browser().req("GET", u)[0] == 400)
st, h, body, url = Browser().follow(authorize_url("no-such-client", ch, "state-six"))
check("6 authorize: an unknown client gets the error page, not a redirect to its address", st in (400, 401) and "chatgpt.com" not in url, (st, url))
st, h, body, url = Browser().follow(authorize_url(CID, ch, "state-six", redirect="https://evil.example.com/cb"))
check("6 authorize: another redirect address than the registered one is not followed", urllib.parse.urlparse(url).hostname == AUTH and st in (400, 401), (st, url))
# the same checks at the second step: a request that reached Hydra without our front is still refused at login/consent (unit tests); here the
# routes of Hydra that bypass the front do not exist
check("6 the public Gateway has no route to Hydra's own /oauth2/auth (only ours)", Browser().req("GET", authorize_url(CID, ch, "state-six"))[0] in (302, 400), "")

# ======================================================================================================================
# 7 · consent
bc = Browser(); kind, det = sign_in(bc, CID, "alice", consent=False)
st_, body_, url_, v_, state_ = det
check("7 consent page: the declared name, the redirect host, the person, tools unticked", "ChatGPT" in body_ and "chatgpt.com" in body_ and "alice@acme.test" in body_
      and 'value="time"' in body_ and not re.search(r"<input[^>]* checked", body_) and "frame-ancestors" in str(st_) + "frame-ancestors", body_[:300])
form = dict(re.findall(r'name="(challenge|csrf)" value="([^"]*)"', body_))
post = lambda data, hdr, br=bc: br.req("POST", base_url("/consent"), urllib.parse.urlencode(data).encode(), {"Content-Type": "application/x-www-form-urlencoded", **hdr})
n_before = len(console_state()["created"])
check("7 consent: no Origin header is refused", post({**form, "action": "allow"}, {})[0] == 403)
check("7 consent: another Origin is refused", post({**form, "action": "allow"}, {"Origin": "https://evil.example.com"})[0] == 403)
check("7 consent: a wrong CSRF token is refused", post({**form, "csrf": "AAAA", "action": "allow"}, {"Origin": ISS})[0] == 403)
check("7 consent: no CSRF cookie is refused", post({**form, "action": "allow"}, {"Origin": ISS}, Browser())[0] == 403)
check("7 consent: a tool that is not open to the person is refused", post({**form, "action": "allow", "server": "payroll"}, {"Origin": ISS})[0] == 400)
check("7 consent: nothing was created by the refusals", len(console_state()["created"]) == n_before)
s, h, t = post({**form, "action": "deny"}, {"Origin": ISS})
q = urllib.parse.parse_qs(urllib.parse.urlparse(h.get("Location", "")).query)
check("7 consent: Deny sends the platform access_denied", s == 302 and "oauth2/auth" in h.get("Location", ""), (s, h.get("Location")))
st, h, body, url = bc.follow(urllib.parse.urljoin(base_url("/"), h["Location"]))
q = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
check("7 consent: ... with the iss parameter", q.get("error") == ["access_denied"] and q.get("iss") == [ISS], url)
# Hydra keeps serving a rejected consent request, and would accept it again: Ambit decides each challenge once.
n_agents = len(console_state()["created"])
page = bc.req("GET", base_url("/consent?consent_challenge=" + form["challenge"]))
f2 = dict(re.findall(r'name="(challenge|csrf)" value="([^"]*)"', page[2]))
check("7 consent: a challenge that was decided cannot be decided again (no second agent)", not f2 or (post({**f2, "action": "allow", "server": "time"}, {"Origin": ISS})[0] == 400 and len(console_state()["created"]) == n_agents))
console_post("/_refuse?msg=you+have+10+agents+already")
kind, det = sign_in(Browser(), CID, "alice")
check("7 consent: when the console refuses the agent, the platform gets access_denied and nothing is issued", kind == "error" and det.get("error") == "access_denied", (kind, det))
console_post("/_refuse?msg=")

# ======================================================================================================================
# 8 · dynamic registration rules and limits
rules = {"unlisted host": "https://evil.example.com/cb", "subdomain of a listed host": "https://evil.chatgpt.com/cb", "listed host inside another": "https://chatgpt.com.evil.example.com/cb",
         "userinfo trick": "https://chatgpt.com@evil.example.com/cb", "http on a listed host": "http://chatgpt.com/cb", "fragment": "https://chatgpt.com/cb#x",
         "other port": "https://chatgpt.com:8443/cb", "custom scheme": "myapp://callback", "http outside loopback": "http://192.168.1.5/cb", "loopback lookalike": "http://127.0.0.1.evil.example.com/cb"}
for name, uri in rules.items():
    st, _, j = register(uri)
    check(f"8 registration: {name} is refused", st == 400 and j.get("error") == "invalid_client_metadata", (st, j))
for name, extra in {"confidential client": {"token_endpoint_auth_method": "client_secret_basic"}, "password grant": {"grant_types": ["password"]},
                    "implicit": {"response_types": ["token"]}, "foreign scope": {"scope": "mcp admin"}}.items():
    st, _, j = register(**extra)
    check(f"8 registration: {name} is refused", st == 400 and j.get("error") == "invalid_client_metadata", (st, j))
st, _, j = api("POST", "/register", {"client_name": "CLI", "redirect_uris": ["http://127.0.0.1:53211/callback"]})
check("8 registration: a native client on a loopback address is accepted", st == 201, (st, j))
big = Browser().req("POST", base_url("/register"), b"{" + b"a" * 20000, {"Content-Type": "application/json"})[0]
check("8 registration: a body that is too large or not JSON is refused", big == 400, big)
# ======================================================================================================================
# 9 · restarts: a sign-in that is open while ambit-auth restarts still completes (nothing is kept in the pods)
if os.environ.get("NO_RESTART"):
    print("skipped: 9 restart (no pods here)"); print("all checks passed" if not fails else "FAILED: " + ", ".join(fails)); sys.exit(1 if fails else 0)
time.sleep(1)
bro = Browser()
st_, h_, body_, url_ = bro.follow(authorize_url(CID, pkce()[1], "state-restart"))
kube("delete", "pod", "-l", "app.kubernetes.io/name=ambit-auth", "--wait=true")
kube("rollout", "status", "deploy/ambit-auth", "--timeout=180s")
wait(lambda: Browser().req("GET", base_url("/.well-known/oauth-authorization-server"))[0] == 200, 60)
fin = idp_pick(bro, body_, url_, "alice")
st, h, body, url = fin
check("9 the sign-in that was open during the restart of both ambit-auth pods reaches the consent page", urllib.parse.urlparse(url).path == "/consent" and st == 200, (st, url))

print()
print("FAILED: " + ", ".join(fails) if fails else "all checks passed")
sys.exit(1 if fails else 0)
