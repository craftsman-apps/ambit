#!/usr/bin/env python3
"""End-to-end checks of tokens at the public Gateway (test/stepoauth.sh runs this after flow.py). Standard library only.

A platform signs in as a person (the whole sign-in, with the real console behind ambit-auth), gets a token, and calls the MCP route of
the public Gateway. What is checked: the 401 with the resource metadata challenge, the metadata document, tools and calls with the
consented scope and nothing else, forged and changed tokens, offboarding, the emergency block (also with Hydra down), API keys on the same
route, and a restart of the console.

Environment: as flow.py, plus GW_PORTS (comma separated local ports of the public Gateway pods), KEY (a valid MCP API key),
ONE_TOOL (the tool name that the front-door server has) and KUBECTL."""
import base64, json, os, sys, time, urllib.parse
src = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "flow.py")).read().split("# 1 · discovery and registration")[0]
sys.argv = sys.argv[:1]
exec(src)

PORTS = [int(p) for p in os.environ["GW_PORTS"].split(",")]
KEY = os.environ["KEY"]
TOOL = os.environ["ONE_TOOL"]
MCP_URL = f"https://{MCP}/mcp"
PRM = f"https://{MCP}/.well-known/oauth-protected-resource/mcp"


def at(port):
    ROUTES[(MCP, 443)] = port


def mcp_post(port, body, token=None, key=None, sid=None, extra=None):
    at(port)
    h = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if token: h["Authorization"] = "Bearer " + token
    if key: h["x-api-key"] = key
    if sid: h["Mcp-Session-Id"] = sid
    h.update(extra or {})
    st, hd, t = Browser().req("POST", MCP_URL, json.dumps(body).encode(), h)
    data = None
    for line in t.splitlines():
        if line.startswith("data: "): data = json.loads(line[6:]); break
    if data is None and t.strip().startswith("{"):
        try: data = json.loads(t)
        except Exception: pass
    return st, {k.lower(): v for k, v in hd.items()}, data, t


def open_session(port, token=None, key=None):
    st, hd, data, t = mcp_post(port, {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "gateway.py", "version": "1"}}}, token, key)
    if st != 200: return st, None, hd
    sid = hd.get("mcp-session-id")
    mcp_post(port, {"jsonrpc": "2.0", "method": "notifications/initialized"}, token, key, sid)
    return 200, sid, hd


def tools(port, token=None, key=None):
    """(status, tool names): a new session and a tool list."""
    st, sid, hd = open_session(port, token, key)
    if st != 200: return st, None
    st, _, data, t = mcp_post(port, {"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, token, key, sid)
    if st != 200 or not data or "result" not in data: return st, None
    return 200, sorted(x["name"] for x in data["result"]["tools"])


def call_tool(port, token, name, args):
    st, sid, hd = open_session(port, token)
    if st != 200: return st, None
    st, _, data, t = mcp_post(port, {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": name, "arguments": args}}, token, None, sid)
    return st, data


def connect(user, tick, name="ChatGPT"):
    """The whole sign-in as a person; returns the token set. A new client each time keeps the grants apart."""
    st, _, reg = register(REDIRECT, name)
    cid = reg["client_id"]
    kind, res = sign_in(Browser(), cid, user, tick=tick)
    if kind != "code": raise RuntimeError(f"sign-in of {user}: {kind} {res}")
    st, tok = exchange(cid, res)
    if st != 200: raise RuntimeError(f"token of {user}: {st} {tok}")
    return cid, tok


def accepted_everywhere(token, secs=60):
    return wait(lambda: all(tools(p, token)[0] == 200 for p in PORTS), secs, 1)


# ---------------------------------------------------------------------------------------------------------------------
# 1 · no token, the metadata, the first token after the install
t0 = time.time()
for p in PORTS:
    st, hd, data, t = mcp_post(p, {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}})
    wa = hd.get("www-authenticate", "")
    check(f"1 no token on pod {p}: 401 with the resource metadata challenge", st == 401 and 'resource_metadata="' + PRM + '"' in wa and wa.lower().startswith("bearer"), (st, wa))
at(PORTS[0])
st, hd, body = Browser().req("GET", PRM)
try: prm = json.loads(body)
except Exception: prm = {}
check("1 the resource metadata document: this MCP URL, Ambit's sign-in host as the authorization server", st == 200 and prm.get("resource") == MCP_URL and prm.get("authorization_servers") == [ISS], (st, body[:300]))
cid1, tok1 = connect("alice", ("time",))
claims1 = jwt_claims(tok1["access_token"])
check("1 the token carries the verified email (AccessRules by email need it) and the person's agent and grant", claims1.get("email_verified") is True and claims1.get("email") == "alice@acme.test" and claims1.get("ambit_agent") and claims1.get("ambit_grant"), sorted(claims1))
ok = accepted_everywhere(tok1["access_token"], 400)
first_ok = time.time() - t0
check("1 the first token is accepted on every public pod (the gateway has Hydra's key; no five-minute refusal)", ok and first_ok < 240, f"first success after {first_ok:.0f} s")
print(f"   first token accepted everywhere after {first_ok:.0f} s", flush=True)

# ---------------------------------------------------------------------------------------------------------------------
# 2 · tools: exactly the consented scope, on every pod, and each call is checked
for p in PORTS:
    st, names = tools(p, tok1["access_token"])
    check(f"2 pod {p}: the consented server's tools, and no other server's", st == 200 and TOOL in (names or []) and all(n.startswith("time_") for n in names), (st, names))
    st, data = call_tool(p, tok1["access_token"], TOOL, {"timezone": "UTC"})
    check(f"2 pod {p}: a call of the consented tool works", st == 200 and data and "result" in data and not data["result"].get("isError"), (st, data))
cid2, tok2 = connect("alice", (), "Claude")  # a second person-mode agent of the same person, with an empty scope
claims2 = jwt_claims(tok2["access_token"])
check("2 two connections of one person are two agents with two grants", claims2["ambit_agent"] != claims1["ambit_agent"] and claims2["ambit_grant"] != claims1["ambit_grant"] and claims2["ambit_scope"] == [], (claims1, claims2))
accepted_everywhere(tok2["access_token"], 120)
for p in PORTS:
    st, names = tools(p, tok2["access_token"])
    check(f"2 pod {p}: an empty scope lists no tool", st == 200 and names == [], (st, names))
    st, data = call_tool(p, tok2["access_token"], TOOL, {"timezone": "UTC"})
    refused = st != 200 or data is None or "error" in data or data.get("result", {}).get("isError")
    check(f"2 pod {p}: the same tool, called with an empty scope, is refused", refused, (st, data))
    st, data = call_tool(p, tok1["access_token"], "everything_echo", {"message": "x"})
    check(f"2 pod {p}: a tool of a server that is not on the front door is not there", st != 200 or data is None or "error" in data or data.get("result", {}).get("isError"), (st, data))

# ---------------------------------------------------------------------------------------------------------------------
# 3 · tokens that are not Ambit's, or not whole
def tamper(tok, **claims):
    h, p, s = tok.split(".")
    pad = p + "=" * (-len(p) % 4)
    c = json.loads(base64.urlsafe_b64decode(pad)); c.update(claims)
    return h + "." + base64.urlsafe_b64encode(json.dumps(c).encode()).rstrip(b"=").decode() + "." + s
good = tok1["access_token"]
h, p, s = good.split(".")
bad = {"the scope changed (signature no longer fits)": tamper(good, ambit_scope=["time", "everything"]),
       "another audience": tamper(good, aud=["https://other.example.com/mcp"]),
       "another issuer": tamper(good, iss="https://evil.example.com"),
       "no signature": h + "." + p + ".",
       "alg none": base64.urlsafe_b64encode(b'{"alg":"none","typ":"JWT"}').rstrip(b"=").decode() + "." + p + ".",
       "not a token": "abc.def.ghi", "empty bearer": ""}
for name, t in bad.items():
    codes = [mcp_post(pt, {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}, token=t or None, extra={"Authorization": "Bearer " + t} if t == "" else None)[0] for pt in PORTS]
    check(f"3 {name}: refused with 401 on every pod", all(c == 401 for c in codes), codes)
# a key that is not valid goes the key way: 403, no challenge; a valid key works on the same route
st, hd, data, t = mcp_post(PORTS[0], {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}, key="ambit_" + "x" * 40)
check("3 a wrong key is 403, not a token challenge", st == 403 and "www-authenticate" not in hd, (st, hd.get("www-authenticate")))
for p in PORTS:
    st, names = tools(p, key=KEY)
    check(f"3 pod {p}: a valid API key still works on the same route", st == 200 and TOOL in (names or []), (st, names))
st, hd, data, t = mcp_post(PORTS[0], {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}, token=good, key="ambit_" + "x" * 40)
check("3 a token and a wrong key together: the key rule decides (403)", st == 403, st)

# ---------------------------------------------------------------------------------------------------------------------
# 4 · offboarding: the gateway refuses the person's token at once, on every pod
def refused_everywhere(token, secs):
    t = time.time()
    seen = {}
    while time.time() - t < secs:
        for p in PORTS:
            if p not in seen and tools(p, token)[0] in (401, 403):
                seen[p] = time.time() - t
        if len(seen) == len(PORTS): return seen
        time.sleep(0.2)
    return seen
kube("create", "configmap", "ambit-offboarded", "--from-literal=emails=[\"alice@acme.test\"]")
t_off = time.time()
seen = refused_everywhere(good, 60)
check("4 offboarding: the person's token is refused on every pod", len(seen) == len(PORTS), seen)
print(f"   offboarding refused the token on every pod after {max(seen.values()) if seen else float('nan'):.1f} s", flush=True)
kube("delete", "configmap", "ambit-offboarded", "--ignore-not-found")
check("4 reactivated: the same token works again", wait(lambda: all(tools(p, good)[0] == 200 for p in PORTS), 60, 1))

# ---------------------------------------------------------------------------------------------------------------------
# 5 · the emergency block: every token of the sign-in host at once, keys untouched, also when Hydra is down
POL = "agentgatewaypolicy/ambit-mcp-public-oauth"
block = '[{"op":"add","path":"/spec/traffic/authorization/policy/matchExpressions/-","value":"false"}]'
def timed_block(label):
    t = time.time()
    kube("patch", POL, "--type", "json", "-p", block)
    applied = time.time() - t
    seen = refused_everywhere(good, 60)
    return applied, seen
applied, seen = timed_block("block")
check("5 emergency block: every token is refused on every pod (the policy edit, no Helm run)", len(seen) == len(PORTS), seen)
check("5 emergency block: a token of the second connection too, whatever it is", all(tools(p, tok2["access_token"])[0] in (401, 403) for p in PORTS))
check("5 emergency block: API keys keep working on the same route", all(tools(p, key=KEY)[0] == 200 for p in PORTS))
print(f"   emergency block: the edit took {applied:.2f} s; refused on every pod after {max(seen.values()) if seen else float('nan'):.1f} s (counted from the end of the edit)", flush=True)
# lift it (remove the last clause), then block again with Hydra's key endpoint down and the old key cached on the gateways
n = len(json.loads(kube("get", POL, "-o", "json"))["spec"]["traffic"]["authorization"]["policy"]["matchExpressions"])
kube("patch", POL, "--type", "json", "-p", '[{"op":"remove","path":"/spec/traffic/authorization/policy/matchExpressions/%d"}]' % (n - 1))
check("5 lifted: tokens work again", wait(lambda: all(tools(p, good)[0] == 200 for p in PORTS), 60, 1))
kube("scale", "statefulset/ambit-hydra", "--replicas=0")
wait(lambda: kube("get", "pods", "-l", "app.kubernetes.io/name=ambit-hydra", "-o", "name") == "", 120, 2)
check("5 Hydra is down: tokens that the gateway can still check (its copy of the keys) keep working", all(tools(p, good)[0] == 200 for p in PORTS))
applied, seen = timed_block("block with Hydra down")
check("5 emergency block with Hydra down and the old key cached: every token is refused on every pod", len(seen) == len(PORTS), seen)
print(f"   emergency block with Hydra down: the edit took {applied:.2f} s; refused on every pod after {max(seen.values()) if seen else float('nan'):.1f} s", flush=True)
check("5 ... and API keys keep working", all(tools(p, key=KEY)[0] == 200 for p in PORTS))
kube("patch", POL, "--type", "json", "-p", '[{"op":"remove","path":"/spec/traffic/authorization/policy/matchExpressions/%d"}]' % (n - 1))
kube("scale", "statefulset/ambit-hydra", "--replicas=2")
kube("rollout", "status", "statefulset/ambit-hydra", "--timeout=300s")
check("5 Hydra back and the block lifted: tokens work", wait(lambda: all(tools(p, good)[0] == 200 for p in PORTS), 90, 1))

# ---------------------------------------------------------------------------------------------------------------------
# 6 · the console restarts: tokens in use keep working (the gateway does not ask the console); a refresh waits for it
st, tokr = token(cid1, grant_type="refresh_token", refresh_token=tok1["refresh_token"])
check("6 before the restart: a refresh works", st == 200 and tokr.get("access_token"), (st, tokr))
kube("scale", "deploy/ambit-console", "--replicas=0")
wait(lambda: kube("get", "pods", "-l", "app.kubernetes.io/name=ambit-console", "-o", "name") == "", 120, 2)
check("6 console down: tokens in use keep working at the gateway", all(tools(p, tokr["access_token"])[0] == 200 for p in PORTS))
st, j = token(cid1, grant_type="refresh_token", refresh_token=tokr["refresh_token"])
check("6 console down: a refresh is refused for now (fail closed, within a minute), and does not use the refresh token up", (st == 0 or st >= 400) and "access_token" not in j, (st, j))
kube("scale", "deploy/ambit-console", "--replicas=1")
kube("rollout", "status", "deploy/ambit-console", "--timeout=300s")
ok = wait(lambda: token(cid1, grant_type="refresh_token", refresh_token=tokr["refresh_token"])[0] == 200, 120, 3)
check("6 console back: the same refresh token works", ok)

# ---------------------------------------------------------------------------------------------------------------------
# 7 · stop: the production path (the console's stop API, the stop list, the real controller, the gateway's policy), measured. The agent of
# one connection is stopped: its token is refused on every pod, its refresh is refused, the person's other connection keeps working, and the
# agent cannot be started again before the retention is over (an access token lives up to 15 minutes).
STOPCTL = os.environ.get("STOPCTL")
if STOPCTL:
    def ctl(*a):
        r = subprocess.run([STOPCTL, *a], capture_output=True, text=True, timeout=180)
        return r.stdout.strip().splitlines()[-1] if r.stdout.strip() else ""
    cidA, tokA = connect("alice", ("time",), "StopMe")
    cidB, tokB = connect("alice", ("time",), "KeepMe")
    agentA = jwt_claims(tokA["access_token"])["ambit_agent"]
    check("7 two connections, two agents", agentA != jwt_claims(tokB["access_token"])["ambit_agent"])
    check("7 both tokens work on every pod before the stop", accepted_everywhere(tokA["access_token"], 120) and accepted_everywhere(tokB["access_token"], 120))
    t_stop = time.time()
    code = ctl("stop", agentA)
    check("7 the console accepts the stop request (202)", code == "202", code)
    seen = {}
    while time.time() - t_stop < 120 and len(seen) < len(PORTS):
        for p in PORTS:
            if p not in seen and tools(p, tokA["access_token"])[0] in (401, 403):
                seen[p] = time.time() - t_stop
        time.sleep(0.1)
    check("7 stop: the stopped agent's token is refused on every pod", len(seen) == len(PORTS), seen)
    t_gw = max(seen.values()) if seen else float("nan")
    t_r = time.time()
    refused = False
    while time.time() - t_r < 60:
        st, j = token(cidA, grant_type="refresh_token", refresh_token=tokA["refresh_token"])
        if st in (400, 401, 403) and "access_token" not in j:
            refused = True
            break
        time.sleep(0.5)
    t_ref = time.time() - t_stop
    check("7 stop: the refresh of the stopped agent's grant is refused", refused)
    check("7 stop: the person's other connection still works on every pod", all(tools(p, tokB["access_token"])[0] == 200 for p in PORTS))
    st, j = token(cidB, grant_type="refresh_token", refresh_token=tokB["refresh_token"])
    check("7 stop: the other connection can refresh", st == 200 and j.get("access_token"), (st, j))
    state, t_state = "", None
    while time.time() - t_stop < 240:
        state = ctl("state", agentA)
        if state == "stopped":
            t_state = time.time() - t_stop
            break
        time.sleep(1)
    check("7 stop: the console reports the agent stopped (every Gateway acknowledged)", state == "stopped", state)
    print(f"   stop measured: token refused on every pod {t_gw:.1f} s, refresh refused {t_ref:.1f} s, console says stopped {t_state if t_state is None else round(t_state, 1)} s (counted from the stop request)", flush=True)
    code = ctl("start", agentA)
    check("7 start: refused for now (409), the retention is not over", code == "409", code)
    kube("rollout", "restart", "deploy/ambit-controller")
    kube("rollout", "status", "deploy/ambit-controller", "--timeout=300s")
    check("7 after a restart of the controller the stopped agent's token is still refused on every pod", all(tools(p, tokA["access_token"])[0] in (401, 403) for p in PORTS))
    check("7 ... and the other connection still works", all(tools(p, tokB["access_token"])[0] == 200 for p in PORTS))

print()
print("FAILED: " + ", ".join(fails) if fails else "all checks passed")
sys.exit(1 if fails else 0)
