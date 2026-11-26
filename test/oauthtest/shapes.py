#!/usr/bin/env python3
"""The request shapes of docs/guides/oauth-mcp-client.md, each one sent as the guide shows it (test/stepoauth.sh check 19). Standard library only.

Every request that the guide documents has a marker `<!-- shape: NAME -->` in the guide and a `shape("NAME", ...)` here; the test fails when
the two lists differ, so a shape cannot be added to the guide without a test, nor a test be left behind.

Environment: as gateway.py, plus GUIDE (the path of the guide)."""
import os
src = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "gateway.py")).read().split("# 1 · no token")[0]
exec(src)

TESTED = []


def shape(name, cond, detail=""):
    check(f"19 shape {name}", cond, detail)
    TESTED.append(name)


def raw(method, url, data=None, headers=None, port=None):
    """One request, as curl would send it: (status, headers, body)."""
    ROUTES[(AUTH, 443)] = port or PORTS[0]; ROUTES[(MCP, 443)] = port or PORTS[0]
    st, h, body = Browser().req(method, url, data, headers)
    return st, {k.lower(): v for k, v in h.items()}, body


def js(body):
    try: return json.loads(body)
    except Exception: return {}


# 1 · a call without a token: the 401 that starts everything
st, h, body = raw("POST", MCP_URL, json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "shape-test", "version": "1"}}}).encode(),
                  {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
wa = h.get("www-authenticate", "")
shape("unauthenticated", st == 401 and wa.startswith("Bearer ") and f'resource_metadata="{PRM}"' in wa, (st, wa))

# 2 · protected resource metadata (RFC 9728)
st, h, body = raw("GET", PRM)
prm = js(body)
shape("protected-resource", st == 200 and prm.get("resource") == MCP_URL and prm.get("authorization_servers") == [ISS] and prm.get("bearer_methods_supported") == ["header"]
      and {"mcp", "offline_access"} <= set(prm.get("scopes_supported", [])), (st, prm))

# 3 · authorization server metadata (RFC 8414)
st, h, body = raw("GET", ISS + "/.well-known/oauth-authorization-server")
md = js(body)
shape("as-metadata", st == 200 and md.get("issuer") == ISS and md.get("authorization_endpoint") == ISS + "/oauth2/auth" and md.get("token_endpoint") == ISS + "/oauth2/token"
      and md.get("revocation_endpoint") == ISS + "/oauth2/revoke" and md.get("registration_endpoint") == ISS + "/register" and md.get("jwks_uri") == ISS + "/.well-known/jwks.json"
      and md.get("response_types_supported") == ["code"] and md.get("grant_types_supported") == ["authorization_code", "refresh_token"]
      and md.get("code_challenge_methods_supported") == ["S256"] and md.get("response_modes_supported") == ["query"] and md.get("authorization_response_iss_parameter_supported") is True
      and set(md.get("token_endpoint_auth_methods_supported", [])) >= {"none"} and md.get("scopes_supported") == ["mcp", "offline_access"], (st, md))

# 4 · dynamic client registration (RFC 7591)
st, h, body = raw("POST", ISS + "/register", json.dumps({"client_name": "My MCP client", "redirect_uris": [REDIRECT], "grant_types": ["authorization_code", "refresh_token"],
                                                         "response_types": ["code"], "token_endpoint_auth_method": "none"}).encode(), {"Content-Type": "application/json"})
reg = js(body)
CID = reg.get("client_id", "")
shape("register", st == 201 and CID and reg.get("redirect_uris") == [REDIRECT] and reg.get("token_endpoint_auth_method") == "none" and reg.get("grant_types") == ["authorization_code", "refresh_token"]
      and reg.get("response_types") == ["code"] and "client_secret" not in reg and isinstance(reg.get("client_id_issued_at"), int), (st, reg))
st, h, body = raw("POST", ISS + "/register", json.dumps({"client_name": "My MCP client", "redirect_uris": ["https://client.example.org/callback"]}).encode(), {"Content-Type": "application/json"})
e = js(body)
shape("register-refused", st == 400 and e.get("error") == "invalid_client_metadata" and e.get("error_description"), (st, e))

# 5 · the authorization request: what the browser is sent to
V, CH = pkce()
STATE = secrets.token_urlsafe(16)
q = urllib.parse.urlencode([("response_type", "code"), ("client_id", CID), ("redirect_uri", REDIRECT), ("code_challenge", CH), ("code_challenge_method", "S256"),
                            ("state", STATE), ("resource", MCP_URL), ("scope", "mcp offline_access")])
st, h, body = raw("GET", f"{ISS}/oauth2/auth?{q}")
shape("authorize", st == 302 and h.get("location", "").startswith(ISS + "/login?login_challenge="), (st, h.get("location")))
bad = {"no PKCE": [p for p in urllib.parse.parse_qsl(q) if p[0] not in ("code_challenge", "code_challenge_method")], "no resource": [p for p in urllib.parse.parse_qsl(q) if p[0] != "resource"],
       "another resource": [(k, "https://other.example.com/mcp" if k == "resource" else v) for k, v in urllib.parse.parse_qsl(q)],
       "plain PKCE": [(k, "plain" if k == "code_challenge_method" else v) for k, v in urllib.parse.parse_qsl(q)]}
res = {}
for name, params in bad.items():
    st, h, body = raw("GET", f"{ISS}/oauth2/auth?{urllib.parse.urlencode(params)}")
    res[name] = (st, js(body).get("error"))
shape("authorize-refused", all(v == (400, "invalid_request") for v in res.values()), res)

# 6 · the answer to the platform: a code with state and iss (the whole sign-in, as the person's browser does it)
b = Browser()
kind, det = sign_in(b, CID, "bob", challenge=CH, state=STATE)
shape("authorize-response", kind == "code" and det.get("state") == STATE and det.get("iss") == ISS and det["_url"].startswith(REDIRECT + "?") and det.get("code"), (kind, det))
CODE = det.get("code")
# ... and a refusal by the person: error=access_denied, also with iss
b2 = Browser()
kind2, det2 = sign_in(b2, CID, "bob", consent=False)
st, body2, url2, v2, state2 = det2
form = dict(re.findall(r'name="(challenge|csrf)" value="([^"]*)"', body2)); form["action"] = "deny"
st, h, t = b2.req("POST", base_url("/consent"), urllib.parse.urlencode(list(form.items())).encode(), {"Origin": ISS, "Content-Type": "application/x-www-form-urlencoded"})
st, h, t, url = b2.follow(urllib.parse.urljoin(base_url("/consent"), h["Location"]))
qq = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
shape("authorize-denied", url.startswith(REDIRECT + "?") and qq.get("error") == ["access_denied"] and qq.get("iss") == [ISS] and qq.get("state") == [state2], url)

# 7 · the token request
st, h, body = raw("POST", ISS + "/oauth2/token", urllib.parse.urlencode({"grant_type": "authorization_code", "code": CODE, "redirect_uri": REDIRECT, "client_id": CID,
                                                                          "code_verifier": V, "resource": MCP_URL}).encode(), {"Content-Type": "application/x-www-form-urlencoded"})
tok = js(body)
claims = jwt_claims(tok["access_token"]) if st == 200 else {}
shape("token-code", st == 200 and tok.get("token_type", "").lower() == "bearer" and 800 < tok.get("expires_in", 0) <= 900 and tok.get("refresh_token") and "mcp" in tok.get("scope", "")
      and tok["access_token"].count(".") == 2 and h.get("cache-control") == "no-store" and claims.get("iss") == ISS and MCP_URL in (claims["aud"] if isinstance(claims.get("aud"), list) else [claims.get("aud")]), (st, sorted(tok), claims))
st, h, body = raw("POST", ISS + "/oauth2/token", urllib.parse.urlencode({"grant_type": "authorization_code", "code": CODE, "redirect_uri": REDIRECT, "client_id": CID, "code_verifier": V}).encode(),
                  {"Content-Type": "application/x-www-form-urlencoded"})
e = js(body)
shape("token-error", st == 400 and e.get("error") == "invalid_grant" and e.get("error_description"), (st, e))  # the code was used once already
V9, CH9 = pkce(); kind9, det9 = sign_in(Browser(), CID, "bob", challenge=CH9)
st9, e9 = token(CID, grant_type="authorization_code", code=det9["code"], redirect_uri=REDIRECT, code_verifier=pkce()[0], resource=MCP_URL)  # a fresh code, another verifier
check("19 shape token-error: a fresh code with a verifier that is not the one of the request is refused (invalid_grant)", kind9 == "code" and st9 == 400 and e9.get("error") == "invalid_grant", (kind9, st9, e9))

# 8 · a call with the token
AT = tok.get("access_token")
st, names = tools(PORTS[0], AT)
shape("mcp-call", st == 200 and names and all(n.startswith("time_") for n in names), (st, names))
st, hd, data, t = mcp_post(PORTS[0], {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}, token="eyJhbGciOiJSUzI1NiJ9.e30.x")
shape("invalid-token", st == 401 and f'resource_metadata="{PRM}"' in hd.get("www-authenticate", ""), (st, hd.get("www-authenticate")))

# 9 · refresh: a new pair; the old refresh token is spent
RT1 = tok["refresh_token"]
st, h, body = raw("POST", ISS + "/oauth2/token", urllib.parse.urlencode({"grant_type": "refresh_token", "refresh_token": RT1, "client_id": CID}).encode(),
                  {"Content-Type": "application/x-www-form-urlencoded"})
t2 = js(body)
shape("refresh", st == 200 and t2.get("refresh_token") and t2["refresh_token"] != RT1 and t2.get("access_token") and t2["access_token"] != AT and 800 < t2.get("expires_in", 0) <= 900, (st, {k: v for k, v in t2.items() if k in ("error", "error_description", "expires_in")}))
RT2 = t2.get("refresh_token")
st, h, body = raw("POST", ISS + "/oauth2/token", urllib.parse.urlencode({"grant_type": "refresh_token", "refresh_token": RT1, "client_id": CID}).encode(), {"Content-Type": "application/x-www-form-urlencoded"})
e = js(body)
shape("refresh-reused", st == 400 and e.get("error") == "invalid_grant", (st, e))
st2, _ = token(CID, grant_type="refresh_token", refresh_token=RT2)
print(f"   after the spent refresh token was shown again, the newer one: HTTP {st2} (the guide says what a client must do)", flush=True)
SECOND_REFRESH_AFTER_REUSE = st2

# 10 · revocation (RFC 7009): a live refresh token of a grant of its own (the one above is spent or ended already)
V, CH = pkce(); kind, det = sign_in(Browser(), CID, "bob", challenge=CH)
st, tk = token(CID, grant_type="authorization_code", code=det["code"], redirect_uri=REDIRECT, code_verifier=V, resource=MCP_URL)
RT3 = tk["refresh_token"]
st0, _, _ = raw("POST", ISS + "/oauth2/revoke", urllib.parse.urlencode({"token": "ory_rt_made-up", "token_type_hint": "refresh_token", "client_id": CID}).encode(), {"Content-Type": "application/x-www-form-urlencoded"})
st, h, body = raw("POST", ISS + "/oauth2/revoke", urllib.parse.urlencode({"token": RT3, "token_type_hint": "refresh_token", "client_id": CID}).encode(), {"Content-Type": "application/x-www-form-urlencoded"})
st2, e2 = token(CID, grant_type="refresh_token", refresh_token=RT3)
shape("revoke", st == 200 and st2 == 400 and e2.get("error") == "invalid_grant", (st, st2, e2))

# 11 · the keys, for a client that checks the token itself
st, h, body = raw("GET", ISS + "/.well-known/jwks.json")
ks = js(body).get("keys", [])
shape("jwks", st == 200 and len(ks) >= 1 and all(k.get("kty") == "RSA" and k.get("kid") and k.get("alg") == "RS256" and "d" not in k for k in ks), (st, [sorted(k) for k in ks]))

# the guide and this test must name the same shapes
guide = open(os.environ["GUIDE"]).read()
named = re.findall(r"<!-- shape: ([a-z0-9-]+) -->", guide)
check("19 the guide names every shape once, and every shape of the guide is tested here", sorted(named) == sorted(TESTED) and len(set(named)) == len(named), (sorted(set(named) ^ set(TESTED)), named))

print()
print("FAILED: " + ", ".join(fails) if fails else "all checks passed")
sys.exit(1 if fails else 0)
