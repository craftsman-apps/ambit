#!/usr/bin/env python3
"""End-to-end checks of the rate-limit buckets of the public Gateway (test/stepoauth.sh check 18 runs this, twice). Standard library only.

The Gateway is in PROXY protocol mode (frontDoor.clientAddress=proxyProtocol), so this test can be any client address it likes, as a load
balancer would tell the Gateway. What is checked (MINOR 3 of the design review, A16):
  buckets   (low limits) each bucket (discovery, sign-in, token, registration) stops at its own limit and leaves the other three open; the 401
            for a call to the MCP URL without a credential is in the discovery bucket, a call with a credential is not; headers that name another
            address (X-Forwarded-For, X-Real-IP, Forwarded, X-Ambit-Client-Address) change nothing, and a caller behind another address that writes
            the limited address into them is not limited; two Gateway pods count together (the limit is not doubled)
  second    (low limits, the ratelimit service stopped) ambit-auth's own counter stops the sign-in pages at the same limit
  nat       (default limits) one shared address: how many whole sign-ins it makes in one minute before the limit, how many requests a sign-in
            costs in each bucket, and that another address is not affected

Environment: as gateway.py, plus LIM_DISCOVERY, LIM_SIGNIN, LIM_TOKEN, LIM_REGISTRATION (the limits the chart was installed with) and the mode as
the first argument."""
import os, sys
ARGV = sys.argv[:]  # the code below clears sys.argv
src = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "gateway.py")).read().split("# 1 · no token")[0]
exec(src)

MODE = ARGV[1]
LIM_MCP = int(os.environ.get("LIM_MCP", "0"))
LIM_GLOBAL = int(os.environ.get("LIM_GLOBAL", "0"))
LIM_CRED = int(os.environ.get("LIM_CRED", "0"))
OPEN = {200, 201, 400, 401, 403}  # what a request that is not limited gets here: never 404 (a route is missing) or 5xx
LIM = {"discovery": int(os.environ["LIM_DISCOVERY"]), "signin": int(os.environ["LIM_SIGNIN"]), "token": int(os.environ["LIM_TOKEN"]), "registration": int(os.environ["LIM_REGISTRATION"])}

# what one request of each bucket looks like, and the status it gets when it is NOT limited
SHAPES = {
    "discovery": ("GET", "/.well-known/oauth-authorization-server", None, {}),
    "signin": ("GET", "/error", None, {}),
    "token": ("POST", "/oauth2/token", {"grant_type": "refresh_token", "client_id": "limits-test", "refresh_token": "ory_rt_made-up"}, {}),
    "registration": ("POST", "/register", json.dumps({"client_name": "x", "redirect_uris": ["https://evil.example.com/cb"]}).encode(), {"Content-Type": "application/json"}),
}
SPOOF = {"X-Forwarded-For": "198.51.100.9", "X-Real-IP": "198.51.100.9", "Forwarded": "for=198.51.100.9", "X-Ambit-Client-Address": "198.51.100.9"}
GW_ANSWER = {}  # the first 429 of each bucket: which layer answered


def send(addr, bucket, port=None, extra=None):
    """One request of a bucket from the made-up address addr. Returns the HTTP status."""
    global CLIENT_ADDR
    CLIENT_ADDR = addr
    ROUTES[(AUTH, 443)] = port or PORTS[0]
    m, path, data, hd = SHAPES[bucket]
    st, h, body = Browser().req(m, base_url(path), data, {**hd, **(extra or {})})
    if st == 429 and bucket not in GW_ANSWER:
        GW_ANSWER[bucket] = "ambit-auth" if h.get("Retry-After") else "the Gateway"
    return st


def mcp_no_credential(addr, port=None, token=None):
    global CLIENT_ADDR
    CLIENT_ADDR = addr
    ROUTES[(MCP, 443)] = port or PORTS[0]
    hd = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if token: hd["Authorization"] = "Bearer " + token
    return Browser().req("POST", f"https://{MCP}/mcp", json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}).encode(), hd)[0]


def mcp_status(addr, key=None, token=None):
    """One MCP initialize with a key or a token from addr: 200, or 429 when limited (an MCP request over a limit may also answer 200 with a
    JSON-RPC error that says 'rate limit': that is reported as 429)."""
    global CLIENT_ADDR
    CLIENT_ADDR = addr
    ROUTES[(MCP, 443)] = PORTS[0]
    hd = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if key: hd["x-api-key"] = key
    if token: hd["Authorization"] = "Bearer " + token
    st, h, body = Browser().req("POST", f"https://{MCP}/mcp", json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "limits", "version": "1"}}}).encode(), hd)
    return 429 if st == 200 and "rate limit" in body.lower() else st


def start_of_window(unit="minute"):
    """The limits count in fixed windows of the clock: start a burst early in one."""
    t = time.gmtime()
    if unit == "minute" and t.tm_sec > 20: time.sleep(62 - t.tm_sec)
    if unit == "hour" and t.tm_min >= 56: time.sleep(3700 - t.tm_min * 60 - t.tm_sec)


def minute_now(): return int(time.time() // 60)


def burst(fn, n, unit="minute"):
    """n calls of fn(i) in one window of the clock; returns the statuses. Run again when a window ended in the middle."""
    for _ in range(3):
        start_of_window(unit)
        w0 = minute_now() if unit == "minute" else int(time.time() // 3600)
        codes = [fn(i) for i in range(n)]
        w1 = minute_now() if unit == "minute" else int(time.time() // 3600)
        if w0 == w1: return codes
    raise RuntimeError("a burst always crossed a window")


def allowed(codes): return sum(1 for c in codes if c != 429)


if MODE == "buckets":
    names = ["discovery", "signin", "token", "registration"]
    for i, b in enumerate(names):
        addr = f"203.0.113.{10 + i}"
        n = LIM[b] + 8
        codes = burst(lambda _: send(addr, b), n, "hour" if b == "registration" else "minute")
        a = allowed(codes)
        check(f"18 {b} bucket: {n} requests from one address, {a} passed and the rest got 429 (limit {LIM[b]})", a == LIM[b] and codes[LIM[b]:] == [429] * 8 and set(codes[:LIM[b]]) <= OPEN, codes)
        others = {o: send(addr, o) for o in names if o != b}
        check(f"18 {b} bucket used up: the other three buckets of the same address are open", all(c != 429 for c in others.values()), others)
    print("   the first 429 of each bucket came from: " + ", ".join(f"{k}: {v}" for k, v in GW_ANSWER.items()), flush=True)
    check("18 every bucket is stopped by the Gateway (the shared counter of the ratelimit service), not only by ambit-auth", set(GW_ANSWER.values()) == {"the Gateway"}, GW_ANSWER)

    # the 401 of a call without a credential is in the discovery bucket; a call with a credential is not
    addr = "203.0.113.40"
    codes = burst(lambda _: mcp_no_credential(addr), LIM["discovery"] + 5)
    check(f"18 the MCP URL without a credential: 401 {allowed(codes)} times, then 429 (the discovery bucket, limit {LIM['discovery']})", allowed(codes) == LIM["discovery"] and set(codes[:LIM["discovery"]]) == {401} and codes[LIM["discovery"]:] == [429] * 5, sorted(set(codes)))
    check("18 ... and a call with a (wrong) token from the same address is not limited: it is in the mcp bucket", mcp_no_credential(addr, token="not-a-token") == 401)
    check("18 ... nor are the sign-in pages of that address", send(addr, "signin") != 429)

    # an address in a header changes nothing
    A = "203.0.113.55"  # used up and then written to in one window (the bursts above may have taken longer than a minute)
    n = LIM["signin"] + 2
    mixed = burst(lambda i: send(A, "signin") if i < n else send(A, "signin", extra={**SPOOF, "X-Forwarded-For": f"198.51.100.{i}"}), n + 6)
    spoofed = mixed[n:]
    check("18 spoofed X-Forwarded-For, X-Real-IP, Forwarded and X-Ambit-Client-Address do not free a limited address", allowed(mixed) == LIM["signin"] and set(spoofed) == {429}, mixed)
    B = "203.0.113.50"
    claims = burst(lambda i: send(B, "signin", extra={"X-Forwarded-For": A, "X-Real-IP": A, "Forwarded": f"for={A}", "X-Ambit-Client-Address": A}), LIM["signin"] + 6)
    check(f"18 another address that writes the limited address into those headers is counted as itself: {allowed(claims)} passed (limit {LIM['signin']}), not 0", allowed(claims) == LIM["signin"], claims)
    C = "203.0.113.51"
    fresh = burst(lambda i: send(C, "signin", extra={"X-Forwarded-For": f"198.51.100.{i % 250}", "X-Ambit-Client-Address": f"198.51.100.{i % 250}"}), LIM["signin"] + 6)
    check(f"18 a new made-up address in a header on every request gives no extra budget: {allowed(fresh)} passed (limit {LIM['signin']})", allowed(fresh) == LIM["signin"], fresh)

    # two Gateway pods count together
    D = "203.0.113.52"
    # the token bucket: only the Gateway counts it (ambit-auth does not see /oauth2/token), so shared counters are the Gateway's
    two = burst(lambda i: send(D, "token", port=PORTS[i % 2]), LIM["token"] + 12)
    check(f"18 two Gateway pods count together: {len(two)} token requests alternating between them, {allowed(two)} passed (limit {LIM['token']}, not {2 * LIM['token']})", allowed(two) == LIM["token"] and set(two[:LIM["token"]]) <= OPEN, two)
    both = [send(D, "token", port=p) for p in PORTS]
    check("18 ... and the address is limited on both pods", both == [429, 429], both)
    # path spellings that a router may read as /oauth2/token are never a way around the token bucket: if one reaches Hydra it is counted as a token request
    E = "203.0.113.53"
    def alias(i):
        global CLIENT_ADDR
        CLIENT_ADDR = E; ROUTES[(AUTH, 443)] = PORTS[0]
        return Browser().req("POST", f"https://{AUTH}/oauth2/%74oken", urllib.parse.urlencode({"grant_type": "refresh_token", "client_id": "limits-test", "refresh_token": "x"}).encode(), {"Content-Type": "application/x-www-form-urlencoded"})[0]
    al = burst(alias, LIM["token"] + 10)
    check("18 a percent-encoded spelling of /oauth2/token is either not served, or counted in the token bucket (no way around the limit)", not any(c in (400, 401) for c in al) or 429 in al[: LIM["token"] + 2], al)
    # the fifth bucket: calls with a key or token (mcp), and the count per credential (an agent), not one counter for every token
    if LIM_MCP:
        F = "203.0.113.54"
        calls = burst(lambda i: mcp_status(F, key=os.environ["KEY"]), LIM_MCP + 8)
        check(f"18 mcp bucket: {len(calls)} calls with a valid key from one address, {allowed(calls)} passed (limit {LIM_MCP})", allowed(calls) == LIM_MCP and set(calls[:LIM_MCP]) <= {200}, calls)
    if LIM_CRED:
        CLIENT_ADDR = "203.0.115.1"  # a fresh address for the registrations and sign-ins
        ROUTES[(AUTH, 443)] = PORTS[0]
        _, tA = connect("person150", ("time",)); _, tB = connect("person151", ("time",))
        res = []
        for tok in (tA, tB):
            codes = burst(lambda i: mcp_status(f"203.0.114.{(i + 1) % 250 + 1}", token=tok["access_token"]), LIM_CRED + 8)  # a new source address for every call: only the credential count can stop it
            res.append(codes)
        check(f"18 per credential: a token's calls stop at the limit of {LIM_CRED} a minute for its agent, and another agent's token has its own count ({allowed(res[0])} and {allowed(res[1])} passed)",
              all(allowed(c) == LIM_CRED for c in res), [sorted(set(c)) for c in res])
    # N8: the bucket is chosen by the host too. Sign-in paths on the MCP host (a 404 there) stay in the mcp bucket: a flood there neither spreads over the
    # sign-in and token buckets of the same address, nor is counted as sign-in traffic.
    if LIM_MCP:
        H = "203.0.113.56"
        def on_mcp_host(i):
            global CLIENT_ADDR
            CLIENT_ADDR = H; ROUTES[(MCP, 443)] = PORTS[0]
            path = ["/oauth2/token", "/login", "/register"][i % 3]
            return Browser().req("POST" if path in ("/oauth2/token", "/register") else "GET", f"https://{MCP}{path}", b"x=y" if path in ("/oauth2/token", "/register") else None, {"Content-Type": "application/x-www-form-urlencoded"})[0]
        flood = burst(on_mcp_host, LIM_MCP + 10)
        check(f"18 sign-in paths on the MCP host: {allowed(flood)} of {len(flood)} passed (the mcp limit {LIM_MCP}), then 429: they count in the mcp bucket", allowed(flood) == LIM_MCP and set(flood[:LIM_MCP]) == {404}, sorted(set(flood)))
        after = {b: send(H, b) for b in ("discovery", "signin", "token")}
        check("18 ... and the sign-in host's discovery, sign-in and token buckets of the same address are still open (no spreading)", all(c != 429 for c in after.values()), after)
        # a host name in other letters is the same host: it must not be a way to another bucket
        U = "203.0.113.57"
        def upper(i):
            global CLIENT_ADDR
            CLIENT_ADDR = U; ROUTES[(AUTH, 443)] = PORTS[0]
            return Browser().req("GET", base_url("/error"), None, {"Host": AUTH.upper()})[0]
        up = burst(upper, LIM["signin"] + 6)
        served = [c for c in up if c not in (404, 429)]
        check(f"18 the sign-in host written in capital letters is either not served (404: the route matches the host name exactly) or counted as the sign-in host: {len(served)} of {len(up)} reached a page, {allowed(up)} passed", not served or allowed(up) == LIM["signin"], sorted(set(up)))
    # N2: IPv6 registrations count by the /48 in ambit-auth: many /64 networks of one /48 share the 10 (here 5) an hour; another /48 has its own
    def reg6(net, i):
        global CLIENT_ADDR
        CLIENT_ADDR = f"2001:db8:{net}:{i:x}::1"; ROUTES[(AUTH, 443)] = PORTS[0]
        st, h, body = Browser().req("POST", base_url("/register"), SHAPES["registration"][2], SHAPES["registration"][3])
        if st == 429: GW_ANSWER["reg6"] = "ambit-auth" if h.get("Retry-After") else "the Gateway"
        return st
    six = burst(lambda i: reg6(0x48, i + 1), LIM["registration"] + 7, "hour")
    check(f"18 IPv6: {len(six)} registrations from {len(six)} different /64 networks of one /48: {allowed(six)} passed (limit {LIM['registration']}), then 429", allowed(six) == LIM["registration"] and six[LIM["registration"]:] == [429] * 7, six)
    check("18 ... and the 429 is ambit-auth's (the Gateway counts the single addresses, and each had one request)", GW_ANSWER.get("reg6") == "ambit-auth", GW_ANSWER)
    check("18 ... another /48 registers", reg6(0x49, 1) != 429)
    # N2: the global limit of accepted registrations, last (it uses the quota that the sign-ins above need)
    if LIM_GLOBAL:
        got = []
        for i in range(LIM_GLOBAL + 6):
            CLIENT_ADDR = f"203.0.116.{i + 1}"; ROUTES[(AUTH, 443)] = PORTS[0]
            st, h, body = Browser().req("POST", base_url("/register"), json.dumps({"client_name": "g", "redirect_uris": [REDIRECT]}).encode(), {"Content-Type": "application/json"})
            got.append((st, h.get("Retry-After")))
        codes = [c for c, _ in got]
        first429 = codes.index(429) if 429 in codes else -1
        check(f"18 global registration limit: registrations from {len(got)} different addresses: {codes.count(201)} created (limit {LIM_GLOBAL} an hour for everybody), then 429 with Retry-After 3600", 1 <= codes.count(201) <= LIM_GLOBAL and first429 > 0 and all(c == 429 for c in codes[first429:]) and got[first429][1] == "3600", got)
    # ambit-auth's second line: its own counter in Valkey, shared by its replicas, with the same numbers (seen in its metrics by the shell script)

elif MODE == "second":
    # The ratelimit service is down (FailOpen): the Gateway lets requests through uncounted, and ambit-auth's own counter (Valkey, shared by its replicas,
    # the same numbers) is the second line.
    addr = "203.0.113.60"
    codes = burst(lambda i: send(addr, "signin", port=PORTS[i % 2]), LIM["signin"] + 6)
    check(f"18 ratelimit service down: ambit-auth stops the sign-in pages at the same limit ({allowed(codes)} passed of {len(codes)}, limit {LIM['signin']})", allowed(codes) == LIM["signin"], codes)
    check("18 ... and it is ambit-auth that answers 429 (it sends Retry-After), not the Gateway", GW_ANSWER.get("signin") == "ambit-auth", GW_ANSWER)
    check("18 ... the discovery bucket of the same address is its own", send(addr, "discovery") == 200)

elif MODE == "nat":
    # One company address, many people. A platform registers once (one client for all of its users), then each person signs in. All from one address.
    # The people sign in at the same time (WORKERS at once): that is what an announcement does, and a sign-in takes seconds on a small cluster.
    import threading
    WORKERS = 4
    COUNT = {"signin": 0, "token": 0, "discovery": 0}
    lock = threading.Lock()
    orig = Browser.req

    def counted(self, method, url, data=None, headers=None):
        p = urllib.parse.urlparse(url)
        if p.hostname == AUTH:
            with lock:
                if p.path in ("/oauth2/auth", "/login", "/callback", "/consent", "/error"): COUNT["signin"] += 1
                elif p.path in ("/oauth2/token", "/oauth2/revoke"): COUNT["token"] += 1
                elif p.path.startswith("/.well-known/"): COUNT["discovery"] += 1
        return orig(self, method, url, data, headers)
    Browser.req = counted

    def one_signin(cid, who):
        kind, res = sign_in(Browser(), cid, f"person{who}", tick=("time",))
        if kind != "code":
            return ("page", res[0], res[2], res[1][:160]) if kind == "page" else (kind, res)
        st, tok = exchange(cid, res)
        return None if st == 200 else ("token", st)

    # 1. what one sign-in costs: alone, from an address of its own
    CLIENT_ADDR = "203.0.113.99"
    cid0 = register(REDIRECT, "NAT cost")[2].get("client_id")
    for k in COUNT: COUNT[k] = 0
    bad = one_signin(cid0, 190)
    cost = dict(COUNT)
    check(f"18 NAT: one whole sign-in alone costs {cost['signin']} requests in the sign-in bucket and {cost['token']} in the token bucket", bad is None and cost["signin"] >= 6, (bad, cost))
    typical = max(cost["signin"], 1)
    # 2. many at once from one address, until the limit answers 429
    result = None
    for attempt in range(3):
        addr = f"203.0.113.{100 + attempt}"
        start_of_window("minute")
        CLIENT_ADDR = addr
        t0, w0 = time.time(), minute_now()
        cid = register(REDIRECT, "NAT test")[2].get("client_id")
        state = {"ok": 0, "next": 0, "fail": None}

        def worker():
            while state["fail"] is None and time.time() - t0 < 58:
                with lock:
                    who = 100 + state["next"]; state["next"] += 1
                bad = one_signin(cid, who % 190)
                with lock:
                    if bad is None: state["ok"] += 1
                    elif state["fail"] is None: state["fail"] = bad
        ts = [threading.Thread(target=worker) for _ in range(WORKERS)]
        [t.start() for t in ts]; [t.join() for t in ts]
        elapsed = time.time() - t0
        if minute_now() == w0 and state["fail"] and state["fail"][:2] == ("page", 429):
            result = (state["ok"], elapsed, addr); break
        print(f"   attempt {attempt + 1} not valid (window crossed or no 429: {state['fail']}, {state['ok']} sign-ins in {elapsed:.0f} s)", flush=True)
    check("18 NAT: one address reaches the sign-in limit within one minute and is then answered 429", result is not None)
    ok, elapsed, addr = result
    expect = LIM["signin"] // typical
    print(f"   NAT: one shared address made {ok} whole sign-ins in {elapsed:.0f} s ({WORKERS} at a time) before the sign-in limit ({LIM['signin']} a minute) answered 429; "
          f"a sign-in costs {typical} requests in the sign-in bucket and {cost['token']} in the token bucket, so the limit is {expect} sign-ins", flush=True)
    check(f"18 NAT: {ok} sign-ins from one address in a minute fit the default sign-in limit of {LIM['signin']} ({typical} requests each; the {WORKERS} that were under way when it answered 429 did not finish)",
          expect - WORKERS <= ok <= expect, (ok, expect))
    other = "203.0.113.150"
    CLIENT_ADDR = other
    kind, res = sign_in(Browser(), cid, "person199", tick=("time",))
    check("18 NAT: another address signs in while the first one is limited", kind == "code", (kind, res if kind != "code" else ""))

else:
    sys.exit("unknown mode " + MODE)

print()
print("FAILED: " + ", ".join(fails) if fails else "all checks passed")
sys.exit(1 if fails else 0)
