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
        check(f"18 {b} bucket: {n} requests from one address, {a} passed and the rest got 429 (limit {LIM[b]})", a == LIM[b] and codes[LIM[b]:] == [429] * 8, codes)
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
    A = "203.0.113.11"  # the sign-in bucket of A is used up above
    spoofed = [send(A, "signin", extra={**SPOOF, "X-Forwarded-For": f"198.51.100.{i}"}) for i in range(6)]
    check("18 spoofed X-Forwarded-For, X-Real-IP, Forwarded and X-Ambit-Client-Address do not free a limited address", set(spoofed) == {429}, spoofed)
    B = "203.0.113.50"
    claims = burst(lambda i: send(B, "signin", extra={"X-Forwarded-For": A, "X-Real-IP": A, "Forwarded": f"for={A}", "X-Ambit-Client-Address": A}), LIM["signin"] + 6)
    check(f"18 another address that writes the limited address into those headers is counted as itself: {allowed(claims)} passed (limit {LIM['signin']}), not 0", allowed(claims) == LIM["signin"], claims)
    C = "203.0.113.51"
    fresh = burst(lambda i: send(C, "signin", extra={"X-Forwarded-For": f"198.51.100.{i % 250}", "X-Ambit-Client-Address": f"198.51.100.{i % 250}"}), LIM["signin"] + 6)
    check(f"18 a new made-up address in a header on every request gives no extra budget: {allowed(fresh)} passed (limit {LIM['signin']})", allowed(fresh) == LIM["signin"], fresh)

    # two Gateway pods count together
    D = "203.0.113.52"
    two = burst(lambda i: send(D, "signin", port=PORTS[i % 2]), LIM["signin"] + 12)
    check(f"18 two Gateway pods count together: {len(two)} requests alternating between them, {allowed(two)} passed (limit {LIM['signin']}, not {2 * LIM['signin']})", allowed(two) == LIM["signin"], two)
    both = [send(D, "signin", port=p) for p in PORTS]
    check("18 ... and the address is limited on both pods", both == [429, 429], both)
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
    COUNT = {"signin": 0, "token": 0, "discovery": 0}
    orig = Browser.req

    def counted(self, method, url, data=None, headers=None):
        p = urllib.parse.urlparse(url)
        if p.hostname == AUTH:
            if p.path in ("/oauth2/auth", "/login", "/callback", "/consent", "/error"): COUNT["signin"] += 1
            elif p.path in ("/oauth2/token", "/oauth2/revoke"): COUNT["token"] += 1
            elif p.path.startswith("/.well-known/"): COUNT["discovery"] += 1
        return orig(self, method, url, data, headers)
    Browser.req = counted
    result = None
    for attempt in range(3):
        addr = f"203.0.113.{100 + attempt}"
        start_of_window("minute")
        CLIENT_ADDR = addr
        t0, w0 = time.time(), minute_now()
        st, _, reg = register(REDIRECT, "NAT test")
        cid = reg.get("client_id")
        for k in COUNT: COUNT[k] = 0
        ok, fail_kind, per = 0, None, []
        while time.time() - t0 < 58:
            before = COUNT["signin"]
            kind, res = sign_in(Browser(), cid, f"person{100 + ok}", tick=("time",))
            if kind != "code":
                fail_kind = (kind, res[0] if kind == "page" else res); break
            st, tok = exchange(cid, res)
            if st != 200:
                fail_kind = ("token", st); break
            ok += 1; per.append(COUNT["signin"] - before)
        elapsed = time.time() - t0
        if minute_now() == w0 and fail_kind and fail_kind[0] == "page" and fail_kind[1] == 429:
            result = (ok, per, elapsed, addr); break
        print(f"   attempt {attempt + 1} not valid (window crossed or no 429: {fail_kind}, {ok} sign-ins in {elapsed:.0f} s)", flush=True)
    check("18 NAT: one address reaches the sign-in limit within one minute and is then answered 429", result is not None)
    ok, per, elapsed, addr = result
    typical = sorted(per)[len(per) // 2]
    print(f"   NAT: one shared address made {ok} whole sign-ins in {elapsed:.0f} s before the sign-in limit ({LIM['signin']} a minute) answered 429; "
          f"a sign-in costs {typical} requests in the sign-in bucket and one code exchange in the token bucket", flush=True)
    check(f"18 NAT: {ok} sign-ins from one address in a minute fit the default sign-in limit of {LIM['signin']} ({typical} requests each)", typical >= 6 and ok in (LIM["signin"] // typical, LIM["signin"] // typical - 1), (ok, per))
    other = "203.0.113.150"
    CLIENT_ADDR = other
    kind, res = sign_in(Browser(), cid, "bob", tick=("time",))
    check("18 NAT: another address signs in while the first one is limited", kind == "code", (kind, res if kind != "code" else ""))
else:
    sys.exit("unknown mode " + MODE)

print()
print("FAILED: " + ", ".join(fails) if fails else "all checks passed")
sys.exit(1 if fails else 0)
